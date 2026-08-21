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
	create       func(context.Context, service.CreateGuestRequestParams) (service.GuestSession, error)
	createInline func(context.Context, service.CreateGuestInlineParams) (service.GuestSession, error)
	cancelByID   func(context.Context, uuid.UUID, int64) error
	mark         func(context.Context, string, string) error
	begin        func(context.Context, string, domain.User) (service.GuestSession, error)
	reserve      func(context.Context, string, domain.User) (service.GuestDelivery, error)
	complete     func(context.Context, service.GuestDelivery, int64) error
	fallback     func(context.Context, uuid.UUID) ([]byte, domain.MediaType, string, error)
	recent       []domain.RecentTarget
	created      service.CreateGuestRequestParams
	inlineParams []service.CreateGuestInlineParams
	markedParam  string
	completedID  int64
}

func (f *fakeGuestUseCases) CreateGuestRequest(ctx context.Context, params service.CreateGuestRequestParams) (service.GuestSession, error) {
	f.created = params
	if f.create != nil {
		return f.create(ctx, params)
	}
	return service.GuestSession{}, nil
}

func (f *fakeGuestUseCases) CreateGuestInlineSecret(ctx context.Context, params service.CreateGuestInlineParams) (service.GuestSession, error) {
	f.inlineParams = append(f.inlineParams, params)
	if f.createInline != nil {
		return f.createInline(ctx, params)
	}
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

func (f *fakeGuestUseCases) CancelGuestRequestByID(ctx context.Context, requestID uuid.UUID, senderID int64) error {
	if f.cancelByID != nil {
		return f.cancelByID(ctx, requestID, senderID)
	}
	return nil
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

func (f *fakeGuestUseCases) GuestMediaFallback(ctx context.Context, requestID uuid.UUID) ([]byte, domain.MediaType, string, error) {
	if f.fallback != nil {
		return f.fallback(ctx, requestID)
	}
	return nil, "", "", service.ErrGuestNotFound
}

func (f *fakeGuestUseCases) GetRecentTargets(_ context.Context, _ int64, limit int) ([]domain.RecentTarget, error) {
	if limit > 0 && len(f.recent) > limit {
		return append([]domain.RecentTarget(nil), f.recent[:limit]...), nil
	}
	return append([]domain.RecentTarget(nil), f.recent...), nil
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
	h := testHandler(&fakeUseCases{ephemeralDeleteAfter: time.Minute}, tg)
	h.guest = guest
	if err := h.HandleUpdate(context.Background(), privateUpdate(202, "/start guest_token")); err != nil {
		t.Fatalf("HandleUpdate() error = %v", err)
	}
	if len(tg.messages) < 2 || tg.messages[0].Text != "private secret" || guest.completedID != 1 {
		t.Fatalf("messages/completion = %#v/%d", tg.messages, guest.completedID)
	}
	if !strings.Contains(tg.messages[1].Text, "1m") {
		t.Fatalf("delivery acknowledgement = %q, want configured deletion delay", tg.messages[1].Text)
	}
}

func TestGuestPrivateMediaDeliveryFallsBackWhenFileIDIsRejected(t *testing.T) {
	requestID := uuid.New()
	fallbackBytes := []byte("guest media plaintext")
	wantUploaded := string(fallbackBytes)
	var uploaded string
	guest := &fakeGuestUseCases{
		begin: func(context.Context, string, domain.User) (service.GuestSession, error) {
			return service.GuestSession{
				Role: service.GuestRoleTarget, Parameter: "guest_token",
				Request: repository.GuestRequest{State: repository.GuestStateReady},
			}, nil
		},
		reserve: func(context.Context, string, domain.User) (service.GuestDelivery, error) {
			return service.GuestDelivery{
				Request: repository.GuestRequest{ID: requestID},
				Content: service.GuestPlaintextContent{
					Kind: domain.PayloadMedia,
					Media: &repository.DeliveryMedia{
						Type: domain.MediaPhoto, TelegramFileID: "dead-file-id",
						ContentType: "image/jpeg", PlaintextSize: int64(len(fallbackBytes)),
					},
				},
				LeaseUntil: time.Now().Add(time.Minute),
			}, nil
		},
		fallback: func(_ context.Context, gotID uuid.UUID) ([]byte, domain.MediaType, string, error) {
			if gotID != requestID {
				t.Fatalf("fallback request ID = %s, want %s", gotID, requestID)
			}
			return fallbackBytes, domain.MediaPhoto, "image/jpeg", nil
		},
	}
	tg := &fakeTelegram{
		sendPrivateByID: func(context.Context, telegram.SendPrivateMediaByFileIDRequest) (telegram.Message, error) {
			return telegram.Message{}, &telegram.APIError{ErrorCode: 400}
		},
		sendPrivateMedia: func(_ context.Context, request telegram.SendPrivateMediaRequest) (telegram.Message, error) {
			uploaded = string(request.Data)
			return telegram.Message{MessageID: 77, Chat: telegram.Chat{ID: 202}}, nil
		},
	}
	h := testHandler(&fakeUseCases{}, tg)
	h.guest = guest
	if err := h.HandleUpdate(context.Background(), privateUpdate(202, "/start guest_token")); err != nil {
		t.Fatalf("HandleUpdate() error = %v", err)
	}
	if len(tg.privateByID) != 1 || len(tg.private) != 1 {
		t.Fatalf("private delivery attempts = %d/%d, want file ID plus fallback upload", len(tg.privateByID), len(tg.private))
	}
	if uploaded != wantUploaded {
		t.Fatalf("fallback upload = %q, want %q", uploaded, wantUploaded)
	}
	if guest.completedID != 77 {
		t.Fatalf("completed message ID = %d, want 77", guest.completedID)
	}
}

func TestRecentTargetPrefersStableIDAfterUsernameClaim(t *testing.T) {
	recent := domain.RecentTarget{
		TargetUserID: 202, TargetUsername: "old_username", DisplayName: "Bob",
	}
	if got := recent.TargetIdentifier(); got != "202" {
		t.Fatalf("TargetIdentifier() = %q, want stable numeric ID", got)
	}
	target := targetFromRecent(recent)
	if target.Kind != command.TargetUserID || target.UserID != 202 {
		t.Fatalf("targetFromRecent() = %#v, want numeric target 202", target)
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
	if len(ans.Results) < 1 {
		t.Fatalf("expected at least 1 inline article result, got %d", len(ans.Results))
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

func TestInlineQueryRecentTargets(t *testing.T) {
	tg := &fakeTelegram{}
	guest := &fakeGuestUseCases{}
	h := testHandler(&fakeUseCases{}, tg)
	h.guest = guest

	// User queries @bot [empty query] -> should list recent targets
	guest.create = func(ctx context.Context, params service.CreateGuestRequestParams) (service.GuestSession, error) {
		return service.GuestSession{Parameter: "guest_media_token"}, nil
	}

	err := h.HandleUpdate(context.Background(), telegram.Update{
		InlineQuery: &telegram.InlineQuery{
			ID:    "query_empty",
			From:  telegram.User{ID: 101, Username: "sender_user"},
			Query: "",
		},
	})
	if err != nil {
		t.Fatalf("HandleUpdate(empty inline query) error = %v", err)
	}
	if len(tg.inlineAnswers) != 1 {
		t.Fatalf("expected 1 inline answer, got %d", len(tg.inlineAnswers))
	}
}

func TestInlineQueryRecentTargetsUsesUniqueRequestAndResultIdentities(t *testing.T) {
	tg := &fakeTelegram{}
	guest := &fakeGuestUseCases{recent: []domain.RecentTarget{
		{TargetUserID: 202, DisplayName: "Bob"},
		{TargetUsername: "other_target", DisplayName: "@other_target"},
	}}
	h := testHandler(&fakeUseCases{}, tg)
	h.guest = guest

	err := h.HandleUpdate(context.Background(), telegram.Update{
		InlineQuery: &telegram.InlineQuery{
			ID:    "query_recent_unique",
			From:  telegram.User{ID: 101, Username: "sender_user"},
			Query: "secret!",
		},
	})
	if err != nil {
		t.Fatalf("HandleUpdate(recent inline query) error = %v", err)
	}
	if len(tg.inlineAnswers) != 1 || len(tg.inlineAnswers[0].Results) != 2 {
		t.Fatalf("recent inline answer = %#v, want two results", tg.inlineAnswers)
	}
	if len(guest.inlineParams) != 2 || guest.inlineParams[0].InlineQueryID == guest.inlineParams[1].InlineQueryID {
		t.Fatalf("recent inline query IDs = %#v, want unique IDs", guest.inlineParams)
	}
	if tg.inlineAnswers[0].Results[0].ID == tg.inlineAnswers[0].Results[1].ID {
		t.Fatalf("recent result IDs collided: %#v", tg.inlineAnswers[0].Results)
	}
}

func TestInlineQuerySeparatesInstantTextAndMediaResultIDs(t *testing.T) {
	tg := &fakeTelegram{}
	guest := &fakeGuestUseCases{}
	h := testHandler(&fakeUseCases{}, tg)
	h.guest = guest

	err := h.HandleUpdate(context.Background(), telegram.Update{
		InlineQuery: &telegram.InlineQuery{
			ID:    "query_media_literal",
			From:  telegram.User{ID: 101, Username: "sender_user"},
			Query: "@target_user media",
		},
	})
	if err != nil {
		t.Fatalf("HandleUpdate(media literal inline query) error = %v", err)
	}
	if len(tg.inlineAnswers) != 1 || len(tg.inlineAnswers[0].Results) != 2 {
		t.Fatalf("inline answer = %#v, want instant and media results", tg.inlineAnswers)
	}
	if tg.inlineAnswers[0].Results[0].ID == tg.inlineAnswers[0].Results[1].ID {
		t.Fatalf("instant/media result IDs collided: %#v", tg.inlineAnswers[0].Results)
	}
}

func TestRecentInlineQueryIDNamespacesTargetKinds(t *testing.T) {
	t.Parallel()

	queryID := "inline-query"
	userTarget := command.Target{Kind: command.TargetUserID, UserID: 202}
	usernameTarget := command.Target{Kind: command.TargetUsername, Username: "user-id-202"}
	if got, want := recentInlineQueryID(queryID, userTarget), "inline-query-recent-user-id-202"; got != want {
		t.Fatalf("recentInlineQueryID(user ID) = %q, want %q", got, want)
	}
	if got, want := recentInlineQueryID(queryID, usernameTarget), "inline-query-recent-username-user-id-202"; got != want {
		t.Fatalf("recentInlineQueryID(username) = %q, want %q", got, want)
	}
	if recentInlineQueryID(queryID, userTarget) == recentInlineQueryID(queryID, usernameTarget) {
		t.Fatal("recent inline query ID namespaces collided across target kinds")
	}
}

func TestInlineQueryPermanentAnswerFailureCancelsReadyPreview(t *testing.T) {
	tg := &fakeTelegram{
		answerInline: func(context.Context, telegram.AnswerInlineQueryRequest) error {
			return &telegram.APIError{ErrorCode: 400, Description: "query is too old"}
		},
	}
	var cancelledID uuid.UUID
	var cancelledSender int64
	guest := &fakeGuestUseCases{
		cancelByID: func(_ context.Context, requestID uuid.UUID, senderID int64) error {
			cancelledID = requestID
			cancelledSender = senderID
			return nil
		},
	}
	h := testHandler(&fakeUseCases{}, tg)
	h.guest = guest

	err := h.HandleUpdate(context.Background(), telegram.Update{
		InlineQuery: &telegram.InlineQuery{
			ID:    "query_cancel_preview",
			From:  telegram.User{ID: 101, Username: "sender_user"},
			Query: "@target_user private preview",
		},
	})
	if err == nil || !telegram.IsPermanent(err) {
		t.Fatalf("HandleUpdate() error = %v, want permanent Telegram error", err)
	}
	if cancelledID == uuid.Nil || cancelledSender != 101 {
		t.Fatalf("cancelled preview = %s/%d, want generated request and sender 101", cancelledID, cancelledSender)
	}
}

func TestInlineQueryTransientAnswerFailureKeepsReadyPreview(t *testing.T) {
	tg := &fakeTelegram{
		answerInline: func(context.Context, telegram.AnswerInlineQueryRequest) error {
			return &telegram.APIError{ErrorCode: 500, Description: "temporary outage"}
		},
	}
	cancelCalls := 0
	guest := &fakeGuestUseCases{
		cancelByID: func(context.Context, uuid.UUID, int64) error {
			cancelCalls++
			return nil
		},
	}
	h := testHandler(&fakeUseCases{}, tg)
	h.guest = guest

	err := h.HandleUpdate(context.Background(), telegram.Update{
		InlineQuery: &telegram.InlineQuery{
			ID:    "query_keep_preview",
			From:  telegram.User{ID: 101, Username: "sender_user"},
			Query: "@target_user retryable preview",
		},
	})
	if err == nil || telegram.IsPermanent(err) {
		t.Fatalf("HandleUpdate() error = %v, want transient Telegram error", err)
	}
	if cancelCalls != 0 {
		t.Fatalf("preview cancellation calls = %d, want 0 for transient failure", cancelCalls)
	}
}

func TestParseInlineQueryVariantsAndQuotes(t *testing.T) {
	tests := []struct {
		input      string
		targetKind command.TargetKind
		targetID   int64
		targetUser string
		secretText string
	}{
		{
			input:      `@target_user "hello world"`,
			targetKind: command.TargetUsername,
			targetUser: "target_user",
			secretText: "hello world",
		},
		{
			input:      `@target_user 'single quotes'`,
			targetKind: command.TargetUsername,
			targetUser: "target_user",
			secretText: "single quotes",
		},
		{
			input:      `@target_user “smart quotes”`,
			targetKind: command.TargetUsername,
			targetUser: "target_user",
			secretText: "smart quotes",
		},
		{
			input:      `@target_user no quotes here`,
			targetKind: command.TargetUsername,
			targetUser: "target_user",
			secretText: "no quotes here",
		},
		{
			input:      `123456789 "numeric target"`,
			targetKind: command.TargetUserID,
			targetID:   123456789,
			secretText: "numeric target",
		},
		{
			input:      `[123456789] "bracket target"`,
			targetKind: command.TargetUserID,
			targetID:   123456789,
			secretText: "bracket target",
		},
		{
			input:      `id:123456789 "id prefix target"`,
			targetKind: command.TargetUserID,
			targetID:   123456789,
			secretText: "id prefix target",
		},
		{
			input:      `target_user "no at-symbol target"`,
			targetKind: command.TargetUsername,
			targetUser: "target_user",
			secretText: "no at-symbol target",
		},
		{
			input:      `@target_user`,
			targetKind: command.TargetUsername,
			targetUser: "target_user",
			secretText: "",
		},
	}

	for _, tt := range tests {
		target, secret, err := parseInlineQuery(tt.input)
		if err != nil {
			t.Fatalf("parseInlineQuery(%q) error = %v", tt.input, err)
		}
		if target.Kind != tt.targetKind {
			t.Errorf("parseInlineQuery(%q) target.Kind = %v, want %v", tt.input, target.Kind, tt.targetKind)
		}
		if tt.targetKind == command.TargetUserID && target.UserID != tt.targetID {
			t.Errorf("parseInlineQuery(%q) target.UserID = %d, want %d", tt.input, target.UserID, tt.targetID)
		}
		if tt.targetKind == command.TargetUsername && target.Username != tt.targetUser {
			t.Errorf("parseInlineQuery(%q) target.Username = %q, want %q", tt.input, target.Username, tt.targetUser)
		}
		if secret != tt.secretText {
			t.Errorf("parseInlineQuery(%q) secret = %q, want %q", tt.input, secret, tt.secretText)
		}
	}
}
