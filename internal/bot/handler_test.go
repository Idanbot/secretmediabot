package bot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
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

type fakeUseCases struct {
	observeMembership func(context.Context, domain.User, domain.Chat) error
	registerPrivate   func(context.Context, domain.User) (domain.User, error)
	createDraft       func(context.Context, service.CreateDraftRequest) (service.CreateDraftResult, error)
	resumeDraft       func(context.Context, domain.User, string) (service.ResumeDraftResult, error)
	cancelDraft       func(context.Context, int64) (domain.Draft, error)
	claimIngest       func(context.Context, int64) (domain.Draft, error)
	releaseIngest     func(context.Context, domain.Draft) error
	finalizeText      func(context.Context, domain.Draft, string) (service.CreatedWhisper, error)
	finalizeMedia     func(context.Context, domain.Draft, domain.MediaReference, []byte, string) (service.CreatedWhisper, error)
	claimPublication  func(context.Context, uuid.UUID) (service.Publication, error)
	claimNext         func(context.Context) (service.Publication, error)
	completePublish   func(context.Context, service.Publication, int64) error
	failPublish       func(context.Context, service.Publication, string, time.Duration) error
	reserveOpen       func(context.Context, string, int64, string) (service.OpenDelivery, error)
	completeOpen      func(context.Context, service.OpenDelivery, int64) error
	failOpen          func(context.Context, service.OpenDelivery, string) error
	isOwner           func(int64) bool
	ownerList         func(context.Context, int64, int, int) ([]domain.Whisper, error)
	ownerReview       func(context.Context, int64, uuid.UUID) (service.OwnerReview, error)
	ownerDelete       func(context.Context, int64, uuid.UUID) error
	ownerRetention    func(context.Context, int64, uuid.UUID, time.Duration) error
}

func (f *fakeUseCases) ObserveMembership(ctx context.Context, user domain.User, chat domain.Chat) error {
	if f.observeMembership != nil {
		return f.observeMembership(ctx, user, chat)
	}
	return nil
}

func (f *fakeUseCases) RegisterPrivateUser(ctx context.Context, user domain.User) (domain.User, error) {
	if f.registerPrivate != nil {
		return f.registerPrivate(ctx, user)
	}
	return user, nil
}

func (f *fakeUseCases) CreateDraft(ctx context.Context, request service.CreateDraftRequest) (service.CreateDraftResult, error) {
	if f.createDraft != nil {
		return f.createDraft(ctx, request)
	}
	return service.CreateDraftResult{}, nil
}

func (f *fakeUseCases) ResumeDraft(ctx context.Context, user domain.User, token string) (service.ResumeDraftResult, error) {
	if f.resumeDraft != nil {
		return f.resumeDraft(ctx, user, token)
	}
	return service.ResumeDraftResult{}, nil
}

func (f *fakeUseCases) CancelLatestDraft(ctx context.Context, userID int64) (domain.Draft, error) {
	if f.cancelDraft != nil {
		return f.cancelDraft(ctx, userID)
	}
	return domain.Draft{}, nil
}

func (f *fakeUseCases) ClaimIngest(ctx context.Context, userID int64) (domain.Draft, error) {
	if f.claimIngest != nil {
		return f.claimIngest(ctx, userID)
	}
	return domain.Draft{}, nil
}

func (f *fakeUseCases) ReleaseIngest(ctx context.Context, draft domain.Draft) error {
	if f.releaseIngest != nil {
		return f.releaseIngest(ctx, draft)
	}
	return nil
}

func (f *fakeUseCases) FinalizeText(ctx context.Context, draft domain.Draft, text string) (service.CreatedWhisper, error) {
	if f.finalizeText != nil {
		return f.finalizeText(ctx, draft, text)
	}
	return service.CreatedWhisper{}, nil
}

func (f *fakeUseCases) FinalizeMedia(ctx context.Context, draft domain.Draft, media domain.MediaReference, data []byte, caption string) (service.CreatedWhisper, error) {
	if f.finalizeMedia != nil {
		return f.finalizeMedia(ctx, draft, media, data, caption)
	}
	return service.CreatedWhisper{}, nil
}

func (f *fakeUseCases) ClaimPublication(ctx context.Context, id uuid.UUID) (service.Publication, error) {
	if f.claimPublication != nil {
		return f.claimPublication(ctx, id)
	}
	return service.Publication{}, nil
}

func (f *fakeUseCases) ClaimNextPublication(ctx context.Context) (service.Publication, error) {
	if f.claimNext != nil {
		return f.claimNext(ctx)
	}
	return service.Publication{}, nil
}

func (f *fakeUseCases) CompletePublication(ctx context.Context, publication service.Publication, messageID int64) error {
	if f.completePublish != nil {
		return f.completePublish(ctx, publication, messageID)
	}
	return nil
}

func (f *fakeUseCases) FailPublication(ctx context.Context, publication service.Publication, code string, retry time.Duration) error {
	if f.failPublish != nil {
		return f.failPublish(ctx, publication, code, retry)
	}
	return nil
}

func (f *fakeUseCases) ReserveOpen(ctx context.Context, data string, userID int64, callbackID string) (service.OpenDelivery, error) {
	if f.reserveOpen != nil {
		return f.reserveOpen(ctx, data, userID, callbackID)
	}
	return service.OpenDelivery{}, nil
}

func (f *fakeUseCases) CompleteOpen(ctx context.Context, delivery service.OpenDelivery, messageID int64) error {
	if f.completeOpen != nil {
		return f.completeOpen(ctx, delivery, messageID)
	}
	return nil
}

func (f *fakeUseCases) FailOpen(ctx context.Context, delivery service.OpenDelivery, code string) error {
	if f.failOpen != nil {
		return f.failOpen(ctx, delivery, code)
	}
	return nil
}

func (f *fakeUseCases) IsOwner(userID int64) bool {
	return f.isOwner != nil && f.isOwner(userID)
}

func (f *fakeUseCases) OwnerList(ctx context.Context, ownerID int64, limit, offset int) ([]domain.Whisper, error) {
	if f.ownerList != nil {
		return f.ownerList(ctx, ownerID, limit, offset)
	}
	return nil, nil
}

func (f *fakeUseCases) OwnerReview(ctx context.Context, ownerID int64, id uuid.UUID) (service.OwnerReview, error) {
	if f.ownerReview != nil {
		return f.ownerReview(ctx, ownerID, id)
	}
	return service.OwnerReview{}, nil
}

func (f *fakeUseCases) OwnerDelete(ctx context.Context, ownerID int64, id uuid.UUID) error {
	if f.ownerDelete != nil {
		return f.ownerDelete(ctx, ownerID, id)
	}
	return nil
}

func (f *fakeUseCases) OwnerSetRetention(ctx context.Context, ownerID int64, id uuid.UUID, retention time.Duration) error {
	if f.ownerRetention != nil {
		return f.ownerRetention(ctx, ownerID, id, retention)
	}
	return nil
}

type fakeTelegram struct {
	sendMessage        func(context.Context, telegram.SendMessageRequest) (telegram.Message, error)
	answerCallback     func(context.Context, telegram.AnswerCallbackQueryRequest) error
	answerGuest        func(context.Context, telegram.AnswerGuestQueryRequest) (telegram.SentGuestMessage, error)
	answerInline       func(context.Context, telegram.AnswerInlineQueryRequest) error
	getFile            func(context.Context, telegram.GetFileRequest) (telegram.File, error)
	downloadFile       func(context.Context, string) ([]byte, error)
	sendEphemeralText  func(context.Context, telegram.SendEphemeralTextRequest) (int64, error)
	sendEphemeralMedia func(context.Context, telegram.SendEphemeralMediaRequest) (int64, error)
	sendPrivateByID    func(context.Context, telegram.SendPrivateMediaByFileIDRequest) (telegram.Message, error)
	sendPrivateMedia   func(context.Context, telegram.SendPrivateMediaRequest) (telegram.Message, error)
	deleteMessage      func(context.Context, telegram.DeleteMessageRequest) error

	messages       []telegram.SendMessageRequest
	answers        []telegram.AnswerCallbackQueryRequest
	guestAnswers   []telegram.AnswerGuestQueryRequest
	inlineAnswers  []telegram.AnswerInlineQueryRequest
	ephemeral      []telegram.SendEphemeralTextRequest
	media          []telegram.SendEphemeralMediaRequest
	private        []telegram.SendPrivateMediaRequest
	privateByID    []telegram.SendPrivateMediaByFileIDRequest
	deletedMessage []telegram.DeleteMessageRequest
}

func (f *fakeTelegram) SendMessage(ctx context.Context, request telegram.SendMessageRequest) (telegram.Message, error) {
	f.messages = append(f.messages, request)
	if f.sendMessage != nil {
		return f.sendMessage(ctx, request)
	}
	return telegram.Message{MessageID: int64(len(f.messages)), Chat: telegram.Chat{ID: request.ChatID}}, nil
}

func (f *fakeTelegram) AnswerCallbackQuery(ctx context.Context, request telegram.AnswerCallbackQueryRequest) error {
	f.answers = append(f.answers, request)
	if f.answerCallback != nil {
		return f.answerCallback(ctx, request)
	}
	return nil
}

func (f *fakeTelegram) AnswerGuestQuery(ctx context.Context, request telegram.AnswerGuestQueryRequest) (telegram.SentGuestMessage, error) {
	f.guestAnswers = append(f.guestAnswers, request)
	if f.answerGuest != nil {
		return f.answerGuest(ctx, request)
	}
	return telegram.SentGuestMessage{InlineMessageID: "inline-guest-1"}, nil
}

func (f *fakeTelegram) AnswerInlineQuery(ctx context.Context, request telegram.AnswerInlineQueryRequest) error {
	f.inlineAnswers = append(f.inlineAnswers, request)
	if f.answerInline != nil {
		return f.answerInline(ctx, request)
	}
	return nil
}

func (f *fakeTelegram) GetFile(ctx context.Context, request telegram.GetFileRequest) (telegram.File, error) {
	if f.getFile != nil {
		return f.getFile(ctx, request)
	}
	return telegram.File{}, nil
}

func (f *fakeTelegram) DownloadFile(ctx context.Context, path string) ([]byte, error) {
	if f.downloadFile != nil {
		return f.downloadFile(ctx, path)
	}
	return nil, nil
}

func (f *fakeTelegram) SendEphemeralText(ctx context.Context, request telegram.SendEphemeralTextRequest) (int64, error) {
	f.ephemeral = append(f.ephemeral, request)
	if f.sendEphemeralText != nil {
		return f.sendEphemeralText(ctx, request)
	}
	return 901, nil
}

func (f *fakeTelegram) SendEphemeralMedia(ctx context.Context, request telegram.SendEphemeralMediaRequest) (int64, error) {
	f.media = append(f.media, request)
	if f.sendEphemeralMedia != nil {
		return f.sendEphemeralMedia(ctx, request)
	}
	return 902, nil
}

func (f *fakeTelegram) SendPrivateMediaByFileID(ctx context.Context, request telegram.SendPrivateMediaByFileIDRequest) (telegram.Message, error) {
	f.privateByID = append(f.privateByID, request)
	if f.sendPrivateByID != nil {
		return f.sendPrivateByID(ctx, request)
	}
	return telegram.Message{MessageID: 904, Chat: telegram.Chat{ID: request.ChatID}}, nil
}

func (f *fakeTelegram) SendPrivateMedia(ctx context.Context, request telegram.SendPrivateMediaRequest) (telegram.Message, error) {
	f.private = append(f.private, request)
	if f.sendPrivateMedia != nil {
		return f.sendPrivateMedia(ctx, request)
	}
	return telegram.Message{MessageID: 903, Chat: telegram.Chat{ID: request.ChatID}}, nil
}

func (f *fakeTelegram) DeleteMessage(ctx context.Context, request telegram.DeleteMessageRequest) error {
	f.deletedMessage = append(f.deletedMessage, request)
	if f.deleteMessage != nil {
		return f.deleteMessage(ctx, request)
	}
	return nil
}

func testHandler(serviceAPI useCases, telegramAPI telegramAPI) *Handler {
	return &Handler{
		service: serviceAPI, telegram: telegramAPI, botUsername: "secret_bot",
		maxMediaBytes: 20 << 20, mediaDownloadTimeout: time.Second, requestTimeout: time.Second,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func groupUpdate(text string) telegram.Update {
	return telegram.Update{UpdateID: 1, Message: &telegram.Message{
		MessageID: 41, From: &telegram.User{ID: 101, FirstName: "Alice", Username: "alice_1"},
		Chat: telegram.Chat{ID: -1001, Type: "supergroup", Title: "Group"}, Text: text,
	}}
}

func privateUpdate(userID int64, text string) telegram.Update {
	return telegram.Update{UpdateID: 2, Message: &telegram.Message{
		MessageID: 42, From: &telegram.User{ID: userID, FirstName: "Alice"},
		Chat: telegram.Chat{ID: userID, Type: "private"}, Text: text,
	}}
}

func TestGroupWhisperTargetsAndObservesSender(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		reply  *telegram.Message
		assert func(*testing.T, service.CreateDraftRequest)
	}{
		{
			name: "username", text: "/whisper @Bob_123",
			assert: func(t *testing.T, request service.CreateDraftRequest) {
				if request.DirectTarget == nil || request.DirectTarget.Kind != command.TargetUsername || request.DirectTarget.Username != "bob_123" {
					t.Fatalf("direct target = %#v", request.DirectTarget)
				}
			},
		},
		{
			name: "numeric id", text: "/whisper 202",
			assert: func(t *testing.T, request service.CreateDraftRequest) {
				if request.DirectTarget == nil || request.DirectTarget.Kind != command.TargetUserID || request.DirectTarget.UserID != 202 {
					t.Fatalf("direct target = %#v", request.DirectTarget)
				}
			},
		},
		{
			name: "reply", text: "/whisper",
			reply: &telegram.Message{MessageID: 12, From: &telegram.User{ID: 202, FirstName: "Bob"}},
			assert: func(t *testing.T, request service.CreateDraftRequest) {
				if request.ReplyRecipient == nil || request.ReplyRecipient.TelegramUserID != 202 || request.SourceReplyMessageID == nil || *request.SourceReplyMessageID != 12 {
					t.Fatalf("reply request = %#v", request)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observed := 0
			var got service.CreateDraftRequest
			useCases := &fakeUseCases{
				observeMembership: func(_ context.Context, user domain.User, chat domain.Chat) error {
					observed++
					if user.TelegramUserID != 101 || chat.TelegramChatID != -1001 {
						t.Fatalf("observed user/chat = %d/%d", user.TelegramUserID, chat.TelegramChatID)
					}
					return nil
				},
				createDraft: func(_ context.Context, request service.CreateDraftRequest) (service.CreateDraftResult, error) {
					got = request
					return service.CreateDraftResult{Recipient: domain.User{TelegramUserID: 202, FirstName: "Bob"}}, nil
				},
			}
			update := groupUpdate(test.text)
			update.Message.ReplyToMessage = test.reply
			if err := testHandler(useCases, &fakeTelegram{}).HandleUpdate(context.Background(), update); err != nil {
				t.Fatalf("HandleUpdate() error = %v", err)
			}
			if observed != 1 {
				t.Fatalf("ObserveMembership calls = %d, want 1", observed)
			}
			test.assert(t, got)
		})
	}
}

func TestGroupWhisperFallsBackToPrivateDeepLink(t *testing.T) {
	const compose = "compose_private-token"
	useCases := &fakeUseCases{createDraft: func(context.Context, service.CreateDraftRequest) (service.CreateDraftResult, error) {
		return service.CreateDraftResult{
			Recipient: domain.User{TelegramUserID: 202, FirstName: "Bob"}, ComposeParameter: compose,
		}, nil
	}}
	tg := &fakeTelegram{}
	tg.sendMessage = func(_ context.Context, request telegram.SendMessageRequest) (telegram.Message, error) {
		if request.ChatID == 101 {
			return telegram.Message{}, errors.New("private chat unavailable")
		}
		return telegram.Message{MessageID: 5, Chat: telegram.Chat{ID: request.ChatID}}, nil
	}
	handler := testHandler(useCases, tg)
	if err := handler.HandleUpdate(context.Background(), groupUpdate("/whisper 202")); err != nil {
		t.Fatalf("HandleUpdate() error = %v", err)
	}
	if len(tg.messages) != 2 {
		t.Fatalf("SendMessage calls = %d, want 2", len(tg.messages))
	}
	fallback := tg.messages[1]
	if fallback.ChatID != -1001 || fallback.ReplyMarkup == nil || len(fallback.ReplyMarkup.InlineKeyboard) != 1 {
		t.Fatalf("fallback request = %#v", fallback)
	}
	button := fallback.ReplyMarkup.InlineKeyboard[0][0]
	parsed, err := url.Parse(button.URL)
	if err != nil {
		t.Fatalf("parse deep link: %v", err)
	}
	if parsed.Host != "t.me" || parsed.Path != "/secret_bot" || parsed.Query().Get("start") != compose {
		t.Fatalf("deep link = %q", button.URL)
	}
}

func TestGroupWhisperDoesNotRetryCommittedDraftForFailedAcknowledgement(t *testing.T) {
	created := 0
	useCases := &fakeUseCases{createDraft: func(context.Context, service.CreateDraftRequest) (service.CreateDraftResult, error) {
		created++
		return service.CreateDraftResult{Recipient: domain.User{TelegramUserID: 202, FirstName: "Bob"}}, nil
	}}
	tg := &fakeTelegram{sendMessage: func(_ context.Context, request telegram.SendMessageRequest) (telegram.Message, error) {
		if request.ChatID == 101 {
			return telegram.Message{MessageID: 7, Chat: telegram.Chat{ID: 101}}, nil
		}
		return telegram.Message{}, errors.New("group acknowledgement failed")
	}}
	if err := testHandler(useCases, tg).HandleUpdate(context.Background(), groupUpdate("/whisper 202")); err != nil {
		t.Fatalf("HandleUpdate() error = %v; committed draft must not be retried", err)
	}
	if created != 1 || len(tg.messages) != 2 {
		t.Fatalf("created/messages = %d/%d", created, len(tg.messages))
	}
}

func TestGroupWhisperCancelsDraftWhenNoComposerPromptCanBeDelivered(t *testing.T) {
	wantErr := errors.New("group fallback failed")
	cancelled := 0
	useCases := &fakeUseCases{
		createDraft: func(context.Context, service.CreateDraftRequest) (service.CreateDraftResult, error) {
			return service.CreateDraftResult{
				Recipient: domain.User{TelegramUserID: 202, FirstName: "Bob"}, ComposeParameter: "compose_private-token",
			}, nil
		},
		cancelDraft: func(_ context.Context, senderID int64) (domain.Draft, error) {
			cancelled++
			if senderID != 101 {
				t.Fatalf("cancel sender ID = %d", senderID)
			}
			return domain.Draft{}, nil
		},
	}
	tg := &fakeTelegram{sendMessage: func(_ context.Context, request telegram.SendMessageRequest) (telegram.Message, error) {
		if request.ChatID == 101 {
			return telegram.Message{}, errors.New("private chat unavailable")
		}
		return telegram.Message{}, wantErr
	}}
	if err := testHandler(useCases, tg).HandleUpdate(context.Background(), groupUpdate("/whisper 202")); !errors.Is(err, wantErr) {
		t.Fatalf("HandleUpdate() error = %v, want %v", err, wantErr)
	}
	if cancelled != 1 {
		t.Fatalf("CancelLatestDraft calls = %d, want 1", cancelled)
	}
}

func TestGroupPrivacyUsesPrivacyDisclosure(t *testing.T) {
	tg := &fakeTelegram{}
	if err := testHandler(&fakeUseCases{}, tg).HandleUpdate(context.Background(), groupUpdate("/privacy")); err != nil {
		t.Fatalf("HandleUpdate() error = %v", err)
	}
	if len(tg.messages) != 1 || tg.messages[0].Text != privacyText {
		t.Fatalf("privacy response = %#v", tg.messages)
	}
}

func leasedDraft() domain.Draft {
	now := time.Now().UTC()
	lease := now.Add(time.Minute)
	return domain.Draft{ID: uuid.New(), SenderID: 101, State: domain.DraftIngestingMedia, IngestLeaseUntil: &lease}
}

func TestPrivateTextFinalizesWithoutReleasingLease(t *testing.T) {
	draft := leasedDraft()
	whisperID := uuid.New()
	releases := 0
	var secret string
	useCases := &fakeUseCases{
		claimIngest:   func(context.Context, int64) (domain.Draft, error) { return draft, nil },
		releaseIngest: func(context.Context, domain.Draft) error { releases++; return nil },
		finalizeText: func(_ context.Context, got domain.Draft, text string) (service.CreatedWhisper, error) {
			secret = text
			return service.CreatedWhisper{Whisper: domain.Whisper{ID: whisperID}}, nil
		},
		claimPublication: func(context.Context, uuid.UUID) (service.Publication, error) {
			return service.Publication{}, errors.New("publisher temporarily unavailable")
		},
	}
	tg := &fakeTelegram{}
	if err := testHandler(useCases, tg).HandleUpdate(context.Background(), privateUpdate(101, "classified text")); err != nil {
		t.Fatalf("HandleUpdate() error = %v", err)
	}
	if secret != "classified text" || releases != 0 {
		t.Fatalf("secret/release = %q/%d", secret, releases)
	}
	if got := tg.messages[len(tg.messages)-1].Text; !strings.Contains(got, "queued") {
		t.Fatalf("confirmation = %q", got)
	}
}

func TestPrivateFinalizeFailureReleasesLease(t *testing.T) {
	draft := leasedDraft()
	releases := 0
	useCases := &fakeUseCases{
		claimIngest: func(context.Context, int64) (domain.Draft, error) { return draft, nil },
		releaseIngest: func(_ context.Context, got domain.Draft) error {
			releases++
			if got.ID != draft.ID {
				t.Fatalf("released draft = %s, want %s", got.ID, draft.ID)
			}
			return nil
		},
		finalizeText: func(context.Context, domain.Draft, string) (service.CreatedWhisper, error) {
			return service.CreatedWhisper{}, service.ErrTextTooLong
		},
	}
	tg := &fakeTelegram{}
	if err := testHandler(useCases, tg).HandleUpdate(context.Background(), privateUpdate(101, "too long")); err != nil {
		t.Fatalf("HandleUpdate() error = %v", err)
	}
	if releases != 1 {
		t.Fatalf("ReleaseIngest calls = %d, want 1", releases)
	}
	if got := tg.messages[len(tg.messages)-1].Text; !strings.Contains(got, "too long") {
		t.Fatalf("user response = %q", got)
	}
}

func TestCancelDoesNotRetryCommittedMutationForFailedAcknowledgement(t *testing.T) {
	cancelled := 0
	useCases := &fakeUseCases{cancelDraft: func(_ context.Context, userID int64) (domain.Draft, error) {
		cancelled++
		if userID != 101 {
			t.Fatalf("cancel user ID = %d", userID)
		}
		return domain.Draft{ID: uuid.New(), SenderID: userID, State: domain.DraftCancelled}, nil
	}}
	tg := &fakeTelegram{sendMessage: func(context.Context, telegram.SendMessageRequest) (telegram.Message, error) {
		return telegram.Message{}, errors.New("acknowledgement failed")
	}}
	if err := testHandler(useCases, tg).HandleUpdate(context.Background(), privateUpdate(101, "/cancel")); err != nil {
		t.Fatalf("HandleUpdate() error = %v; committed cancellation must not be retried", err)
	}
	if cancelled != 1 {
		t.Fatalf("CancelLatestDraft calls = %d, want 1", cancelled)
	}
}

func TestPrivateMediaDownloadsFinalizesAndZerosBuffer(t *testing.T) {
	draft := leasedDraft()
	whisperID := uuid.New()
	downloaded := []byte("media-secret")
	var seenMedia domain.MediaReference
	var seenData []byte
	var seenCaption string
	useCases := &fakeUseCases{
		claimIngest: func(context.Context, int64) (domain.Draft, error) { return draft, nil },
		finalizeMedia: func(_ context.Context, _ domain.Draft, media domain.MediaReference, data []byte, caption string) (service.CreatedWhisper, error) {
			seenMedia, seenData, seenCaption = media, data, caption
			return service.CreatedWhisper{Whisper: domain.Whisper{ID: whisperID}}, nil
		},
		claimPublication: func(context.Context, uuid.UUID) (service.Publication, error) {
			return service.Publication{}, errors.New("queued")
		},
	}
	tg := &fakeTelegram{
		getFile: func(_ context.Context, request telegram.GetFileRequest) (telegram.File, error) {
			if request.FileID != "voice-file" {
				t.Fatalf("GetFile ID = %q", request.FileID)
			}
			return telegram.File{FileID: request.FileID, FilePath: "voice/file.ogg", FileSize: int64(len(downloaded))}, nil
		},
		downloadFile: func(_ context.Context, path string) ([]byte, error) {
			if path != "voice/file.ogg" {
				t.Fatalf("DownloadFile path = %q", path)
			}
			return downloaded, nil
		},
	}
	update := privateUpdate(101, "")
	update.Message.Voice = &telegram.Voice{FileID: "voice-file", FileUniqueID: "voice-unique", MIMEType: "audio/ogg"}
	update.Message.Caption = "encrypted caption"
	if err := testHandler(useCases, tg).HandleUpdate(context.Background(), update); err != nil {
		t.Fatalf("HandleUpdate() error = %v", err)
	}
	if seenMedia.Type != domain.MediaVoice || seenMedia.Ref != "voice-file" || seenMedia.SizeBytes != int64(len(downloaded)) {
		t.Fatalf("media = %#v", seenMedia)
	}
	if seenCaption != "encrypted caption" || string(seenData) != strings.Repeat("\x00", len(seenData)) {
		t.Fatalf("caption/zeroed data = %q/%v", seenCaption, seenData)
	}
	for index, value := range downloaded {
		if value != 0 {
			t.Fatalf("downloaded[%d] was not zeroed", index)
		}
	}
}

func TestPrivateMediaDownloadFailureReleasesLease(t *testing.T) {
	draft := leasedDraft()
	wantErr := errors.New("download failed")
	releases := 0
	useCases := &fakeUseCases{
		claimIngest:   func(context.Context, int64) (domain.Draft, error) { return draft, nil },
		releaseIngest: func(context.Context, domain.Draft) error { releases++; return nil },
	}
	tg := &fakeTelegram{
		getFile: func(context.Context, telegram.GetFileRequest) (telegram.File, error) {
			return telegram.File{FileID: "photo", FilePath: "photos/file.jpg"}, nil
		},
		downloadFile: func(context.Context, string) ([]byte, error) { return nil, wantErr },
	}
	update := privateUpdate(101, "")
	update.Message.Photo = []telegram.PhotoSize{{FileID: "photo", FileUniqueID: "unique"}}
	if err := testHandler(useCases, tg).HandleUpdate(context.Background(), update); !errors.Is(err, wantErr) {
		t.Fatalf("HandleUpdate() error = %v, want %v", err, wantErr)
	}
	if releases != 1 {
		t.Fatalf("ReleaseIngest calls = %d, want 1", releases)
	}
}

func callbackUpdate() telegram.Update {
	return telegram.Update{UpdateID: 3, CallbackQuery: &telegram.CallbackQuery{
		ID: "callback-1", From: telegram.User{ID: 202}, Data: "w:opaque",
		Message: &telegram.Message{MessageID: 77, Chat: telegram.Chat{ID: -1001, Type: "supergroup"}},
	}}
}

func TestCallbackDenialAlwaysAnswers(t *testing.T) {
	useCases := &fakeUseCases{reserveOpen: func(context.Context, string, int64, string) (service.OpenDelivery, error) {
		return service.OpenDelivery{}, service.ErrWrongRecipient
	}}
	tg := &fakeTelegram{}
	if err := testHandler(useCases, tg).HandleUpdate(context.Background(), callbackUpdate()); err != nil {
		t.Fatalf("HandleUpdate() error = %v", err)
	}
	if len(tg.answers) != 1 || tg.answers[0].CallbackQueryID != "callback-1" || !tg.answers[0].ShowAlert {
		t.Fatalf("callback answers = %#v", tg.answers)
	}
	if len(tg.ephemeral) != 0 {
		t.Fatalf("ephemeral sends = %d, want 0", len(tg.ephemeral))
	}
}

func TestCallbackTextDeliveryCompletesAndAnswers(t *testing.T) {
	plaintext := []byte("recipient secret")
	delivery := service.OpenDelivery{
		Whisper: domain.Whisper{ID: uuid.New(), SourceChatID: -1001, ProtectContent: true},
		EventID: 9, CallbackQueryID: "callback-1",
		Content: service.PlaintextContent{Kind: domain.PayloadText, Text: plaintext},
	}
	completed := 0
	useCases := &fakeUseCases{
		reserveOpen: func(_ context.Context, data string, actorID int64, callbackID string) (service.OpenDelivery, error) {
			if data != "w:opaque" || actorID != 202 || callbackID != "callback-1" {
				t.Fatalf("reserve args = %q/%d/%q", data, actorID, callbackID)
			}
			return delivery, nil
		},
		completeOpen: func(_ context.Context, got service.OpenDelivery, ephemeralID int64) error {
			completed++
			if got.EventID != 9 || ephemeralID != 901 {
				t.Fatalf("complete args = event %d, ephemeral %d", got.EventID, ephemeralID)
			}
			return nil
		},
	}
	tg := &fakeTelegram{}
	if err := testHandler(useCases, tg).HandleUpdate(context.Background(), callbackUpdate()); err != nil {
		t.Fatalf("HandleUpdate() error = %v", err)
	}
	if completed != 1 || len(tg.ephemeral) != 1 || len(tg.answers) != 1 {
		t.Fatalf("complete/ephemeral/answers = %d/%d/%d", completed, len(tg.ephemeral), len(tg.answers))
	}
	if got := tg.answers[0]; got.Text != "Telegram accepted the secret for this app. It should appear in this group." || got.ShowAlert {
		t.Fatalf("callback answer = %#v", got)
	}
	request := tg.ephemeral[0]
	if request.ReceiverUserID != 202 || request.CallbackQueryID != "callback-1" || request.Text != "recipient secret" || !request.ProtectContent {
		t.Fatalf("ephemeral request = %#v", request)
	}
	for index, value := range plaintext {
		if value != 0 {
			t.Fatalf("plaintext[%d] was not zeroed", index)
		}
	}
}

func TestCallbackMediaDeliveryUsesAuthorizedTelegramReference(t *testing.T) {
	caption := []byte("recipient caption")
	delivery := service.OpenDelivery{
		Whisper: domain.Whisper{
			ID: uuid.New(), SourceChatID: -1001, SourceThreadID: optionalMessageID(8), ProtectContent: true,
		},
		EventID: 11, CallbackQueryID: "callback-1",
		Content: service.PlaintextContent{
			Kind: domain.PayloadMedia,
			Media: &repository.DeliveryMedia{
				Type: domain.MediaVoice, TelegramFileID: "recipient-file-id",
			},
			Caption: caption,
		},
	}
	completed := 0
	useCases := &fakeUseCases{
		reserveOpen: func(context.Context, string, int64, string) (service.OpenDelivery, error) { return delivery, nil },
		completeOpen: func(_ context.Context, _ service.OpenDelivery, ephemeralID int64) error {
			completed++
			if ephemeralID != 902 {
				t.Fatalf("ephemeral ID = %d, want 902", ephemeralID)
			}
			return nil
		},
	}
	tg := &fakeTelegram{}
	if err := testHandler(useCases, tg).HandleUpdate(context.Background(), callbackUpdate()); err != nil {
		t.Fatalf("HandleUpdate() error = %v", err)
	}
	if completed != 1 || len(tg.media) != 1 || len(tg.answers) != 1 {
		t.Fatalf("complete/media/answers = %d/%d/%d", completed, len(tg.media), len(tg.answers))
	}
	request := tg.media[0]
	if request.ChatID != -1001 || request.MessageThreadID == nil || *request.MessageThreadID != 8 ||
		request.ReceiverUserID != 202 || request.CallbackQueryID != "callback-1" ||
		request.Type != domain.MediaVoice || request.FileID != "recipient-file-id" ||
		request.Caption != "recipient caption" || !request.ProtectContent {
		t.Fatalf("ephemeral media request = %#v", request)
	}
	for index, value := range caption {
		if value != 0 {
			t.Fatalf("caption[%d] was not zeroed", index)
		}
	}
}

func TestCallbackDeliveryFailureReleasesAndAnswers(t *testing.T) {
	wantErr := errors.New("Telegram delivery failed")
	delivery := service.OpenDelivery{
		Whisper: domain.Whisper{ID: uuid.New(), SourceChatID: -1001}, EventID: 10,
		CallbackQueryID: "callback-1", Content: service.PlaintextContent{Kind: domain.PayloadText, Text: []byte("secret")},
	}
	failed := 0
	completed := 0
	useCases := &fakeUseCases{
		reserveOpen: func(context.Context, string, int64, string) (service.OpenDelivery, error) { return delivery, nil },
		failOpen: func(_ context.Context, got service.OpenDelivery, code string) error {
			failed++
			if got.EventID != 10 || code != "telegram_delivery_failed" {
				t.Fatalf("fail args = event %d/code %q", got.EventID, code)
			}
			return nil
		},
		completeOpen: func(context.Context, service.OpenDelivery, int64) error { completed++; return nil },
	}
	tg := &fakeTelegram{sendEphemeralText: func(context.Context, telegram.SendEphemeralTextRequest) (int64, error) {
		return 0, wantErr
	}}
	if err := testHandler(useCases, tg).HandleUpdate(context.Background(), callbackUpdate()); err != nil {
		t.Fatalf("HandleUpdate() error = %v", err)
	}
	if failed != 1 || completed != 0 || len(tg.answers) != 1 {
		t.Fatalf("failed/completed/answers = %d/%d/%d", failed, completed, len(tg.answers))
	}
}

func TestCallbackWithoutMessageStillAnswers(t *testing.T) {
	update := callbackUpdate()
	update.CallbackQuery.Message = nil
	tg := &fakeTelegram{}
	if err := testHandler(&fakeUseCases{}, tg).HandleUpdate(context.Background(), update); err != nil {
		t.Fatalf("HandleUpdate() error = %v", err)
	}
	if len(tg.answers) != 1 {
		t.Fatalf("callback answers = %d, want 1", len(tg.answers))
	}
}

func TestPublicationFailureRecordsRetryPolicy(t *testing.T) {
	tests := []struct {
		name      string
		apiError  *telegram.APIError
		wantRetry bool
		wantCode  string
	}{
		{name: "rate limit without retry-after", apiError: &telegram.APIError{ErrorCode: 429}, wantRetry: true, wantCode: "telegram_rate_limited"},
		{name: "permanent rejection", apiError: &telegram.APIError{ErrorCode: 400}, wantRetry: false, wantCode: "telegram_rejected_envelope"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			publication := service.Publication{Whisper: domain.Whisper{
				ID: uuid.New(), SourceChatID: -1001, PublishAttemptCount: 1,
			}, CallbackData: "w:opaque"}
			var retry time.Duration
			var code string
			useCases := &fakeUseCases{
				claimNext: func(context.Context) (service.Publication, error) { return publication, nil },
				failPublish: func(_ context.Context, got service.Publication, gotCode string, gotRetry time.Duration) error {
					if got.Whisper.ID != publication.Whisper.ID {
						t.Fatalf("failed publication ID = %s", got.Whisper.ID)
					}
					code, retry = gotCode, gotRetry
					return nil
				},
			}
			tg := &fakeTelegram{sendMessage: func(context.Context, telegram.SendMessageRequest) (telegram.Message, error) {
				return telegram.Message{}, test.apiError
			}}
			didWork, err := testHandler(useCases, tg).PublishNext(context.Background())
			if !didWork || !errors.Is(err, test.apiError) {
				t.Fatalf("PublishNext() = %v, %v", didWork, err)
			}
			if code != test.wantCode || (retry > 0) != test.wantRetry {
				t.Fatalf("failure code/retry = %q/%s, want %q/retry=%v", code, retry, test.wantCode, test.wantRetry)
			}
		})
	}
}

func TestPublicationSuccessCompletesLease(t *testing.T) {
	publication := service.Publication{
		Whisper: domain.Whisper{ID: uuid.New(), SourceChatID: -1001, ProtectContent: true},
		Sender:  domain.User{FirstName: "Alice"}, Recipient: domain.User{FirstName: "Bob"}, CallbackData: "w:opaque",
	}
	completed := 0
	useCases := &fakeUseCases{completePublish: func(_ context.Context, got service.Publication, messageID int64) error {
		completed++
		if got.Whisper.ID != publication.Whisper.ID || messageID != 55 {
			t.Fatalf("complete publication = %s/%d", got.Whisper.ID, messageID)
		}
		return nil
	}}
	tg := &fakeTelegram{sendMessage: func(_ context.Context, request telegram.SendMessageRequest) (telegram.Message, error) {
		if strings.Contains(request.Text, "opaque") || request.ReplyMarkup == nil || request.ReplyMarkup.InlineKeyboard[0][0].CallbackData != "w:opaque" {
			t.Fatalf("envelope request = %#v", request)
		}
		return telegram.Message{MessageID: 55, Chat: telegram.Chat{ID: request.ChatID}}, nil
	}}
	if err := testHandler(useCases, tg).Publish(context.Background(), publication); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if completed != 1 {
		t.Fatalf("CompletePublication calls = %d, want 1", completed)
	}
}

func TestOwnerCommandsAuthorizeReviewAndDelete(t *testing.T) {
	whisperID := uuid.New()
	t.Run("unauthorized", func(t *testing.T) {
		deleted := 0
		useCases := &fakeUseCases{
			isOwner:     func(int64) bool { return false },
			ownerDelete: func(context.Context, int64, uuid.UUID) error { deleted++; return nil },
		}
		tg := &fakeTelegram{}
		if err := testHandler(useCases, tg).HandleUpdate(context.Background(), privateUpdate(101, "/owner_delete "+whisperID.String())); err != nil {
			t.Fatalf("HandleUpdate() error = %v", err)
		}
		if deleted != 0 || len(tg.messages) != 1 || !strings.Contains(tg.messages[0].Text, "owner-only") {
			t.Fatalf("delete/messages = %d/%#v", deleted, tg.messages)
		}
	})

	t.Run("review text", func(t *testing.T) {
		plaintext := []byte("owner-visible secret")
		var reviewed uuid.UUID
		useCases := &fakeUseCases{
			isOwner: func(id int64) bool { return id == 101 },
			ownerReview: func(_ context.Context, ownerID int64, id uuid.UUID) (service.OwnerReview, error) {
				reviewed = id
				return service.OwnerReview{Content: service.PlaintextContent{Kind: domain.PayloadText, Text: plaintext}}, nil
			},
		}
		tg := &fakeTelegram{}
		if err := testHandler(useCases, tg).HandleUpdate(context.Background(), privateUpdate(101, "/owner_open "+whisperID.String())); err != nil {
			t.Fatalf("HandleUpdate() error = %v", err)
		}
		if reviewed != whisperID || len(tg.messages) != 1 || tg.messages[0].Text != "owner-visible secret" || !tg.messages[0].ProtectContent {
			t.Fatalf("review/messages = %s/%#v", reviewed, tg.messages)
		}
		for index, value := range plaintext {
			if value != 0 {
				t.Fatalf("owner plaintext[%d] was not zeroed", index)
			}
		}
	})

	t.Run("review media", func(t *testing.T) {
		mediaBytes := []byte("owner-visible-media")
		caption := []byte("owner-visible-caption")
		useCases := &fakeUseCases{
			isOwner: func(id int64) bool { return id == 101 },
			ownerReview: func(_ context.Context, _ int64, id uuid.UUID) (service.OwnerReview, error) {
				if id != whisperID {
					t.Fatalf("review ID = %s", id)
				}
				return service.OwnerReview{Content: service.PlaintextContent{
					Kind: domain.PayloadMedia,
					Media: &repository.DeliveryMedia{
						Type: domain.MediaPhoto, ContentType: "image/jpeg", PlaintextSize: int64(len(mediaBytes)),
					},
					MediaBytes: mediaBytes, Caption: caption,
				}}, nil
			},
		}
		tg := &fakeTelegram{}
		if err := testHandler(useCases, tg).HandleUpdate(context.Background(), privateUpdate(101, "/owner_open "+whisperID.String())); err != nil {
			t.Fatalf("HandleUpdate() error = %v", err)
		}
		if len(tg.private) != 1 {
			t.Fatalf("private media sends = %d, want 1", len(tg.private))
		}
		request := tg.private[0]
		if request.ChatID != 101 || request.Type != domain.MediaPhoto || request.ContentType != "image/jpeg" ||
			request.Caption != "owner-visible-caption" || !request.ProtectContent {
			t.Fatalf("owner media request = %#v", request)
		}
		for index, value := range mediaBytes {
			if value != 0 {
				t.Fatalf("owner media[%d] was not zeroed", index)
			}
		}
		for index, value := range caption {
			if value != 0 {
				t.Fatalf("owner caption[%d] was not zeroed", index)
			}
		}
	})

	t.Run("delete", func(t *testing.T) {
		var deleted uuid.UUID
		useCases := &fakeUseCases{
			isOwner:     func(int64) bool { return true },
			ownerDelete: func(_ context.Context, _ int64, id uuid.UUID) error { deleted = id; return nil },
		}
		tg := &fakeTelegram{}
		if err := testHandler(useCases, tg).HandleUpdate(context.Background(), privateUpdate(101, "/owner_delete "+whisperID.String())); err != nil {
			t.Fatalf("HandleUpdate() error = %v", err)
		}
		if deleted != whisperID || len(tg.messages) != 1 || !strings.Contains(tg.messages[0].Text, "deleted") {
			t.Fatalf("delete/messages = %s/%#v", deleted, tg.messages)
		}
	})
}

func TestOwnerListChunksLargeMetadataResponse(t *testing.T) {
	whispers := make([]domain.Whisper, 50)
	for index := range whispers {
		whispers[index] = domain.Whisper{
			ID: uuid.New(), SenderID: int64(1000 + index), RecipientID: int64(2000 + index),
			Status: domain.WhisperActive, PublishState: domain.PublishPublished,
			Content:   domain.ContentReference{Kind: domain.PayloadText},
			CreatedAt: time.Date(2026, 8, 17, 12, index%60, 0, 0, time.UTC),
		}
	}
	useCases := &fakeUseCases{
		isOwner: func(int64) bool { return true },
		ownerList: func(_ context.Context, ownerID int64, limit, offset int) ([]domain.Whisper, error) {
			if ownerID != 101 || limit != 50 || offset != 100 {
				t.Fatalf("owner list args = %d/%d/%d", ownerID, limit, offset)
			}
			return whispers, nil
		},
	}
	tg := &fakeTelegram{}
	if err := testHandler(useCases, tg).HandleUpdate(context.Background(), privateUpdate(101, "/owner_list 50 100")); err != nil {
		t.Fatalf("HandleUpdate() error = %v", err)
	}
	if len(tg.messages) < 2 {
		t.Fatalf("owner list messages = %d, want chunked response", len(tg.messages))
	}
	var combined strings.Builder
	for _, message := range tg.messages {
		if length := len([]rune(message.Text)); length > 3500 {
			t.Fatalf("owner list chunk length = %d", length)
		}
		combined.WriteString(message.Text)
	}
	if !strings.Contains(combined.String(), whispers[0].ID.String()) || !strings.Contains(combined.String(), whispers[len(whispers)-1].ID.String()) {
		t.Fatal("chunked owner list omitted metadata")
	}
	if !strings.Contains(combined.String(), "Next page: /owner_list 50 150") {
		t.Fatal("full owner list page omitted next-page hint")
	}
}

func TestOwnerListPaginationArguments(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		wantLimit  int
		wantOffset int
		valid      bool
	}{
		{name: "defaults", command: "/owner_list", wantLimit: 20, valid: true},
		{name: "limit only", command: "/owner_list 12", wantLimit: 12, valid: true},
		{name: "limit and offset", command: "/owner_list 12 24", wantLimit: 12, wantOffset: 24, valid: true},
		{name: "zero limit", command: "/owner_list 0", valid: false},
		{name: "excessive limit", command: "/owner_list 51", valid: false},
		{name: "negative offset", command: "/owner_list 20 -1", valid: false},
		{name: "extra argument", command: "/owner_list 20 0 extra", valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			useCases := &fakeUseCases{
				isOwner: func(int64) bool { return true },
				ownerList: func(_ context.Context, ownerID int64, limit, offset int) ([]domain.Whisper, error) {
					called = true
					if ownerID != 101 || limit != test.wantLimit || offset != test.wantOffset {
						t.Fatalf("owner list args = %d/%d/%d", ownerID, limit, offset)
					}
					return nil, nil
				},
			}
			tg := &fakeTelegram{}
			if err := testHandler(useCases, tg).HandleUpdate(context.Background(), privateUpdate(101, test.command)); err != nil {
				t.Fatalf("HandleUpdate() error = %v", err)
			}
			if called != test.valid {
				t.Fatalf("OwnerList called = %t, want %t", called, test.valid)
			}
			if !test.valid && (len(tg.messages) != 1 || !strings.Contains(tg.messages[0].Text, "Usage: /owner_list")) {
				t.Fatalf("invalid pagination response = %#v", tg.messages)
			}
		})
	}
}
