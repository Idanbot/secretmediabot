package bot_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/bot"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/secretcrypto"
	"github.com/idan/secretmediabot/internal/service"
	"github.com/idan/secretmediabot/internal/telegram"
	"github.com/idan/secretmediabot/internal/testutil"
)

func newKeyring(t *testing.T) *secretcrypto.Keyring {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i*7 + 3)
	}
	keyring, err := secretcrypto.NewKeyring("v1", map[string][]byte{"v1": key})
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return keyring
}

type e2eStore struct {
	users         map[int64]domain.User
	memberships   map[int64]map[int64]domain.User
	drafts        map[int64]domain.Draft
	whispers      map[uuid.UUID]domain.Whisper
	tokens        map[string]uuid.UUID // tokenHash -> whisperID
	texts         map[uuid.UUID]repository.StoredEncryptedPayload
	medias        map[uuid.UUID]repository.StoredEncryptedPayload
	blobs         map[uuid.UUID]repository.DeliveryMedia
	callbackBlobs map[uuid.UUID]repository.StoredEncryptedPayload
}

func newE2EStore() *e2eStore {
	return &e2eStore{
		users:         make(map[int64]domain.User),
		memberships:   make(map[int64]map[int64]domain.User),
		drafts:        make(map[int64]domain.Draft),
		whispers:      make(map[uuid.UUID]domain.Whisper),
		tokens:        make(map[string]uuid.UUID),
		texts:         make(map[uuid.UUID]repository.StoredEncryptedPayload),
		medias:        make(map[uuid.UUID]repository.StoredEncryptedPayload),
		blobs:         make(map[uuid.UUID]repository.DeliveryMedia),
		callbackBlobs: make(map[uuid.UUID]repository.StoredEncryptedPayload),
	}
}

func (s *e2eStore) ObserveMembership(ctx context.Context, params repository.ObserveMembershipParams) error {
	if s.memberships[params.Chat.TelegramChatID] == nil {
		s.memberships[params.Chat.TelegramChatID] = make(map[int64]domain.User)
	}
	s.memberships[params.Chat.TelegramChatID][params.User.TelegramUserID] = params.User
	s.users[params.User.TelegramUserID] = params.User
	return nil
}

func (s *e2eStore) ObserveUser(ctx context.Context, user domain.User, now time.Time) (domain.User, error) {
	s.users[user.TelegramUserID] = user
	return user, nil
}

func (s *e2eStore) FindObservedUserByID(ctx context.Context, chatID int64, userID int64) (domain.User, error) {
	if chat, ok := s.memberships[chatID]; ok {
		if u, ok := chat[userID]; ok {
			return u, nil
		}
	}
	if u, ok := s.users[userID]; ok {
		return u, nil
	}
	return domain.User{}, repository.ErrNotFound
}

func (s *e2eStore) FindObservedUserByUsername(ctx context.Context, chatID int64, username string) (domain.User, error) {
	clean := strings.ToLower(strings.TrimPrefix(username, "@"))
	if chat, ok := s.memberships[chatID]; ok {
		for _, u := range chat {
			if strings.ToLower(u.Username) == clean {
				return u, nil
			}
		}
	}
	for _, u := range s.users {
		if strings.ToLower(u.Username) == clean {
			return u, nil
		}
	}
	return domain.User{}, repository.ErrNotFound
}

func (s *e2eStore) CountActiveDrafts(ctx context.Context, senderID int64, now time.Time) (int64, error) {
	if d, ok := s.drafts[senderID]; ok && (d.State == domain.DraftAwaitingMedia || d.State == domain.DraftIngestingMedia) {
		return 1, nil
	}
	return 0, nil
}

func (s *e2eStore) CountRecentWhispersBySender(ctx context.Context, senderID int64, since time.Time) (int64, error) {
	return 0, nil
}

func (s *e2eStore) CreateDraft(ctx context.Context, params repository.CreateDraftParams) (domain.Draft, error) {
	s.drafts[params.Draft.SenderID] = params.Draft
	return params.Draft, nil
}

func (s *e2eStore) FindDraftByComposeTokenHash(ctx context.Context, hash []byte) (domain.Draft, error) {
	for _, d := range s.drafts {
		if string(d.ComposeTokenHash) == string(hash) {
			return d, nil
		}
	}
	return domain.Draft{}, repository.ErrNotFound
}

func (s *e2eStore) CancelLatestDraftForSender(ctx context.Context, senderID int64, now time.Time) (domain.Draft, error) {
	if d, ok := s.drafts[senderID]; ok {
		d.State = domain.DraftCancelled
		s.drafts[senderID] = d
		return d, nil
	}
	return domain.Draft{}, repository.ErrNotFound
}

func (s *e2eStore) ClaimLatestDraftIngest(ctx context.Context, senderID int64, now, lease time.Time) (domain.Draft, error) {
	d, ok := s.drafts[senderID]
	if !ok || d.State != domain.DraftAwaitingMedia {
		return domain.Draft{}, repository.ErrNotFound
	}
	d.State = domain.DraftIngestingMedia
	d.IngestLeaseUntil = &lease
	s.drafts[senderID] = d
	return d, nil
}

func (s *e2eStore) ReleaseDraftIngest(ctx context.Context, params repository.ReleaseDraftIngestParams) error {
	if d, ok := s.drafts[params.SenderID]; ok {
		d.State = domain.DraftAwaitingMedia
		d.IngestLeaseUntil = nil
		s.drafts[params.SenderID] = d
		return nil
	}
	return repository.ErrNotFound
}

func (s *e2eStore) FinalizeDraft(ctx context.Context, params repository.FinalizeDraftParams) (domain.Whisper, error) {
	w := params.Whisper
	w.PublishState = domain.PublishPending
	w.Status = domain.WhisperActive
	s.whispers[w.ID] = w
	s.tokens[string(w.OpenTokenHash)] = w.ID

	if params.CallbackToken != nil {
		s.callbackBlobs[w.ID] = repository.StoredEncryptedPayload{
			ID:                  params.CallbackToken.ID,
			EncryptionAlgorithm: "AES-256-GCM",
			EncryptionKeyID:     params.CallbackToken.Payload.KeyID,
			Nonce:               params.CallbackToken.Payload.Nonce,
			Ciphertext:          params.CallbackToken.Payload.Ciphertext,
			CiphertextSHA256:    params.CallbackToken.Payload.CiphertextSHA256[:],
			ContentType:         params.CallbackToken.ContentType,
			PlaintextSize:       params.CallbackToken.PlaintextSize,
			RetainUntil:         params.CallbackToken.RetainUntil,
		}
	}

	if params.Text != nil {
		s.texts[w.ID] = repository.StoredEncryptedPayload{
			ID:                  params.Text.ID,
			EncryptionAlgorithm: "AES-256-GCM",
			EncryptionKeyID:     params.Text.Payload.KeyID,
			Nonce:               params.Text.Payload.Nonce,
			Ciphertext:          params.Text.Payload.Ciphertext,
			CiphertextSHA256:    params.Text.Payload.CiphertextSHA256[:],
			ContentType:         params.Text.ContentType,
			PlaintextSize:       params.Text.PlaintextSize,
			RetainUntil:         params.Text.RetainUntil,
		}
	}
	if params.Media != nil {
		s.medias[w.ID] = repository.StoredEncryptedPayload{
			ID:                  params.Media.ID,
			EncryptionAlgorithm: "AES-256-GCM",
			EncryptionKeyID:     params.Media.Payload.KeyID,
			Nonce:               params.Media.Payload.Nonce,
			Ciphertext:          params.Media.Payload.Ciphertext,
			CiphertextSHA256:    params.Media.Payload.CiphertextSHA256[:],
			ContentType:         params.Media.ContentType,
			PlaintextSize:       params.Media.PlaintextSize,
			RetainUntil:         params.Media.RetainUntil,
		}
	}

	delete(s.drafts, params.SenderID)
	return w, nil
}

func (s *e2eStore) ClaimPublish(ctx context.Context, params repository.ClaimPublishParams) (repository.PublishClaim, error) {
	w, ok := s.whispers[params.WhisperID]
	if !ok {
		return repository.PublishClaim{}, repository.ErrNotFound
	}
	w.PublishState = domain.PublishPublishing
	s.whispers[w.ID] = w
	return repository.PublishClaim{
		Whisper:       w,
		CallbackToken: s.callbackBlobs[w.ID],
	}, nil
}

func (s *e2eStore) ClaimNextPublish(ctx context.Context, now, lease time.Time) (repository.PublishClaim, error) {
	for _, w := range s.whispers {
		if w.PublishState == domain.PublishPending {
			w.PublishState = domain.PublishPublishing
			s.whispers[w.ID] = w
			return repository.PublishClaim{
				Whisper:       w,
				CallbackToken: s.callbackBlobs[w.ID],
			}, nil
		}
	}
	return repository.PublishClaim{}, repository.ErrNotFound
}

func (s *e2eStore) MarkPublished(ctx context.Context, params repository.MarkPublishedParams) error {
	w := s.whispers[params.WhisperID]
	w.PublishState = domain.PublishPublished
	w.PublicMessageID = &params.PublicMessageID
	s.whispers[w.ID] = w
	return nil
}

func (s *e2eStore) MarkPublishFailed(ctx context.Context, params repository.MarkPublishFailedParams) error {
	w := s.whispers[params.WhisperID]
	w.PublishState = domain.PublishFailed
	s.whispers[w.ID] = w
	return nil
}

func (s *e2eStore) ReserveOpen(ctx context.Context, params repository.ReserveOpenParams) (repository.OpenReservation, error) {
	whisperID, ok := s.tokens[string(params.OpenTokenHash)]
	if !ok {
		return repository.OpenReservation{}, repository.ErrNotFound
	}
	w := s.whispers[whisperID]
	if w.RecipientID != params.TelegramUserID {
		return repository.OpenReservation{}, repository.ErrUnauthorized
	}
	if w.Status != domain.WhisperActive {
		return repository.OpenReservation{}, repository.ErrAlreadyOpened
	}
	w.Status = domain.WhisperOpening
	s.whispers[whisperID] = w

	content := repository.DeliveryContent{Kind: w.Content.Kind}
	if txt, ok := s.texts[w.ID]; ok {
		content.Text = &txt
	}
	if blob, ok := s.blobs[w.ID]; ok {
		content.Media = &blob
	}

	return repository.OpenReservation{
		Whisper: w,
		EventID: 1,
		Content: content,
	}, nil
}

func (s *e2eStore) CompleteOpen(ctx context.Context, params repository.CompleteOpenParams) error {
	w := s.whispers[params.WhisperID]
	w.Status = domain.WhisperOpened
	w.OpenedAt = &params.Now
	s.whispers[w.ID] = w
	return nil
}

func (s *e2eStore) FailOpen(ctx context.Context, params repository.FailOpenParams) error {
	w := s.whispers[params.WhisperID]
	w.Status = domain.WhisperActive
	s.whispers[w.ID] = w
	return nil
}

func (s *e2eStore) OwnerListWhispers(ctx context.Context, params repository.OwnerListWhispersParams) ([]domain.Whisper, error) {
	list := make([]domain.Whisper, 0, len(s.whispers))
	for _, w := range s.whispers {
		list = append(list, w)
	}
	return list, nil
}

func (s *e2eStore) OwnerGetWhisper(ctx context.Context, params repository.OwnerGetWhisperParams) (domain.Whisper, error) {
	if w, ok := s.whispers[params.WhisperID]; ok {
		return w, nil
	}
	return domain.Whisper{}, repository.ErrNotFound
}

func (s *e2eStore) OwnerFetchEncryptedContent(ctx context.Context, params repository.OwnerGetWhisperParams) (repository.StoredContent, error) {
	return repository.StoredContent{}, nil
}

func (s *e2eStore) OwnerDeleteWhisper(ctx context.Context, params repository.OwnerDeleteWhisperParams) error {
	delete(s.whispers, params.WhisperID)
	return nil
}

func (s *e2eStore) OwnerUpdateRetention(ctx context.Context, params repository.OwnerUpdateRetentionParams) error {
	return nil
}

func (s *e2eStore) FetchWhisperMedia(ctx context.Context, id uuid.UUID) (repository.WhisperMediaBlob, error) {
	return repository.WhisperMediaBlob{}, errors.New("no media blob")
}

func TestE2ETextWhisperFullUserJourney(t *testing.T) {
	t.Parallel()

	mockServer := testutil.NewTelegramMockServer("secretmediabot")
	defer mockServer.Close()

	client, err := telegram.NewClient(telegram.ClientConfig{
		Token:   mockServer.BotToken,
		BaseURL: mockServer.BaseURL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	keyring := newKeyring(t)
	store := newE2EStore()
	useCases, err := service.New(store, keyring, service.Options{
		DraftTTL:                       time.Hour,
		WhisperTTL:                     24 * time.Hour,
		ContentRetention:               30 * 24 * time.Hour,
		IngestLease:                    time.Minute,
		OpenLease:                      30 * time.Second,
		PublishLease:                   time.Minute,
		EphemeralDeleteAfter:           30 * time.Second,
		MaxMediaBytes:                  20 * 1024 * 1024,
		MaxActiveDraftsPerUser:         1,
		MaxWhispersPerUserPerHour:      10,
		MaxActiveGuestRequestsPerUser:  1,
		MaxGuestRequestsPerUserPerHour: 10,
		DefaultOneTime:                 true,
		ProtectContent:                 true,
		OwnerIDs:                       []int64{999},
	})
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}

	handler, err := bot.New(bot.Config{
		Service:              useCases,
		Telegram:             client,
		BotUsername:          "secretmediabot",
		MaxMediaBytes:        20 * 1024 * 1024,
		MediaDownloadTimeout: 10 * time.Second,
		RequestTimeout:       5 * time.Second,
		Logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("bot.New: %v", err)
	}

	ctx := context.Background()
	sender := telegram.User{ID: 101, FirstName: "Alice", Username: "alice_user"}
	recipient := telegram.User{ID: 202, FirstName: "Bob", Username: "bobby_user"}
	intruder := telegram.User{ID: 303, FirstName: "Eve", Username: "eve_user"}
	groupChat := telegram.Chat{ID: -1001, Type: "supergroup", Title: "Secret Group"}

	// Step 1: Pre-observe group members
	if err := handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 1,
		Message: &telegram.Message{
			MessageID: 10,
			Chat:      groupChat,
			From:      &sender,
			Text:      "Hello group",
		},
	}); err != nil {
		t.Fatalf("HandleUpdate(member message) error = %v", err)
	}
	if err := handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 2,
		Message: &telegram.Message{
			MessageID: 11,
			Chat:      groupChat,
			From:      &recipient,
			Text:      "Hey Alice",
		},
	}); err != nil {
		t.Fatalf("HandleUpdate(recipient message) error = %v", err)
	}

	// Step 2: Sender sends /whisper @bobby_user in group
	if err := handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 3,
		Message: &telegram.Message{
			MessageID: 12,
			Chat:      groupChat,
			From:      &sender,
			Text:      "/whisper @bobby_user",
		},
	}); err != nil {
		t.Fatalf("HandleUpdate(/whisper @bobby_user) error = %v", err)
	}

	// Verify bot replied with private composer instructions
	if len(mockServer.SentMessages) == 0 {
		t.Fatal("expected bot to reply with private composer instructions")
	}

	// Step 3: Sender sends secret text in private chat (which finalizes and publishes group envelope)
	privateChat := telegram.Chat{ID: 101, Type: "private"}
	if err := handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 4,
		Message: &telegram.Message{
			MessageID: 20,
			Chat:      privateChat,
			From:      &sender,
			Text:      "Top secret information for Bob only",
		},
	}); err != nil {
		t.Fatalf("HandleUpdate(secret text) error = %v", err)
	}

	// Step 4: Verify group envelope was posted with callback button
	var envelopeMsg telegram.SendMessageRequest
	foundEnvelope := false
	for _, msg := range mockServer.SentMessages {
		if msg.ChatID == groupChat.ID && msg.ReplyMarkup != nil && len(msg.ReplyMarkup.InlineKeyboard) > 0 {
			envelopeMsg = msg
			foundEnvelope = true
			break
		}
	}
	if !foundEnvelope {
		t.Fatal("expected bot to post group envelope with callback button")
	}
	callbackData := envelopeMsg.ReplyMarkup.InlineKeyboard[0][0].CallbackData

	// Step 5: Unauthorized user (Eve) tries to click Open secret -> rejected with alert
	if err := handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 5,
		CallbackQuery: &telegram.CallbackQuery{
			ID:   "cb_eve_1",
			From: intruder,
			Message: &telegram.Message{
				MessageID: 100,
				Chat:      groupChat,
			},
			Data: callbackData,
		},
	}); err != nil {
		t.Fatalf("HandleUpdate(unauthorized callback) error = %v", err)
	}

	// Verify unauthorized user received alert
	foundAlert := false
	for _, cb := range mockServer.AnsweredCallbacks {
		if cb.CallbackQueryID == "cb_eve_1" && cb.ShowAlert {
			foundAlert = true
			break
		}
	}
	if !foundAlert {
		t.Fatal("expected unauthorized user to receive an alert refusal")
	}

	// Step 6: Authorized recipient (Bob) clicks Open secret -> ephemeral delivery
	if err := handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 6,
		CallbackQuery: &telegram.CallbackQuery{
			ID:   "cb_bob_1",
			From: recipient,
			Message: &telegram.Message{
				MessageID: 100,
				Chat:      groupChat,
			},
			Data: callbackData,
		},
	}); err != nil {
		t.Fatalf("HandleUpdate(authorized callback) error = %v", err)
	}

	// Verify whisper is marked opened in store
	var openedWhisper domain.Whisper
	for _, w := range store.whispers {
		openedWhisper = w
		break
	}
	if openedWhisper.Status != domain.WhisperOpened {
		t.Fatalf("whisper status = %v, want opened", openedWhisper.Status)
	}
}

func TestE2ECancelActiveDraft(t *testing.T) {
	t.Parallel()

	mockServer := testutil.NewTelegramMockServer("secretmediabot")
	defer mockServer.Close()

	client, err := telegram.NewClient(telegram.ClientConfig{
		Token:   mockServer.BotToken,
		BaseURL: mockServer.BaseURL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	keyring := newKeyring(t)
	store := newE2EStore()
	useCases, err := service.New(store, keyring, service.Options{
		DraftTTL:                       time.Hour,
		WhisperTTL:                     24 * time.Hour,
		ContentRetention:               30 * 24 * time.Hour,
		IngestLease:                    time.Minute,
		OpenLease:                      30 * time.Second,
		PublishLease:                   time.Minute,
		EphemeralDeleteAfter:           30 * time.Second,
		MaxMediaBytes:                  20 * 1024 * 1024,
		MaxActiveDraftsPerUser:         1,
		MaxWhispersPerUserPerHour:      10,
		MaxActiveGuestRequestsPerUser:  1,
		MaxGuestRequestsPerUserPerHour: 10,
		DefaultOneTime:                 true,
		ProtectContent:                 true,
		OwnerIDs:                       []int64{999},
	})
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}

	handler, err := bot.New(bot.Config{
		Service:              useCases,
		Telegram:             client,
		BotUsername:          "secretmediabot",
		MaxMediaBytes:        20 * 1024 * 1024,
		MediaDownloadTimeout: 10 * time.Second,
		RequestTimeout:       5 * time.Second,
		Logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("bot.New: %v", err)
	}

	ctx := context.Background()
	sender := telegram.User{ID: 101, FirstName: "Alice", Username: "alice_user"}
	recipient := telegram.User{ID: 202, FirstName: "Bob", Username: "bobby_user"}
	groupChat := telegram.Chat{ID: -1001, Type: "supergroup", Title: "Secret Group"}

	_ = handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 1,
		Message:  &telegram.Message{MessageID: 10, Chat: groupChat, From: &sender, Text: "Hi"},
	})
	_ = handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 2,
		Message:  &telegram.Message{MessageID: 11, Chat: groupChat, From: &recipient, Text: "Hey"},
	})
	_ = handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 3,
		Message:  &telegram.Message{MessageID: 12, Chat: groupChat, From: &sender, Text: "/whisper @bobby_user"},
	})

	if count, _ := store.CountActiveDrafts(ctx, sender.ID, time.Now()); count != 1 {
		t.Fatalf("expected 1 active draft, got %d", count)
	}

	// Sender sends /cancel in private chat
	privateChat := telegram.Chat{ID: sender.ID, Type: "private"}
	if err := handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 4,
		Message:  &telegram.Message{MessageID: 20, Chat: privateChat, From: &sender, Text: "/cancel"},
	}); err != nil {
		t.Fatalf("HandleUpdate(/cancel) error = %v", err)
	}

	if count, _ := store.CountActiveDrafts(ctx, sender.ID, time.Now()); count != 0 {
		t.Fatalf("expected 0 active drafts after /cancel, got %d", count)
	}
}

func TestE2EPhotoWhisperFlow(t *testing.T) {
	t.Parallel()

	mockServer := testutil.NewTelegramMockServer("secretmediabot")
	defer mockServer.Close()

	client, err := telegram.NewClient(telegram.ClientConfig{
		Token:   mockServer.BotToken,
		BaseURL: mockServer.BaseURL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	keyring := newKeyring(t)
	store := newE2EStore()
	useCases, err := service.New(store, keyring, service.Options{
		DraftTTL:                       time.Hour,
		WhisperTTL:                     24 * time.Hour,
		ContentRetention:               30 * 24 * time.Hour,
		IngestLease:                    time.Minute,
		OpenLease:                      30 * time.Second,
		PublishLease:                   time.Minute,
		EphemeralDeleteAfter:           30 * time.Second,
		MaxMediaBytes:                  20 * 1024 * 1024,
		MaxActiveDraftsPerUser:         1,
		MaxWhispersPerUserPerHour:      10,
		MaxActiveGuestRequestsPerUser:  1,
		MaxGuestRequestsPerUserPerHour: 10,
		DefaultOneTime:                 true,
		ProtectContent:                 true,
		OwnerIDs:                       []int64{999},
	})
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}

	handler, err := bot.New(bot.Config{
		Service:              useCases,
		Telegram:             client,
		BotUsername:          "secretmediabot",
		MaxMediaBytes:        20 * 1024 * 1024,
		MediaDownloadTimeout: 10 * time.Second,
		RequestTimeout:       5 * time.Second,
		Logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("bot.New: %v", err)
	}

	ctx := context.Background()
	sender := telegram.User{ID: 101, FirstName: "Alice", Username: "alice_user"}
	recipient := telegram.User{ID: 202, FirstName: "Bob", Username: "bobby_user"}
	groupChat := telegram.Chat{ID: -1001, Type: "supergroup", Title: "Secret Group"}

	_ = handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 1, Message: &telegram.Message{MessageID: 10, Chat: groupChat, From: &sender, Text: "Hi"},
	})
	_ = handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 2, Message: &telegram.Message{MessageID: 11, Chat: groupChat, From: &recipient, Text: "Hey"},
	})
	_ = handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 3, Message: &telegram.Message{MessageID: 12, Chat: groupChat, From: &sender, Text: "/whisper @bobby_user"},
	})

	// Sender sends photo in private chat
	privateChat := telegram.Chat{ID: sender.ID, Type: "private"}
	if err := handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 4,
		Message: &telegram.Message{
			MessageID: 20,
			Chat:      privateChat,
			From:      &sender,
			Caption:   "Confidential diagram",
			Photo: []telegram.PhotoSize{
				{FileID: "photo_large", FileUniqueID: "u_photo_large", Width: 800, Height: 600, FileSize: 30},
			},
		},
	}); err != nil {
		t.Fatalf("HandleUpdate(photo secret) error = %v", err)
	}

	// Verify group envelope was posted
	var callbackData string
	for _, msg := range mockServer.SentMessages {
		if msg.ChatID == groupChat.ID && msg.ReplyMarkup != nil && len(msg.ReplyMarkup.InlineKeyboard) > 0 {
			callbackData = msg.ReplyMarkup.InlineKeyboard[0][0].CallbackData
			break
		}
	}
	if callbackData == "" {
		t.Fatal("expected group envelope to be posted for photo whisper")
	}

	// Recipient opens photo whisper
	if err := handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 5,
		CallbackQuery: &telegram.CallbackQuery{
			ID:   "cb_bob_photo",
			From: recipient,
			Message: &telegram.Message{
				MessageID: 100,
				Chat:      groupChat,
			},
			Data: callbackData,
		},
	}); err != nil {
		t.Fatalf("HandleUpdate(open photo callback) error = %v", err)
	}
}

