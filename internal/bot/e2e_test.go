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

func (s *e2eStore) CreateGuestRequest(ctx context.Context, params repository.GuestCreateParams) (repository.GuestRequest, error) {
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

func TestE2EInlineInstantTextWhisperFlow(t *testing.T) {
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
		MaxActiveDraftsPerUser:         5,
		MaxWhispersPerUserPerHour:      50,
		MaxActiveGuestRequestsPerUser:  25,
		MaxGuestRequestsPerUserPerHour: 100,
		DefaultOneTime:                 true,
		ProtectContent:                 true,
		OwnerIDs:                       []int64{999},
		GuestModeEnabled:               true,
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

	// Step 1: Alice submits inline query: @secretmediabot @bobby_user The secret code is 998877
	if err := handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 1,
		InlineQuery: &telegram.InlineQuery{
			ID:    "inline_q1",
			From:  sender,
			Query: "@bobby_user The secret code is 998877",
		},
	}); err != nil {
		t.Fatalf("HandleUpdate(inline instant text) error = %v", err)
	}

	if len(mockServer.AnsweredInlineQueries) == 0 {
		t.Fatal("expected AnswerInlineQuery to be called")
	}
	inlineAnswer := mockServer.AnsweredInlineQueries[0]
	if len(inlineAnswer.Results) != 1 {
		t.Fatalf("expected 1 inline result article, got %d", len(inlineAnswer.Results))
	}
	article := inlineAnswer.Results[0]
	if !strings.Contains(article.Title, "@bobby_user") {
		t.Fatalf("expected article title to mention target, got %q", article.Title)
	}
	if len(article.ReplyMarkup.InlineKeyboard) == 0 || len(article.ReplyMarkup.InlineKeyboard[0]) == 0 {
		t.Fatalf("expected button in reply markup, got %#v", article.ReplyMarkup)
	}
	button := article.ReplyMarkup.InlineKeyboard[0][0]
	if button.Text != "🔓 Open Secret" {
		t.Fatalf("expected button text '🔓 Open Secret', got %q", button.Text)
	}

	// Extract guest parameter from URL
	paramIndex := strings.Index(button.URL, "?start=")
	if paramIndex == -1 {
		t.Fatalf("button URL does not contain start param: %q", button.URL)
	}
	startParam := button.URL[paramIndex+7:]

	// Step 2: Intruder tries to open secret in private chat
	intruderChat := telegram.Chat{ID: intruder.ID, Type: "private"}
	_ = handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 2,
		Message: &telegram.Message{
			MessageID: 50,
			Chat:      intruderChat,
			From:      &intruder,
			Text:      "/start " + startParam,
		},
	})

	// Verify intruder did not receive secret
	for _, msg := range mockServer.SentMessages {
		if msg.ChatID == intruder.ID && strings.Contains(msg.Text, "998877") {
			t.Fatal("intruder was delivered the secret!")
		}
	}

	// Step 3: Recipient Bob opens secret in private chat
	bobChat := telegram.Chat{ID: recipient.ID, Type: "private"}
	if err := handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 3,
		Message: &telegram.Message{
			MessageID: 51,
			Chat:      bobChat,
			From:      &recipient,
			Text:      "/start " + startParam,
		},
	}); err != nil {
		t.Fatalf("HandleUpdate(Bob open secret) error = %v", err)
	}

	// Verify Bob received the secret plaintext
	deliveredSecret := false
	for _, msg := range mockServer.SentMessages {
		if msg.ChatID == recipient.ID && strings.Contains(msg.Text, "The secret code is 998877") {
			deliveredSecret = true
			break
		}
	}
	if !deliveredSecret {
		t.Fatal("expected Bob to receive secret plaintext message")
	}

	// Step 4: Bob attempts to re-open one-time secret
	if err := handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 4,
		Message: &telegram.Message{
			MessageID: 52,
			Chat:      bobChat,
			From:      &recipient,
			Text:      "/start " + startParam,
		},
	}); err != nil {
		t.Fatalf("HandleUpdate(Bob re-open) error = %v", err)
	}
}

func TestE2EInlineMediaTwoStepDraftFlow(t *testing.T) {
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
		MaxActiveDraftsPerUser:         5,
		MaxWhispersPerUserPerHour:      50,
		MaxActiveGuestRequestsPerUser:  25,
		MaxGuestRequestsPerUserPerHour: 100,
		DefaultOneTime:                 true,
		ProtectContent:                 true,
		OwnerIDs:                       []int64{999},
		GuestModeEnabled:               true,
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

	// Step 1: Alice submits inline query without text: @secretmediabot @bobby_user
	if err := handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 1,
		InlineQuery: &telegram.InlineQuery{
			ID:    "inline_q2",
			From:  sender,
			Query: "@bobby_user",
		},
	}); err != nil {
		t.Fatalf("HandleUpdate(inline draft query) error = %v", err)
	}

	if len(mockServer.AnsweredInlineQueries) == 0 {
		t.Fatal("expected AnswerInlineQuery to be called")
	}
	inlineAnswer := mockServer.AnsweredInlineQueries[0]
	article := inlineAnswer.Results[0]
	button := article.ReplyMarkup.InlineKeyboard[0][0]
	if button.Text != "➕ Add or open privately" {
		t.Fatalf("expected button text '➕ Add or open privately', got %q", button.Text)
	}

	paramIndex := strings.Index(button.URL, "?start=")
	startParam := button.URL[paramIndex+7:]

	// Step 2: Recipient Bob taps button BEFORE Alice adds secret
	bobChat := telegram.Chat{ID: recipient.ID, Type: "private"}
	if err := handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 2,
		Message: &telegram.Message{
			MessageID: 60,
			Chat:      bobChat,
			From:      &recipient,
			Text:      "/start " + startParam,
		},
	}); err != nil {
		t.Fatalf("HandleUpdate(Bob tap before secret added) error = %v", err)
	}

	// Verify Bob received notice that secret is not ready yet
	foundNotReadyNotice := false
	for _, msg := range mockServer.SentMessages {
		if msg.ChatID == recipient.ID && strings.Contains(msg.Text, "not added the secret yet") {
			foundNotReadyNotice = true
			break
		}
	}
	if !foundNotReadyNotice {
		t.Fatal("expected notice that secret has not been added yet")
	}

	// Step 3: Alice taps button in DM to add secret
	aliceChat := telegram.Chat{ID: sender.ID, Type: "private"}
	if err := handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 3,
		Message: &telegram.Message{
			MessageID: 61,
			Chat:      aliceChat,
			From:      &sender,
			Text:      "/start " + startParam,
		},
	}); err != nil {
		t.Fatalf("HandleUpdate(Alice start composer) error = %v", err)
	}

	// Step 4: Alice uploads secret photo in private DM
	if err := handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 4,
		Message: &telegram.Message{
			MessageID: 62,
			Chat:      aliceChat,
			From:      &sender,
			Caption:   "Confidential blueprint 2026",
			Photo: []telegram.PhotoSize{
				{FileID: "blueprint_photo", FileUniqueID: "u_blueprint", Width: 1024, Height: 768, FileSize: 40},
			},
		},
	}); err != nil {
		t.Fatalf("HandleUpdate(Alice upload photo) error = %v", err)
	}

	// Verify Alice received confirmation that secret is stored securely
	foundStoredConfirm := false
	for _, msg := range mockServer.SentMessages {
		if msg.ChatID == sender.ID && strings.Contains(msg.Text, "Secret stored privately") {
			foundStoredConfirm = true
			break
		}
	}
	if !foundStoredConfirm {
		t.Fatal("expected confirmation that secret is stored privately")
	}

	// Step 5: Recipient Bob now taps button to open secret
	if err := handler.HandleUpdate(ctx, telegram.Update{
		UpdateID: 5,
		Message: &telegram.Message{
			MessageID: 63,
			Chat:      bobChat,
			From:      &recipient,
			Text:      "/start " + startParam,
		},
	}); err != nil {
		t.Fatalf("HandleUpdate(Bob open secret after upload) error = %v", err)
	}

	// Verify Bob received the decrypted media
	deliveredMedia := false
	for _, call := range mockServer.RecordedCalls() {
		if call.Method == "sendPhoto" {
			deliveredMedia = true
			break
		}
	}
	if !deliveredMedia {
		t.Fatal("expected Bob to receive secret photo via sendPhoto")
	}
}
