package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/command"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/service"
	"github.com/idan/secretmediabot/internal/telegram"
)

type fakeGuestUseCases struct {
	create      func(context.Context, service.CreateGuestRequestParams) (service.GuestSession, error)
	mark        func(context.Context, string, string) error
	begin       func(context.Context, string, domain.User) (service.GuestSession, error)
	reserve     func(context.Context, string, domain.User) (service.GuestDelivery, error)
	complete    func(context.Context, service.GuestDelivery, int64) error
	created     service.CreateGuestRequestParams
	markedParam string
	completedID int64
}

func (f *fakeGuestUseCases) CreateGuestRequest(ctx context.Context, params service.CreateGuestRequestParams) (service.GuestSession, error) {
	f.created = params
	if f.create != nil {
		return f.create(ctx, params)
	}
	return service.GuestSession{}, nil
}

func (f *fakeGuestUseCases) CreateGuestInlineSecret(ctx context.Context, params service.CreateGuestInlineParams) (service.GuestSession, error) {
	return service.GuestSession{
		Request: repository.GuestRequest{
			ID:          uuid.New(),
			State:       repository.GuestStateReady,
			PayloadKind: domain.PayloadText,
		},
		Parameter: "guest_inline_secret_token",
	}, nil
}

func (f *fakeGuestUseCases) MarkGuestEnvelope(ctx context.Context, parameter, inlineID string) error {
	f.markedParam = parameter + ":" + inlineID
	if f.mark != nil {
		return f.mark(ctx, parameter, inlineID)
	}
	return nil
}

func (f *fakeGuestUseCases) CancelGuestRequest(context.Context, int64) (int, error) {
	return 0, service.ErrGuestNotFound
}

func (f *fakeGuestUseCases) BeginGuestSession(ctx context.Context, parameter string, user domain.User) (service.GuestSession, error) {
	if f.begin != nil {
		return f.begin(ctx, parameter, user)
	}
	return service.GuestSession{}, nil
}

func (f *fakeGuestUseCases) ClaimGuestIngestForSender(context.Context, int64) (service.GuestIngestClaim, error) {
	return service.GuestIngestClaim{}, service.ErrGuestNotFound
}

func (f *fakeGuestUseCases) ReleaseGuestIngest(context.Context, service.GuestIngestClaim) error {
	return nil
}

func (f *fakeGuestUseCases) FinalizeGuestText(context.Context, service.GuestIngestClaim, string) (repository.GuestRequest, error) {
	return repository.GuestRequest{}, nil
}

func (f *fakeGuestUseCases) FinalizeGuestMedia(context.Context, service.GuestIngestClaim, domain.MediaReference, []byte, string) (repository.GuestRequest, error) {
	return repository.GuestRequest{}, nil
}

func (f *fakeGuestUseCases) ReserveGuestOpen(ctx context.Context, parameter string, user domain.User) (service.GuestDelivery, error) {
	if f.reserve != nil {
		return f.reserve(ctx, parameter, user)
	}
	return service.GuestDelivery{}, nil
}

func (f *fakeGuestUseCases) CompleteGuestOpen(ctx context.Context, delivery service.GuestDelivery, messageID int64) error {
	f.completedID = messageID
	if f.complete != nil {
		return f.complete(ctx, delivery, messageID)
	}
	return nil
}

func (f *fakeGuestUseCases) FailGuestOpen(context.Context, service.GuestDelivery) error { return nil }

func (f *fakeGuestUseCases) GuestMediaFallback(context.Context, uuid.UUID) ([]byte, domain.MediaType, string, error) {
	return nil, "", "", service.ErrGuestNotFound
}

func TestParseGuestTargetSupportsIDsAndUsernames(t *testing.T) {
	tests := []struct {
		text string
		kind command.TargetKind
		id   int64
		name string
	}{
		{text: "@secret_bot 123456", kind: command.TargetUserID, id: 123456},
		{text: "@secret_bot @Target_User", kind: command.TargetUsername, name: "target_user"},
	}
	for _, tt := range tests {
		got, err := parseGuestTarget(tt.text, "secret_bot")
		if err != nil || got.Kind != tt.kind || got.UserID != tt.id || got.Username != tt.name {
			t.Fatalf("parseGuestTarget(%q) = %#v, %v", tt.text, got, err)
		}
	}
	if _, err := parseGuestTarget("@secret_bot @target secret-in-public", "secret_bot"); err == nil {
		t.Fatal("parseGuestTarget accepted public secret content")
	}
}

func TestGuestMessageCreatesOpaqueEnvelope(t *testing.T) {
	tg := &fakeTelegram{}
	guest := &fakeGuestUseCases{create: func(_ context.Context, params service.CreateGuestRequestParams) (service.GuestSession, error) {
		if params.Target.Kind != command.TargetUsername || params.Target.Username != "target_user" {
			t.Fatalf("target = %#v", params.Target)
		}
		return service.GuestSession{
			Request: repository.GuestRequest{ID: uuid.New()}, Parameter: "guest_opaque-token",
		}, nil
	}}
	h := testHandler(&fakeUseCases{}, tg)
	h.guest = guest
	err := h.HandleUpdate(context.Background(), telegram.Update{GuestMessage: &telegram.Message{
		GuestQueryID: "guest-query", From: &telegram.User{ID: 101, Username: "sender"},
		Chat: telegram.Chat{ID: -1001, Type: "supergroup"}, Text: "@secret_bot @target_user",
	}})
	if err != nil {
		t.Fatalf("HandleUpdate() error = %v", err)
	}
	if len(tg.guestAnswers) != 1 || len(tg.guestAnswers[0].Result.ReplyMarkup.InlineKeyboard) != 1 {
		t.Fatalf("guest answers = %#v", tg.guestAnswers)
	}
	result := tg.guestAnswers[0].Result
	if result.InputMessageContent.MessageText == "" || strings.Contains(result.InputMessageContent.MessageText, "secret-in-public") {
		t.Fatalf("guest envelope exposed content: %#v", result)
	}
	if guest.markedParam != "guest_opaque-token:inline-guest-1" {
		t.Fatalf("marked envelope = %q", guest.markedParam)
	}
}

func TestGuestPrivateOpenSendsAndCompletes(t *testing.T) {
	tg := &fakeTelegram{}
	guest := &fakeGuestUseCases{
		begin: func(context.Context, string, domain.User) (service.GuestSession, error) {
			return service.GuestSession{Role: service.GuestRoleTarget, Parameter: "guest_token", Request: repository.GuestRequest{State: repository.GuestStateReady}}, nil
		},
		reserve: func(context.Context, string, domain.User) (service.GuestDelivery, error) {
			return service.GuestDelivery{
				Request:    repository.GuestRequest{ID: uuid.New()},
				Content:    service.GuestPlaintextContent{Kind: domain.PayloadText, Text: []byte("private secret")},
				LeaseUntil: time.Now().Add(time.Minute),
			}, nil
		},
	}
	h := testHandler(&fakeUseCases{}, tg)
	h.guest = guest
	if err := h.HandleUpdate(context.Background(), privateUpdate(202, "/start guest_token")); err != nil {
		t.Fatalf("HandleUpdate() error = %v", err)
	}
	if len(tg.messages) < 2 || tg.messages[0].Text != "private secret" || guest.completedID != 1 {
		t.Fatalf("messages/completion = %#v/%d", tg.messages, guest.completedID)
	}
}

func TestInlineQueryDirectSecretMessage(t *testing.T) {
	tg := &fakeTelegram{}
	guest := &fakeGuestUseCases{}
	h := testHandler(&fakeUseCases{}, tg)
	h.guest = guest

	// User types @bot @target_user Here is the secret code 12345
	err := h.HandleUpdate(context.Background(), telegram.Update{
		InlineQuery: &telegram.InlineQuery{
			ID:    "query_direct_1",
			From:  telegram.User{ID: 101, Username: "sender_user"},
			Query: "@target_user Here is the secret code 12345",
		},
	})
	if err != nil {
		t.Fatalf("HandleUpdate(InlineQuery) error = %v", err)
	}

	if len(tg.inlineAnswers) != 1 {
		t.Fatalf("expected 1 inline answer, got %d", len(tg.inlineAnswers))
	}
	ans := tg.inlineAnswers[0]
	if len(ans.Results) != 1 {
		t.Fatalf("expected 1 inline article result, got %d", len(ans.Results))
	}
	article := ans.Results[0]
	if !strings.Contains(article.Title, "@target_user") {
		t.Fatalf("expected article title to mention target, got %q", article.Title)
	}
	if len(article.ReplyMarkup.InlineKeyboard) != 1 || len(article.ReplyMarkup.InlineKeyboard[0]) != 1 {
		t.Fatalf("expected 1 button in reply markup, got %#v", article.ReplyMarkup)
	}
	button := article.ReplyMarkup.InlineKeyboard[0][0]
	if button.Text != "🔓 Open Secret" {
		t.Fatalf("expected button text '🔓 Open Secret', got %q", button.Text)
	}
	if !strings.Contains(button.URL, "guest_inline_secret_token") {
		t.Fatalf("expected button URL to contain guest token, got %q", button.URL)
	}
}
