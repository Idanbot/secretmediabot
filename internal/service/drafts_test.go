package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/idan/secretmediabot/internal/command"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/token"
)

func TestCreateDraftResolvesUsernameOnlyWithinSourceChat(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	chatA := domain.Chat{TelegramChatID: -1001, Type: domain.ChatTypeSupergroup, Title: "A"}
	chatB := domain.Chat{TelegramChatID: -1002, Type: domain.ChatTypeGroup, Title: "B"}
	sender := domain.User{TelegramUserID: 101, Username: "sender"}
	inChat := domain.User{TelegramUserID: 102, Username: "SharedName"}
	otherChat := domain.User{TelegramUserID: 103, Username: "SharedName"}
	store.addMember(chatA.TelegramChatID, inChat)
	store.addMember(chatB.TelegramChatID, otherChat)
	service, _ := newTestService(t, store, validServiceOptions())
	target := command.Target{Kind: command.TargetUsername, Username: "sharedname"}

	result, err := service.CreateDraft(context.Background(), CreateDraftRequest{
		Sender: sender, Chat: chatA, DirectTarget: &target,
	})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	if result.Recipient.TelegramUserID != inChat.TelegramUserID {
		t.Fatalf("recipient ID = %d, want same-chat user %d", result.Recipient.TelegramUserID, inChat.TelegramUserID)
	}
	if result.Draft.SourceChatID != chatA.TelegramChatID || result.Draft.SenderID != sender.TelegramUserID ||
		result.Draft.RecipientID != inChat.TelegramUserID {
		t.Fatalf("created draft identity = %#v", result.Draft)
	}
	if result.ComposeParameter == "" {
		t.Fatal("CreateDraft() returned an empty compose parameter")
	}
	if _, err := validateComposeParameter(result.ComposeParameter); err != nil {
		t.Fatalf("compose parameter is not strict/parseable: %v", err)
	}
}

func TestCreateDraftRejectsCrossChatAndUnauthorizedTargets(t *testing.T) {
	t.Parallel()

	chat := domain.Chat{TelegramChatID: -2001, Type: domain.ChatTypeSupergroup, Title: "allowed"}
	otherChat := domain.Chat{TelegramChatID: -2002, Type: domain.ChatTypeSupergroup, Title: "other"}
	sender := domain.User{TelegramUserID: 201, Username: "sender"}
	targetUser := domain.User{TelegramUserID: 202, Username: "target"}

	tests := []struct {
		name    string
		prepare func(*memoryStore, *Options) CreateDraftRequest
		want    error
	}{
		{
			name: "numeric target observed in another chat",
			prepare: func(store *memoryStore, _ *Options) CreateDraftRequest {
				store.addMember(otherChat.TelegramChatID, targetUser)
				target := command.Target{Kind: command.TargetUserID, UserID: targetUser.TelegramUserID}
				return CreateDraftRequest{Sender: sender, Chat: chat, DirectTarget: &target}
			},
			want: ErrTargetNotObserved,
		},
		{
			name: "bot target",
			prepare: func(_ *memoryStore, _ *Options) CreateDraftRequest {
				bot := domain.User{TelegramUserID: 203, Username: "other_bot", IsBot: true}
				return CreateDraftRequest{Sender: sender, Chat: chat, ReplyRecipient: &bot}
			},
			want: ErrTargetIsBot,
		},
		{
			name: "sender target",
			prepare: func(_ *memoryStore, _ *Options) CreateDraftRequest {
				self := sender
				return CreateDraftRequest{Sender: sender, Chat: chat, ReplyRecipient: &self}
			},
			want: ErrTargetIsSender,
		},
		{
			name: "disallowed chat",
			prepare: func(_ *memoryStore, options *Options) CreateDraftRequest {
				options.AllowedChatIDs = []int64{otherChat.TelegramChatID}
				recipient := targetUser
				return CreateDraftRequest{Sender: sender, Chat: chat, ReplyRecipient: &recipient}
			},
			want: ErrChatNotAllowed,
		},
		{
			name: "private source chat",
			prepare: func(_ *memoryStore, _ *Options) CreateDraftRequest {
				privateChat := domain.Chat{TelegramChatID: sender.TelegramUserID, Type: domain.ChatTypePrivate}
				recipient := targetUser
				return CreateDraftRequest{Sender: sender, Chat: privateChat, ReplyRecipient: &recipient}
			},
			want: ErrTargetRequired,
		},
		{
			name: "reply and direct target together",
			prepare: func(_ *memoryStore, _ *Options) CreateDraftRequest {
				recipient := targetUser
				direct := command.Target{Kind: command.TargetUserID, UserID: targetUser.TelegramUserID}
				return CreateDraftRequest{Sender: sender, Chat: chat, ReplyRecipient: &recipient, DirectTarget: &direct}
			},
			want: ErrTargetRequired,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := newMemoryStore()
			options := validServiceOptions()
			request := test.prepare(store, &options)
			service, _ := newTestService(t, store, options)
			_, err := service.CreateDraft(context.Background(), request)
			if !errors.Is(err, test.want) {
				t.Fatalf("CreateDraft() error = %v, want %v", err, test.want)
			}
			if store.createdCount() != 0 {
				t.Fatal("rejected target unexpectedly persisted a draft")
			}
		})
	}
}

func TestResumeDraftDoesNotRevealAnotherSendersDraft(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	generated, err := token.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	draft, err := domain.NewDraft(domain.NewDraftParams{
		ComposeTokenHash: generated.Hash[:],
		SenderID:         301,
		RecipientID:      302,
		SourceChatID:     -3001,
		CreatedAt:        serviceTestNow,
		ExpiresAt:        serviceTestNow.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("NewDraft() error = %v", err)
	}
	store.drafts[string(generated.Hash[:])] = draft
	store.addMember(draft.SourceChatID, domain.User{TelegramUserID: draft.RecipientID, Username: "recipient"})
	service, _ := newTestService(t, store, validServiceOptions())

	_, err = service.ResumeDraft(context.Background(), domain.User{TelegramUserID: 399}, ComposePrefix+generated.Raw)
	if !errors.Is(err, ErrDraftNotFound) {
		t.Fatalf("ResumeDraft() wrong sender error = %v, want ErrDraftNotFound", err)
	}

	resumed, err := service.ResumeDraft(context.Background(), domain.User{TelegramUserID: draft.SenderID}, ComposePrefix+generated.Raw)
	if err != nil {
		t.Fatalf("ResumeDraft() owner error = %v", err)
	}
	if resumed.Draft.ID != draft.ID || resumed.Recipient.TelegramUserID != draft.RecipientID {
		t.Fatalf("resumed draft = %#v", resumed)
	}
}

func TestCreateDraftMapsPreflightAndTransactionalLimits(t *testing.T) {
	t.Parallel()

	chat := domain.Chat{TelegramChatID: -4001, Type: domain.ChatTypeSupergroup}
	sender := domain.User{TelegramUserID: 401, Username: "sender"}
	recipient := domain.User{TelegramUserID: 402, Username: "recipient"}
	tests := []struct {
		name       string
		configure  func(*memoryStore)
		want       error
		wantCreate bool
	}{
		{name: "active preflight", configure: func(s *memoryStore) { s.activeDrafts = 1 }, want: ErrTooManyDrafts},
		{name: "rate preflight", configure: func(s *memoryStore) { s.recentCount = 30 }, want: ErrRateLimited},
		{name: "active transaction race", configure: func(s *memoryStore) { s.createErr = repository.ErrTooManyActiveDrafts }, want: ErrTooManyDrafts, wantCreate: true},
		{name: "rate transaction race", configure: func(s *memoryStore) { s.createErr = repository.ErrWhisperRateLimit }, want: ErrRateLimited, wantCreate: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := newMemoryStore()
			store.addMember(chat.TelegramChatID, recipient)
			test.configure(store)
			service, _ := newTestService(t, store, validServiceOptions())
			target := command.Target{Kind: command.TargetUserID, UserID: recipient.TelegramUserID}
			_, err := service.CreateDraft(context.Background(), CreateDraftRequest{
				Sender: sender, Chat: chat, DirectTarget: &target,
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("CreateDraft() error = %v, want %v", err, test.want)
			}
			if got := store.createdCount(); (got > 0) != test.wantCreate {
				t.Fatalf("CreateDraft repository calls = %d, wantCreate = %t", got, test.wantCreate)
			}
		})
	}
}
