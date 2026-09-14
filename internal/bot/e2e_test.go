package bot_test

import (
	"context"
	"errors"
	"fmt"
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
	captions      map[uuid.UUID]repository.StoredEncryptedPayload
	blobs         map[uuid.UUID]repository.DeliveryMedia
	callbackBlobs map[uuid.UUID]repository.StoredEncryptedPayload
	guests        map[uuid.UUID]repository.GuestRequest
	guestTokens   map[string]uuid.UUID // tokenHash -> guestID
	guestTexts    map[uuid.UUID]repository.StoredEncryptedPayload
	guestMedias   map[uuid.UUID]repository.StoredEncryptedPayload
	guestCaptions map[uuid.UUID]repository.StoredEncryptedPayload
	guestBlobs    map[uuid.UUID]repository.DeliveryMedia
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
		captions:      make(map[uuid.UUID]repository.StoredEncryptedPayload),
		blobs:         make(map[uuid.UUID]repository.DeliveryMedia),
		callbackBlobs: make(map[uuid.UUID]repository.StoredEncryptedPayload),
		guests:        make(map[uuid.UUID]repository.GuestRequest),
		guestTokens:   make(map[string]uuid.UUID),
		guestTexts:    make(map[uuid.UUID]repository.StoredEncryptedPayload),
		guestMedias:   make(map[uuid.UUID]repository.StoredEncryptedPayload),
		guestCaptions: make(map[uuid.UUID]repository.StoredEncryptedPayload),
		guestBlobs:    make(map[uuid.UUID]repository.DeliveryMedia),
	}
}

type e2eEnv struct {
	t       *testing.T
	mock    *testutil.TelegramMockServer
	client  *telegram.Client
	store   *e2eStore
	svc     *service.Service
	handler *bot.Handler
	opts    service.Options
	ctx     context.Context
	nextID  int64

	sender    telegram.User
	recipient telegram.User
	intruder  telegram.User
	owner     telegram.User
	group     telegram.Chat
}

func newE2E(t *testing.T, tweak ...func(*service.Options)) *e2eEnv {
	t.Helper()

	opts := service.Options{
		DraftTTL:                       time.Hour,
		WhisperTTL:                     24 * time.Hour,
		ContentRetention:               30 * 24 * time.Hour,
		IngestLease:                    time.Minute,
		OpenLease:                      30 * time.Second,
		PublishLease:                   time.Minute,
		EphemeralDeleteAfter:           30 * time.Second,
		MaxMediaBytes:                  20 * 1024 * 1024,
		MaxActiveDraftsPerUser:         5,
		MaxWhispersPerUserPerHour:      50,
		MaxActiveGuestRequestsPerUser:  25,
		MaxGuestRequestsPerUserPerHour: 100,
		DefaultOneTime:                 true,
		ProtectContent:                 true,
		OwnerIDs:                       []int64{999},
		GuestModeEnabled:               true,
	}
	for _, fn := range tweak {
		fn(&opts)
	}

	mock := testutil.NewTelegramMockServer("secretmediabot")
	t.Cleanup(mock.Close)

	client, err := telegram.NewClient(telegram.ClientConfig{
		Token:   mock.BotToken,
		BaseURL: mock.BaseURL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	store := newE2EStore()
	svc, err := service.New(store, newKeyring(t), opts)
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	handler, err := bot.New(bot.Config{
		Service:              svc,
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

	return &e2eEnv{
		t: t, mock: mock, client: client, store: store, svc: svc, handler: handler, opts: opts,
		ctx:       context.Background(),
		sender:    telegram.User{ID: 101, FirstName: "Alice", Username: "alice_user"},
		recipient: telegram.User{ID: 202, FirstName: "Bob", Username: "bobby_user"},
		intruder:  telegram.User{ID: 303, FirstName: "Eve", Username: "eve_user"},
		owner:     telegram.User{ID: 999, FirstName: "Owner", Username: "owner_user"},
		group:     telegram.Chat{ID: -1001, Type: "supergroup", Title: "Secret Group"},
	}
}

func (e *e2eEnv) private(user telegram.User) telegram.Chat {
	return telegram.Chat{ID: user.ID, Type: "private"}
}

func (e *e2eEnv) handle(update telegram.Update) {
	e.t.Helper()
	e.nextID++
	update.UpdateID = e.nextID
	if err := e.handler.HandleUpdate(e.ctx, update); err != nil {
		e.t.Fatalf("HandleUpdate: %v", err)
	}
}

func (e *e2eEnv) observeGroup(users ...telegram.User) {
	e.t.Helper()
	for i, user := range users {
		u := user
		e.handle(telegram.Update{Message: &telegram.Message{
			MessageID: int64(10 + i), Chat: e.group, From: &u, Text: "hi",
		}})
	}
}

func (e *e2eEnv) envelopeCallback() string {
	e.t.Helper()
	for _, msg := range e.mock.SentMessages {
		if msg.ChatID == e.group.ID && msg.ReplyMarkup != nil && len(msg.ReplyMarkup.InlineKeyboard) > 0 {
			return msg.ReplyMarkup.InlineKeyboard[0][0].CallbackData
		}
	}
	e.t.Fatal("expected group envelope with callback button")
	return ""
}

func (e *e2eEnv) openCallback(id string, from telegram.User, data string) {
	e.t.Helper()
	e.handle(telegram.Update{CallbackQuery: &telegram.CallbackQuery{
		ID: id, From: from,
		Message: &telegram.Message{MessageID: 100, Chat: e.group},
		Data:    data,
	}})
}

func (e *e2eEnv) privateStart(user telegram.User, param string) {
	e.t.Helper()
	u := user
	e.handle(telegram.Update{Message: &telegram.Message{
		Chat: e.private(user), From: &u, Text: "/start " + param,
	}})
}

func (e *e2eEnv) latestInlineButton() telegram.InlineKeyboardButton {
	e.t.Helper()
	if len(e.mock.AnsweredInlineQueries) == 0 {
		e.t.Fatal("expected AnswerInlineQuery")
	}
	ans := e.mock.AnsweredInlineQueries[len(e.mock.AnsweredInlineQueries)-1]
	if len(ans.Results) == 0 || ans.Results[0].ReplyMarkup == nil ||
		len(ans.Results[0].ReplyMarkup.InlineKeyboard) == 0 ||
		len(ans.Results[0].ReplyMarkup.InlineKeyboard[0]) == 0 {
		e.t.Fatalf("inline answer missing button: %#v", ans)
	}
	return ans.Results[0].ReplyMarkup.InlineKeyboard[0][0]
}

func (e *e2eEnv) latestStartParam() string {
	e.t.Helper()
	url := e.latestInlineButton().URL
	idx := strings.Index(url, "?start=")
	if idx == -1 {
		e.t.Fatalf("button URL missing start param: %q", url)
	}
	return url[idx+7:]
}

func (e *e2eEnv) requireSent(chatID int64, substr string) {
	e.t.Helper()
	for _, msg := range e.mock.SentMessages {
		if msg.ChatID == chatID && strings.Contains(msg.Text, substr) {
			return
		}
	}
	e.t.Fatalf("no message to chat %d containing %q", chatID, substr)
}

func (e *e2eEnv) requireEphemeral(receiverID int64, substr string) {
	e.t.Helper()
	for _, msg := range e.mock.SentMessages {
		if msg.ReceiverUserID == receiverID && strings.Contains(msg.Text, substr) {
			return
		}
	}
	e.t.Fatalf("no ephemeral text for user %d containing %q", receiverID, substr)
}

func (e *e2eEnv) requireCallback(id string, alert bool, substr string) {
	e.t.Helper()
	for _, cb := range e.mock.AnsweredCallbacks {
		if cb.CallbackQueryID != id {
			continue
		}
		if cb.ShowAlert != alert {
			e.t.Fatalf("callback %s ShowAlert = %v, want %v", id, cb.ShowAlert, alert)
		}
		if substr != "" && !strings.Contains(cb.Text, substr) {
			e.t.Fatalf("callback %s text = %q, want substring %q", id, cb.Text, substr)
		}
		return
	}
	e.t.Fatalf("callback %s was not answered", id)
}

func (e *e2eEnv) methodCount(method string) int {
	n := 0
	for _, call := range e.mock.RecordedCalls() {
		if call.Method == method {
			n++
		}
	}
	return n
}

func (e *e2eEnv) requireMethod(method string) {
	e.t.Helper()
	if e.methodCount(method) == 0 {
		e.t.Fatalf("expected Telegram method %s", method)
	}
}

func (e *e2eEnv) openedWhisper() domain.Whisper {
	e.t.Helper()
	for _, w := range e.store.whispers {
		return w
	}
	e.t.Fatal("expected a whisper in the store")
	return domain.Whisper{}
}

func (e *e2eEnv) replaceService(keyring *secretcrypto.Keyring) {
	e.t.Helper()
	svc, err := service.New(e.store, keyring, e.opts)
	if err != nil {
		e.t.Fatalf("service.New: %v", err)
	}
	handler, err := bot.New(bot.Config{
		Service:              svc,
		Telegram:             e.client,
		BotUsername:          "secretmediabot",
		MaxMediaBytes:        20 * 1024 * 1024,
		MediaDownloadTimeout: 10 * time.Second,
		RequestTimeout:       5 * time.Second,
		Logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		e.t.Fatalf("bot.New: %v", err)
	}
	e.svc = svc
	e.handler = handler
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
		mediaType := domain.MediaPhoto
		if w.Content.Media != nil {
			mediaType = w.Content.Media.Type
		}
		s.blobs[w.ID] = repository.DeliveryMedia{
			BlobID:               params.Media.ID,
			Type:                 mediaType,
			TelegramFileID:       params.TelegramFileID,
			TelegramFileUniqueID: params.TelegramFileUniqueID,
			ContentType:          params.Media.ContentType,
			PlaintextSize:        params.Media.PlaintextSize,
		}
	}
	if params.Caption != nil {
		s.captions[w.ID] = repository.StoredEncryptedPayload{
			ID:                  params.Caption.ID,
			EncryptionAlgorithm: "AES-256-GCM",
			EncryptionKeyID:     params.Caption.Payload.KeyID,
			Nonce:               params.Caption.Payload.Nonce,
			Ciphertext:          params.Caption.Payload.Ciphertext,
			CiphertextSHA256:    params.Caption.Payload.CiphertextSHA256[:],
			ContentType:         params.Caption.ContentType,
			PlaintextSize:       params.Caption.PlaintextSize,
			RetainUntil:         params.Caption.RetainUntil,
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
	if w.IsExpired(params.Now) || w.Status == domain.WhisperExpired {
		return repository.OpenReservation{}, repository.ErrExpired
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
	if caption, ok := s.captions[w.ID]; ok {
		content.Caption = &caption
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

func (s *e2eStore) OwnerListWhisperDetails(ctx context.Context, params repository.OwnerListWhispersParams) ([]domain.OwnerWhisper, error) {
	list, err := s.OwnerListWhispers(ctx, params)
	if err != nil {
		return nil, err
	}
	details := make([]domain.OwnerWhisper, 0, len(list))
	for _, whisper := range list {
		details = append(details, domain.OwnerWhisper{
			Whisper:   whisper,
			Sender:    s.users[whisper.SenderID],
			Recipient: s.users[whisper.RecipientID],
		})
	}
	return details, nil
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
	media, ok := s.medias[id]
	if !ok {
		return repository.WhisperMediaBlob{}, errors.New("no media blob")
	}
	blob := s.blobs[id]
	return repository.WhisperMediaBlob{
		WhisperID:   id,
		MediaType:   blob.Type,
		ContentType: blob.ContentType,
		Stored:      media,
	}, nil
}

func (s *e2eStore) CreateGuestRequest(ctx context.Context, params repository.GuestCreateParams) (repository.GuestRequest, error) {
	if params.MaxActivePerSender > 0 {
		active := 0
		for _, existing := range s.guests {
			if existing.SenderID != params.Request.SenderID {
				continue
			}
			switch existing.State {
			case repository.GuestStateAwaitingSecret, repository.GuestStateIngestingSecret, repository.GuestStateReady:
				active++
			}
		}
		if active+1 > params.MaxActivePerSender {
			return repository.GuestRequest{}, repository.ErrGuestActiveLimit
		}
	}
	req := params.Request
	s.guests[req.ID] = req
	s.guestTokens[string(req.TokenHash)] = req.ID
	if params.TextPayload != nil {
		s.guestTexts[req.ID] = repository.StoredEncryptedPayload{
			ID:                  params.TextPayload.ID,
			EncryptionAlgorithm: params.TextPayload.EncryptionAlgorithm,
			EncryptionKeyID:     params.TextPayload.EncryptionKeyID,
			Nonce:               params.TextPayload.Nonce,
			Ciphertext:          params.TextPayload.Ciphertext,
			CiphertextSHA256:    params.TextPayload.CiphertextSHA256[:],
			ContentType:         params.TextPayload.ContentType,
			PlaintextSize:       params.TextPayload.PlaintextSize,
			RetainUntil:         params.TextPayload.RetainUntil,
		}
	}
	return req, nil
}

func (s *e2eStore) FindGuestRequestByTokenHash(ctx context.Context, hash []byte) (repository.GuestRequest, error) {
	id, ok := s.guestTokens[string(hash)]
	if !ok {
		return repository.GuestRequest{}, repository.ErrNotFound
	}
	req, ok := s.guests[id]
	if !ok {
		return repository.GuestRequest{}, repository.ErrNotFound
	}
	return req, nil
}

func (s *e2eStore) FindAwaitingGuestSecret(ctx context.Context, senderID int64, now time.Time) (repository.GuestRequest, error) {
	for _, req := range s.guests {
		if req.SenderID == senderID && (req.State == repository.GuestStateAwaitingSecret || req.State == repository.GuestStateIngestingSecret) {
			return req, nil
		}
	}
	return repository.GuestRequest{}, repository.ErrNotFound
}

func (s *e2eStore) ClaimGuestTarget(ctx context.Context, params repository.GuestClaimTargetParams) (repository.GuestRequest, error) {
	id, ok := s.guestTokens[string(params.TokenHash)]
	if !ok {
		return repository.GuestRequest{}, repository.ErrNotFound
	}
	req := s.guests[id]
	if req.TargetUserID != nil && *req.TargetUserID != params.User.TelegramUserID {
		return repository.GuestRequest{}, repository.ErrUnauthorized
	}
	if req.TargetUserID == nil && req.TargetUsername != "" &&
		!strings.EqualFold(strings.TrimPrefix(params.User.Username, "@"), strings.TrimPrefix(req.TargetUsername, "@")) {
		return repository.GuestRequest{}, repository.ErrUnauthorized
	}
	if req.SenderID == params.User.TelegramUserID {
		return repository.GuestRequest{}, repository.ErrUnauthorized
	}
	req.TargetUserID = &params.User.TelegramUserID
	req.TargetClaimedAt = &params.Now
	s.guests[id] = req
	return req, nil
}

func (s *e2eStore) ClaimGuestIngest(ctx context.Context, params repository.GuestClaimIngestParams) (repository.GuestRequest, error) {
	for _, req := range s.guests {
		if req.SenderID == params.SenderID && (req.State == repository.GuestStateAwaitingSecret || req.State == repository.GuestStateIngestingSecret) {
			req.State = repository.GuestStateIngestingSecret
			req.IngestLeaseUntil = &params.LeaseUntil
			s.guests[req.ID] = req
			return req, nil
		}
	}
	return repository.GuestRequest{}, repository.ErrNotFound
}

func (s *e2eStore) ReleaseGuestIngest(ctx context.Context, params repository.GuestReleaseIngestParams) error {
	for _, req := range s.guests {
		if req.SenderID == params.SenderID && req.State == repository.GuestStateIngestingSecret {
			req.State = repository.GuestStateAwaitingSecret
			req.IngestLeaseUntil = nil
			s.guests[req.ID] = req
			return nil
		}
	}
	return nil
}

func (s *e2eStore) FinalizeGuest(ctx context.Context, params repository.GuestFinalizeParams) error {
	req, ok := s.guests[params.RequestID]
	if !ok {
		return repository.ErrNotFound
	}
	req.State = repository.GuestStateReady
	req.PayloadKind = params.Kind
	req.SecretReadyAt = &params.Now
	s.guests[req.ID] = req

	if params.Text != nil {
		s.guestTexts[req.ID] = repository.StoredEncryptedPayload{
			ID:                  params.Text.ID,
			EncryptionAlgorithm: params.Text.EncryptionAlgorithm,
			EncryptionKeyID:     params.Text.EncryptionKeyID,
			Nonce:               params.Text.Nonce,
			Ciphertext:          params.Text.Ciphertext,
			CiphertextSHA256:    params.Text.CiphertextSHA256[:],
			ContentType:         params.Text.ContentType,
			PlaintextSize:       params.Text.PlaintextSize,
			RetainUntil:         params.Text.RetainUntil,
		}
	}
	if params.Media != nil {
		s.guestMedias[req.ID] = repository.StoredEncryptedPayload{
			ID:                  params.Media.ID,
			EncryptionAlgorithm: params.Media.EncryptionAlgorithm,
			EncryptionKeyID:     params.Media.EncryptionKeyID,
			Nonce:               params.Media.Nonce,
			Ciphertext:          params.Media.Ciphertext,
			CiphertextSHA256:    params.Media.CiphertextSHA256[:],
			ContentType:         params.Media.ContentType,
			PlaintextSize:       params.Media.PlaintextSize,
			RetainUntil:         params.Media.RetainUntil,
		}
		s.guestBlobs[req.ID] = repository.DeliveryMedia{
			Type:                 params.MediaType,
			TelegramFileID:       params.TelegramFileID,
			TelegramFileUniqueID: params.TelegramFileUnique,
			ContentType:          params.TelegramContent,
			PlaintextSize:        params.Media.PlaintextSize,
		}
	}
	if params.Caption != nil {
		s.guestCaptions[req.ID] = repository.StoredEncryptedPayload{
			ID:                  params.Caption.ID,
			EncryptionAlgorithm: params.Caption.EncryptionAlgorithm,
			EncryptionKeyID:     params.Caption.EncryptionKeyID,
			Nonce:               params.Caption.Nonce,
			Ciphertext:          params.Caption.Ciphertext,
			CiphertextSHA256:    params.Caption.CiphertextSHA256[:],
			ContentType:         params.Caption.ContentType,
			PlaintextSize:       params.Caption.PlaintextSize,
			RetainUntil:         params.Caption.RetainUntil,
		}
	}
	return nil
}

func (s *e2eStore) ClaimGuestOpen(ctx context.Context, params repository.GuestClaimOpenParams) (repository.GuestOpenReservation, error) {
	id, ok := s.guestTokens[string(params.TokenHash)]
	if !ok {
		return repository.GuestOpenReservation{}, repository.ErrNotFound
	}
	req := s.guests[id]
	if req.SenderID == params.User.TelegramUserID {
		return repository.GuestOpenReservation{}, repository.ErrUnauthorized
	}
	if req.TargetUserID != nil && *req.TargetUserID != params.User.TelegramUserID {
		return repository.GuestOpenReservation{}, repository.ErrUnauthorized
	}
	if req.TargetUserID == nil && req.TargetUsername != "" &&
		!strings.EqualFold(strings.TrimPrefix(params.User.Username, "@"), strings.TrimPrefix(req.TargetUsername, "@")) {
		return repository.GuestOpenReservation{}, repository.ErrUnauthorized
	}
	if req.State == repository.GuestStateOpened {
		return repository.GuestOpenReservation{}, repository.ErrAlreadyOpened
	}
	if req.State == repository.GuestStateAwaitingSecret || req.State == repository.GuestStateIngestingSecret {
		return repository.GuestOpenReservation{}, repository.ErrNotActive
	}

	req.State = repository.GuestStateOpening
	req.OpeningReservedAt = &params.Now
	req.OpeningLeaseUntil = &params.LeaseUntil
	s.guests[id] = req

	content := repository.GuestDeliveryContent{Kind: req.PayloadKind}
	if txt, ok := s.guestTexts[req.ID]; ok {
		content.Text = &txt
	}
	if blob, ok := s.guestBlobs[req.ID]; ok {
		content.Media = &blob
	}
	if caption, ok := s.guestCaptions[req.ID]; ok {
		content.Caption = &caption
	}

	return repository.GuestOpenReservation{
		Request: req,
		Content: content,
	}, nil
}

func (s *e2eStore) CompleteGuestOpen(ctx context.Context, params repository.GuestCompleteOpenParams) error {
	req := s.guests[params.RequestID]
	req.State = repository.GuestStateOpened
	s.guests[params.RequestID] = req
	return nil
}

func (s *e2eStore) FailGuestOpen(ctx context.Context, params repository.GuestFailOpenParams) error {
	req := s.guests[params.RequestID]
	req.State = repository.GuestStateReady
	s.guests[params.RequestID] = req
	return nil
}

func (s *e2eStore) MarkGuestEnvelope(ctx context.Context, hash []byte, inlineID string, now time.Time) error {
	id, ok := s.guestTokens[string(hash)]
	if !ok {
		return repository.ErrNotFound
	}
	req := s.guests[id]
	req.InlineMessageID = inlineID
	s.guests[id] = req
	return nil
}

func (s *e2eStore) CancelGuestRequest(ctx context.Context, params repository.CancelGuestParams) (int, error) {
	count := 0
	for id, req := range s.guests {
		if req.SenderID == params.SenderID && req.State != repository.GuestStateOpened && req.State != repository.GuestStateCancelled {
			req.State = repository.GuestStateCancelled
			s.guests[id] = req
			count++
		}
	}
	return count, nil
}

func (s *e2eStore) CancelGuestRequestByID(ctx context.Context, params repository.CancelGuestRequestByIDParams) error {
	req, ok := s.guests[params.RequestID]
	if !ok || req.SenderID != params.SenderID {
		return repository.ErrNotFound
	}
	if req.State != repository.GuestStateAwaitingSecret && req.State != repository.GuestStateReady {
		return repository.ErrNotFound
	}
	req.State = repository.GuestStateCancelled
	s.guests[params.RequestID] = req
	return nil
}

func (s *e2eStore) FindGuestMediaPayload(ctx context.Context, id uuid.UUID) (repository.GuestMediaBlob, error) {
	if _, ok := s.guests[id]; !ok {
		return repository.GuestMediaBlob{}, repository.ErrNotFound
	}
	media, ok := s.guestMedias[id]
	if !ok {
		return repository.GuestMediaBlob{}, repository.ErrNotFound
	}
	blob := s.guestBlobs[id]
	return repository.GuestMediaBlob{
		RequestID: id,
		MediaType: blob.Type,
		Stored:    media,
	}, nil
}

func (s *e2eStore) FindRecentTargetsForSender(ctx context.Context, senderID int64, limit int) ([]domain.RecentTarget, error) {
	if limit <= 0 {
		limit = 3
	}
	var results []domain.RecentTarget
	seen := make(map[string]bool)
	for _, req := range s.guests {
		if req.SenderID == senderID {
			key := "username:" + strings.ToLower(strings.TrimPrefix(req.TargetUsername, "@"))
			if req.TargetUserID != nil {
				key = fmt.Sprintf("user-id:%d", *req.TargetUserID)
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			targetID := int64(0)
			if req.TargetUserID != nil {
				targetID = *req.TargetUserID
			}
			displayName := req.TargetUsername
			if targetID > 0 {
				if u, ok := s.users[targetID]; ok && u.DisplayName() != "" {
					displayName = u.DisplayName()
				}
			}
			if displayName == "" {
				displayName = fmt.Sprintf("User %d", targetID)
			}
			results = append(results, domain.RecentTarget{
				TargetUserID:   targetID,
				TargetUsername: req.TargetUsername,
				DisplayName:    displayName,
				LastUsedAt:     req.CreatedAt,
			})
			if len(results) >= limit {
				break
			}
		}
	}
	return results, nil
}

func TestE2ETextWhisperFullUserJourney(t *testing.T) {
	t.Parallel()
	env := newE2E(t)
	env.observeGroup(env.sender, env.recipient)

	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 12, Chat: env.group, From: &env.sender, Text: "/whisper @bobby_user",
	}})
	if len(env.mock.SentMessages) == 0 {
		t.Fatal("expected bot to reply with private composer instructions")
	}

	const secret = "Top secret information for Bob only"
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 20, Chat: env.private(env.sender), From: &env.sender, Text: secret,
	}})
	callback := env.envelopeCallback()

	env.openCallback("cb_eve_1", env.intruder, callback)
	env.requireCallback("cb_eve_1", true, "")

	env.openCallback("cb_bob_1", env.recipient, callback)
	env.requireEphemeral(env.recipient.ID, secret)
	if env.openedWhisper().Status != domain.WhisperOpened {
		t.Fatalf("whisper status = %v, want opened", env.openedWhisper().Status)
	}

	env.openCallback("cb_bob_2", env.recipient, callback)
	env.requireCallback("cb_bob_2", true, "already delivered")
}

func TestE2EReplyWhisperFullJourney(t *testing.T) {
	t.Parallel()
	env := newE2E(t)
	env.observeGroup(env.sender, env.recipient)

	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 12, Chat: env.group, From: &env.sender, Text: "/whisper",
		ReplyToMessage: &telegram.Message{MessageID: 11, From: &env.recipient},
	}})

	const secret = "reply-targeted secret"
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 20, Chat: env.private(env.sender), From: &env.sender, Text: secret,
	}})
	env.openCallback("cb_bob_reply", env.recipient, env.envelopeCallback())
	env.requireEphemeral(env.recipient.ID, secret)
}

func TestE2ENumericIDGroupWhisper(t *testing.T) {
	t.Parallel()
	env := newE2E(t)
	env.observeGroup(env.sender, env.recipient)

	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 12, Chat: env.group, From: &env.sender, Text: "/whisper 202",
	}})

	const secret = "numeric-id secret"
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 20, Chat: env.private(env.sender), From: &env.sender, Text: secret,
	}})
	env.openCallback("cb_bob_id", env.recipient, env.envelopeCallback())
	env.requireEphemeral(env.recipient.ID, secret)
}

func TestE2ECancelActiveDraft(t *testing.T) {
	t.Parallel()
	env := newE2E(t)
	env.observeGroup(env.sender, env.recipient)

	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 12, Chat: env.group, From: &env.sender, Text: "/whisper @bobby_user",
	}})
	if count, _ := env.store.CountActiveDrafts(env.ctx, env.sender.ID, time.Now()); count != 1 {
		t.Fatalf("expected 1 active draft, got %d", count)
	}

	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 20, Chat: env.private(env.sender), From: &env.sender, Text: "/cancel",
	}})
	if count, _ := env.store.CountActiveDrafts(env.ctx, env.sender.ID, time.Now()); count != 0 {
		t.Fatalf("expected 0 active drafts after /cancel, got %d", count)
	}
}

func TestE2EMediaWhisperTypes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		attach func(*telegram.Message)
		method string
	}{
		{
			name: "photo",
			attach: func(m *telegram.Message) {
				m.Caption = "Confidential diagram"
				m.Photo = []telegram.PhotoSize{{
					FileID: "photo_large", FileUniqueID: "u_photo_large",
					Width: 800, Height: 600, FileSize: 30,
				}}
			},
			method: "sendPhoto",
		},
		{
			name: "voice",
			attach: func(m *telegram.Message) {
				m.Voice = &telegram.Voice{
					FileID: "voice_note", FileUniqueID: "u_voice",
					Duration: 4, MIMEType: "audio/ogg", FileSize: 30,
				}
			},
			method: "sendVoice",
		},
		{
			name: "video",
			attach: func(m *telegram.Message) {
				m.Video = &telegram.Video{
					FileID: "video_secret", FileUniqueID: "u_video",
					Width: 1280, Height: 720, Duration: 8, MIMEType: "video/mp4", FileSize: 30,
				}
			},
			method: "sendVideo",
		},
		{
			name: "audio",
			attach: func(m *telegram.Message) {
				m.Audio = &telegram.Audio{
					FileID: "audio_secret", FileUniqueID: "u_audio",
					Duration: 12, MIMEType: "audio/mpeg", FileSize: 30,
				}
			},
			method: "sendAudio",
		},
		{
			name: "document",
			attach: func(m *telegram.Message) {
				m.Document = &telegram.Document{
					FileID: "doc_secret", FileUniqueID: "u_doc",
					FileName: "secret.pdf", MIMEType: "application/pdf", FileSize: 30,
				}
			},
			method: "sendDocument",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newE2E(t)
			env.observeGroup(env.sender, env.recipient)
			env.handle(telegram.Update{Message: &telegram.Message{
				MessageID: 12, Chat: env.group, From: &env.sender, Text: "/whisper @bobby_user",
			}})

			msg := &telegram.Message{MessageID: 20, Chat: env.private(env.sender), From: &env.sender}
			tc.attach(msg)
			env.handle(telegram.Update{Message: msg})

			env.openCallback("cb_bob_"+tc.name, env.recipient, env.envelopeCallback())
			env.requireMethod(tc.method)
			if env.openedWhisper().Status != domain.WhisperOpened {
				t.Fatalf("whisper status = %v, want opened", env.openedWhisper().Status)
			}
		})
	}
}

func TestE2EExpiredWhisperRefusesOpen(t *testing.T) {
	t.Parallel()
	env := newE2E(t)
	env.observeGroup(env.sender, env.recipient)
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 12, Chat: env.group, From: &env.sender, Text: "/whisper @bobby_user",
	}})
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 20, Chat: env.private(env.sender), From: &env.sender, Text: "soon-expired secret",
	}})
	callback := env.envelopeCallback()

	for id, w := range env.store.whispers {
		w.ExpiresAt = time.Now().Add(-time.Second)
		env.store.whispers[id] = w
	}

	env.openCallback("cb_expired", env.recipient, callback)
	env.requireCallback("cb_expired", true, "expired")
}

func TestE2EPublicationForbiddenNotifiesSender(t *testing.T) {
	t.Parallel()
	env := newE2E(t)
	env.observeGroup(env.sender, env.recipient)
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 12, Chat: env.group, From: &env.sender, Text: "/whisper @bobby_user",
	}})
	env.mock.RejectChat("sendMessage", env.group.ID, 403, "Forbidden: bot was kicked from the group")
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 20, Chat: env.private(env.sender), From: &env.sender, Text: "undeliverable secret",
	}})
	env.requireSent(env.sender.ID, "couldn't post the secret envelope")
}

func TestE2EDeadFileIDFallsBackToUpload(t *testing.T) {
	t.Parallel()
	env := newE2E(t)
	env.observeGroup(env.sender, env.recipient)
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 12, Chat: env.group, From: &env.sender, Text: "/whisper @bobby_user",
	}})
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 20, Chat: env.private(env.sender), From: &env.sender,
		Photo: []telegram.PhotoSize{{
			FileID: "stale_photo", FileUniqueID: "u_stale",
			Width: 640, Height: 480, FileSize: 30,
		}},
	}})

	env.mock.RejectOnce("sendPhoto", 400, "Bad Request: file_id is invalid")
	env.openCallback("cb_bob_fallback", env.recipient, env.envelopeCallback())
	if got := env.methodCount("sendPhoto"); got < 2 {
		t.Fatalf("sendPhoto calls = %d, want file_id attempt plus multipart fallback", got)
	}
	if env.openedWhisper().Status != domain.WhisperOpened {
		t.Fatalf("whisper status = %v, want opened after fallback", env.openedWhisper().Status)
	}
}

func TestE2EInlineInstantTextWhisperFlow(t *testing.T) {
	t.Parallel()
	env := newE2E(t)

	env.handle(telegram.Update{InlineQuery: &telegram.InlineQuery{
		ID: "inline_q0", From: env.sender, Query: "@bobby_user",
	}})
	env.handle(telegram.Update{InlineQuery: &telegram.InlineQuery{
		ID: "inline_q1", From: env.sender, Query: "@bobby_user The secret code is 998877",
	}})
	if len(env.mock.AnsweredInlineQueries) < 2 {
		t.Fatal("expected AnswerInlineQuery to be called for both queries")
	}
	article := env.mock.AnsweredInlineQueries[1].Results[0]
	if !strings.Contains(article.Title, "@bobby_user") {
		t.Fatalf("expected article title to mention target, got %q", article.Title)
	}
	button := env.latestInlineButton()
	if button.Text != "🔓 Open Secret" {
		t.Fatalf("expected button text '🔓 Open Secret', got %q", button.Text)
	}
	startParam := env.latestStartParam()

	env.privateStart(env.intruder, startParam)
	for _, msg := range env.mock.SentMessages {
		if msg.ChatID == env.intruder.ID && strings.Contains(msg.Text, "998877") {
			t.Fatal("intruder was delivered the secret!")
		}
	}

	env.privateStart(env.recipient, startParam)
	env.requireSent(env.recipient.ID, "The secret code is 998877")

	env.privateStart(env.recipient, startParam)
	env.requireSent(env.recipient.ID, "already opened")
}

func TestE2EInlineMediaTwoStepDraftFlow(t *testing.T) {
	t.Parallel()
	env := newE2E(t)

	env.handle(telegram.Update{InlineQuery: &telegram.InlineQuery{
		ID: "inline_q2", From: env.sender, Query: "@bobby_user",
	}})
	button := env.latestInlineButton()
	if button.Text != "➕ Add or open privately" {
		t.Fatalf("expected button text '➕ Add or open privately', got %q", button.Text)
	}
	startParam := env.latestStartParam()

	env.privateStart(env.recipient, startParam)
	env.requireSent(env.recipient.ID, "not added the secret yet")

	env.privateStart(env.sender, startParam)
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 62, Chat: env.private(env.sender), From: &env.sender,
		Caption: "Confidential blueprint 2026",
		Photo: []telegram.PhotoSize{{
			FileID: "blueprint_photo", FileUniqueID: "u_blueprint",
			Width: 1024, Height: 768, FileSize: 30,
		}},
	}})
	env.requireSent(env.sender.ID, "Secret stored privately")

	env.privateStart(env.recipient, startParam)
	env.requireMethod("sendPhoto")
}

func TestE2EGuestUsernameClaimSurvivesRename(t *testing.T) {
	t.Parallel()
	env := newE2E(t)

	env.handle(telegram.Update{InlineQuery: &telegram.InlineQuery{
		ID: "inline_q_bind", From: env.sender, Query: "@bobby_user",
	}})
	startParam := env.latestStartParam()

	env.privateStart(env.recipient, startParam)
	env.requireSent(env.recipient.ID, "not added the secret yet")

	renamed := env.recipient
	renamed.Username = "bob_renamed"
	env.privateStart(env.sender, startParam)
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 70, Chat: env.private(env.sender), From: &env.sender, Text: "bound-after-rename secret",
	}})

	hijacker := env.intruder
	hijacker.Username = "bobby_user"
	env.privateStart(hijacker, startParam)
	for _, msg := range env.mock.SentMessages {
		if msg.ChatID == hijacker.ID && strings.Contains(msg.Text, "bound-after-rename secret") {
			t.Fatal("username hijacker received the secret after the target ID was bound")
		}
	}

	env.privateStart(renamed, startParam)
	env.requireSent(renamed.ID, "bound-after-rename secret")
}

func TestE2EInlineInstantTextWhisperWithQuotes(t *testing.T) {
	t.Parallel()
	env := newE2E(t)

	env.handle(telegram.Update{InlineQuery: &telegram.InlineQuery{
		ID: "inline_q_quotes", From: env.sender, Query: `@bobby_user "Confidential code 4242"`,
	}})
	env.privateStart(env.recipient, env.latestStartParam())

	found := false
	for _, msg := range env.mock.SentMessages {
		if msg.ChatID == env.recipient.ID && msg.Text == "Confidential code 4242" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected Bob to receive decrypted secret without outer quotes")
	}
}

func TestE2EInlineInstantTextWhisperWithNumericID(t *testing.T) {
	t.Parallel()
	env := newE2E(t)

	env.handle(telegram.Update{InlineQuery: &telegram.InlineQuery{
		ID: "inline_q_id", From: env.sender, Query: `[202] secret-for-id`,
	}})
	env.privateStart(env.recipient, env.latestStartParam())
	env.requireSent(env.recipient.ID, "secret-for-id")
}

func TestE2EInlineSenderClicksOwnButton(t *testing.T) {
	t.Parallel()
	env := newE2E(t)

	env.handle(telegram.Update{InlineQuery: &telegram.InlineQuery{
		ID: "inline_q_sender", From: env.sender, Query: `@bobby_user secret-content`,
	}})
	env.privateStart(env.sender, env.latestStartParam())
	env.requireSent(env.sender.ID, "You are the sender of this secret")
}

func TestE2EInlineInstantMultipleWordsSecret(t *testing.T) {
	t.Parallel()
	env := newE2E(t)

	env.handle(telegram.Update{InlineQuery: &telegram.InlineQuery{
		ID: "inline_q_multi", From: env.sender, Query: `@bobby_user secret1 secret2 secret3`,
	}})
	if len(env.mock.AnsweredInlineQueries[0].Results) < 2 {
		t.Fatalf("expected at least 2 inline results (text secret & media option), got %d",
			len(env.mock.AnsweredInlineQueries[0].Results))
	}
	env.privateStart(env.recipient, env.latestStartParam())

	found := false
	for _, msg := range env.mock.SentMessages {
		if msg.ChatID == env.recipient.ID && msg.Text == "secret1 secret2 secret3" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected Bob to receive 'secret1 secret2 secret3'")
	}
}

func TestE2EInlineQuerySecretWithoutTargetUsingRecent(t *testing.T) {
	t.Parallel()
	env := newE2E(t)
	env.recipient = telegram.User{ID: 202, FirstName: "JoeTheBoss", Username: "joetheboss"}

	env.handle(telegram.Update{InlineQuery: &telegram.InlineQuery{
		ID: "inline_q_first", From: env.sender, Query: `@joetheboss initial secret message`,
	}})
	env.handle(telegram.Update{InlineQuery: &telegram.InlineQuery{
		ID: "inline_q_without_target", From: env.sender, Query: `secret-without-target-specified`,
	}})
	result := env.mock.AnsweredInlineQueries[len(env.mock.AnsweredInlineQueries)-1].Results[0]
	if !strings.Contains(result.Title, "joetheboss") && !strings.Contains(result.Title, "JoeTheBoss") {
		t.Fatalf("expected title to suggest recent target joetheboss, got %q", result.Title)
	}
	env.privateStart(env.recipient, env.latestStartParam())
	env.requireSent(env.recipient.ID, "secret-without-target-specified")
}

func TestE2EOwnerMenuAndEphemeralToggle(t *testing.T) {
	t.Parallel()
	env := newE2E(t)

	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 1, Chat: env.private(env.owner), From: &env.owner, Text: "/owner_menu",
	}})
	env.requireSent(env.owner.ID, "Operator Menu")

	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 2, Chat: env.private(env.owner), From: &env.owner, Text: "/owner_ephemeral 1m",
	}})
	if env.svc.GetEphemeralDeleteAfter() != time.Minute {
		t.Fatalf("expected EphemeralDeleteAfter to be 1m, got %s", env.svc.GetEphemeralDeleteAfter())
	}

	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 3, Chat: env.private(env.owner), From: &env.owner, Text: "/owner_ephemeral off",
	}})
	if env.svc.GetEphemeralDeleteAfter() != 0 {
		t.Fatalf("expected EphemeralDeleteAfter to be 0 (disabled), got %s", env.svc.GetEphemeralDeleteAfter())
	}
}

func TestE2ERejectsSelfTargetedWhisper(t *testing.T) {
	t.Parallel()
	env := newE2E(t)
	env.observeGroup(env.sender, env.recipient)
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 12, Chat: env.group, From: &env.sender, Text: "/whisper @alice_user",
	}})
	env.requireSent(env.group.ID, "Choose someone other than yourself")
}

func TestE2ERejectsUnobservedUsername(t *testing.T) {
	t.Parallel()
	env := newE2E(t)
	env.observeGroup(env.sender)
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 12, Chat: env.group, From: &env.sender, Text: "/whisper @stranger_user",
	}})
	env.requireSent(env.group.ID, "I have not observed that user in this group")
}

func TestE2ERejectsAlbumMedia(t *testing.T) {
	t.Parallel()
	env := newE2E(t)
	env.observeGroup(env.sender, env.recipient)
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 12, Chat: env.group, From: &env.sender, Text: "/whisper @bobby_user",
	}})
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 20, Chat: env.private(env.sender), From: &env.sender,
		MediaGroupID: "album-1",
		Photo: []telegram.PhotoSize{{
			FileID: "album_photo", FileUniqueID: "u_album", Width: 800, Height: 600, FileSize: 30,
		}},
	}})
	env.requireSent(env.sender.ID, "Albums are not supported")
}

func TestE2ESecondDraftIsRefused(t *testing.T) {
	t.Parallel()
	env := newE2E(t, func(o *service.Options) { o.MaxActiveDraftsPerUser = 1 })
	env.observeGroup(env.sender, env.recipient, env.intruder)
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 12, Chat: env.group, From: &env.sender, Text: "/whisper @bobby_user",
	}})
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 13, Chat: env.group, From: &env.sender, Text: "/whisper @eve_user",
	}})
	env.requireSent(env.group.ID, "Finish or /cancel your active draft")
}

func TestE2EDraftTakesPrecedenceOverGuestIngest(t *testing.T) {
	t.Parallel()
	env := newE2E(t)
	env.observeGroup(env.sender, env.recipient)

	env.handle(telegram.Update{InlineQuery: &telegram.InlineQuery{
		ID: "inline_guest_pending", From: env.sender, Query: "@bobby_user",
	}})
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 12, Chat: env.group, From: &env.sender, Text: "/whisper @bobby_user",
	}})

	const secret = "draft-wins-over-guest"
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 20, Chat: env.private(env.sender), From: &env.sender, Text: secret,
	}})
	env.openCallback("cb_draft_wins", env.recipient, env.envelopeCallback())
	env.requireEphemeral(env.recipient.ID, secret)
}

func TestE2EKeyRotationStillDecryptsExistingWhisper(t *testing.T) {
	t.Parallel()
	env := newE2E(t)
	env.observeGroup(env.sender, env.recipient)
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 12, Chat: env.group, From: &env.sender, Text: "/whisper @bobby_user",
	}})
	const secret = "rotated-key secret"
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 20, Chat: env.private(env.sender), From: &env.sender, Text: secret,
	}})
	callback := env.envelopeCallback()

	v1 := make([]byte, 32)
	v2 := make([]byte, 32)
	for i := range v1 {
		v1[i] = byte(i*7 + 3)
		v2[i] = byte(i*11 + 5)
	}
	rotated, err := secretcrypto.NewKeyring("v2", map[string][]byte{"v1": v1, "v2": v2})
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	env.replaceService(rotated)

	env.openCallback("cb_rotated", env.recipient, callback)
	env.requireEphemeral(env.recipient.ID, secret)
}

func TestE2EGuestTwoStepTextComposer(t *testing.T) {
	t.Parallel()
	env := newE2E(t)

	env.handle(telegram.Update{InlineQuery: &telegram.InlineQuery{
		ID: "inline_text_draft", From: env.sender, Query: "@bobby_user",
	}})
	startParam := env.latestStartParam()
	env.privateStart(env.sender, startParam)
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 21, Chat: env.private(env.sender), From: &env.sender, Text: "guest composer text",
	}})
	env.requireSent(env.sender.ID, "Secret stored privately")

	env.privateStart(env.recipient, startParam)
	env.requireSent(env.recipient.ID, "guest composer text")
}

func TestE2EGuestCancelDiscardsPendingRequest(t *testing.T) {
	t.Parallel()
	env := newE2E(t)

	env.handle(telegram.Update{InlineQuery: &telegram.InlineQuery{
		ID: "inline_cancel", From: env.sender, Query: "@bobby_user",
	}})
	startParam := env.latestStartParam()
	env.handle(telegram.Update{Message: &telegram.Message{
		MessageID: 22, Chat: env.private(env.sender), From: &env.sender, Text: "/cancel",
	}})
	env.requireSent(env.sender.ID, "Locked secret cancelled")

	env.privateStart(env.recipient, startParam)
	env.requireSent(env.recipient.ID, "invalid or expired")
}

func TestE2EInlineRejectsSelfTarget(t *testing.T) {
	t.Parallel()
	env := newE2E(t)
	env.handle(telegram.Update{InlineQuery: &telegram.InlineQuery{
		ID: "inline_self", From: env.sender, Query: "@alice_user secret-to-self",
	}})
	if len(env.mock.AnsweredInlineQueries) == 0 {
		t.Fatal("expected inline notice")
	}
	title := env.mock.AnsweredInlineQueries[0].Results[0].Title
	if !strings.Contains(title, "Cannot send secret to yourself") {
		t.Fatalf("inline title = %q, want self-target refusal", title)
	}
}

func TestE2EGuestActiveLimitBlocksNewRequest(t *testing.T) {
	t.Parallel()
	env := newE2E(t, func(o *service.Options) { o.MaxActiveGuestRequestsPerUser = 1 })
	env.handle(telegram.Update{InlineQuery: &telegram.InlineQuery{
		ID: "inline_limit_1", From: env.sender, Query: "@bobby_user first secret",
	}})
	env.handle(telegram.Update{InlineQuery: &telegram.InlineQuery{
		ID: "inline_limit_2", From: env.sender, Query: "@eve_user second secret",
	}})
	ans := env.mock.AnsweredInlineQueries[len(env.mock.AnsweredInlineQueries)-1]
	if len(ans.Results) == 0 || !strings.Contains(ans.Results[0].Title, "Active secret limit") {
		t.Fatalf("expected active-limit inline notice, got %#v", ans)
	}
}
