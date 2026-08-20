package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/command"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/secretcrypto"
	"github.com/idan/secretmediabot/internal/token"
)

const GuestPrefix = "guest_"

type GuestStore interface {
	CreateGuestRequest(context.Context, repository.GuestCreateParams) (repository.GuestRequest, error)
	FindGuestRequestByTokenHash(context.Context, []byte) (repository.GuestRequest, error)
	FindAwaitingGuestSecret(context.Context, int64, time.Time) (repository.GuestRequest, error)
	ClaimGuestTarget(context.Context, repository.GuestClaimTargetParams) (repository.GuestRequest, error)
	ClaimGuestIngest(context.Context, repository.GuestClaimIngestParams) (repository.GuestRequest, error)
	ReleaseGuestIngest(context.Context, repository.GuestReleaseIngestParams) error
	FinalizeGuest(context.Context, repository.GuestFinalizeParams) error
	ClaimGuestOpen(context.Context, repository.GuestClaimOpenParams) (repository.GuestOpenReservation, error)
	CompleteGuestOpen(context.Context, repository.GuestCompleteOpenParams) error
	FailGuestOpen(context.Context, repository.GuestFailOpenParams) error
	MarkGuestEnvelope(context.Context, []byte, string, time.Time) error
	CancelGuestRequest(context.Context, repository.CancelGuestParams) (int, error)
	FindGuestMediaPayload(context.Context, uuid.UUID) (repository.GuestMediaBlob, error)
	FindRecentTargetsForSender(context.Context, int64, int) ([]domain.RecentTarget, error)
}

type CreateGuestRequestParams struct {
	Sender          domain.User
	Target          command.Target
	SourceChat      *domain.Chat
	SourceThreadID  *int64
	SourceMessageID *int64
	GuestQueryID    string
	InlineQueryID   string
}

type GuestRole string

const (
	GuestRoleSender GuestRole = "sender"
	GuestRoleTarget GuestRole = "target"
)

type GuestSession struct {
	Request   repository.GuestRequest
	Parameter string
	Role      GuestRole
}

type GuestIngestClaim struct {
	Request    repository.GuestRequest
	LeaseUntil time.Time
}

type GuestPlaintextContent struct {
	Kind    domain.PayloadKind
	Text    []byte
	Media   *repository.DeliveryMedia
	Caption []byte
}

func (c *GuestPlaintextContent) Zero() {
	secretcrypto.Zero(c.Text)
	secretcrypto.Zero(c.Caption)
	c.Text = nil
	c.Caption = nil
}

type GuestDelivery struct {
	Request    repository.GuestRequest
	Content    GuestPlaintextContent
	LeaseUntil time.Time
}

func (s *Service) CreateGuestRequest(ctx context.Context, params CreateGuestRequestParams) (GuestSession, error) {
	if s.guestStore == nil || !s.options.GuestModeEnabled {
		return GuestSession{}, ErrGuestUnavailable
	}
	if params.Sender.TelegramUserID <= 0 || params.Sender.IsBot {
		return GuestSession{}, ErrTargetRequired
	}
	if params.SourceChat != nil && !params.SourceChat.Type.SupportsWhispers() {
		return GuestSession{}, ErrGuestSourceUnsupported
	}
	if params.GuestQueryID == "" && params.InlineQueryID == "" {
		return GuestSession{}, ErrGuestInvalidRequest
	}
	now := s.now()
	guestToken, err := token.Generate()
	if err != nil {
		return GuestSession{}, err
	}
	request := repository.GuestRequest{
		ID: uuid.New(), TokenHash: guestToken.Hash[:], SenderID: params.Sender.TelegramUserID,
		State: repository.GuestStateAwaitingSecret, SourceThreadID: cloneInt64(params.SourceThreadID),
		SourceMessageID: cloneInt64(params.SourceMessageID), GuestQueryID: params.GuestQueryID,
		InlineQueryID: params.InlineQueryID, CreatedAt: now, UpdatedAt: now,
		ExpiresAt: now.Add(s.options.WhisperTTL), RetentionDeleteAt: now.Add(s.options.ContentRetention),
	}
	if params.SourceChat != nil {
		chatID := params.SourceChat.TelegramChatID
		request.SourceChatID = &chatID
	}
	switch params.Target.Kind {
	case command.TargetUserID:
		if params.Target.UserID == params.Sender.TelegramUserID {
			return GuestSession{}, ErrTargetIsSender
		}
		request.TargetUserID = cloneInt64(&params.Target.UserID)
	case command.TargetUsername:
		normalized := strings.ToLower(strings.TrimPrefix(params.Target.Username, "@"))
		// A self-targeted username could never be opened: usernames are
		// unique, so the claim-time sender check would always reject it.
		if normalized == normalizeUsernameHint(params.Sender.Username) {
			return GuestSession{}, ErrTargetIsSender
		}
		request.TargetUsername = normalized
	default:
		return GuestSession{}, ErrTargetRequired
	}
	request, err = s.guestStore.CreateGuestRequest(ctx, repository.GuestCreateParams{
		Request: request, Sender: params.Sender, Chat: params.SourceChat, Now: now,
		MaxActivePerSender: s.options.MaxActiveGuestRequestsPerUser,
		RecentSince:        now.Add(-time.Hour), MaxRecentPerSender: s.options.MaxGuestRequestsPerUserPerHour,
	})
	if err != nil {
		return GuestSession{}, mapGuestRepositoryError(err)
	}
	recent := domain.RecentTarget{LastUsedAt: now}
	switch params.Target.Kind {
	case command.TargetUserID:
		recent.TargetUserID = params.Target.UserID
		recent.DisplayName = fmt.Sprintf("User %d", params.Target.UserID)
	case command.TargetUsername:
		recent.TargetUsername = request.TargetUsername
		recent.DisplayName = "@" + request.TargetUsername
	}
	s.RecordRecentTarget(params.Sender.TelegramUserID, recent)
	return GuestSession{Request: request, Parameter: GuestPrefix + guestToken.Raw}, nil
}

type CreateGuestInlineParams struct {
	Sender        domain.User
	Target        command.Target
	Text          string
	InlineQueryID string
}

func (s *Service) CreateGuestInlineSecret(ctx context.Context, params CreateGuestInlineParams) (GuestSession, error) {
	if s.guestStore == nil || !s.options.GuestModeEnabled {
		return GuestSession{}, ErrGuestUnavailable
	}
	if params.Sender.TelegramUserID <= 0 || params.Sender.IsBot {
		return GuestSession{}, ErrTargetRequired
	}
	text := strings.TrimSpace(params.Text)
	if text == "" {
		return GuestSession{}, ErrUnsupportedContent
	}
	if len([]rune(text)) > MaxSecretTextRunes {
		return GuestSession{}, ErrTextTooLong
	}
	now := s.now()
	guestToken, err := token.Generate()
	if err != nil {
		return GuestSession{}, err
	}
	request := repository.GuestRequest{
		ID: uuid.New(), TokenHash: guestToken.Hash[:], SenderID: params.Sender.TelegramUserID,
		State: repository.GuestStateReady, PayloadKind: domain.PayloadText,
		InlineQueryID: params.InlineQueryID, CreatedAt: now, UpdatedAt: now,
		SecretReadyAt: &now,
		ExpiresAt:     now.Add(s.options.WhisperTTL), RetentionDeleteAt: now.Add(s.options.ContentRetention),
	}
	switch params.Target.Kind {
	case command.TargetUserID:
		if params.Target.UserID == params.Sender.TelegramUserID {
			return GuestSession{}, ErrTargetIsSender
		}
		request.TargetUserID = cloneInt64(&params.Target.UserID)
	case command.TargetUsername:
		normalized := strings.ToLower(strings.TrimPrefix(params.Target.Username, "@"))
		if normalized == normalizeUsernameHint(params.Sender.Username) {
			return GuestSession{}, ErrTargetIsSender
		}
		request.TargetUsername = normalized
	default:
		return GuestSession{}, ErrTargetRequired
	}

	payload, err := s.encryptGuestPayload(secretcrypto.PurposeText, request.ID, []byte(text), "text/plain; charset=utf-8", now.Add(s.options.ContentRetention))
	if err != nil {
		return GuestSession{}, err
	}

	request, err = s.guestStore.CreateGuestRequest(ctx, repository.GuestCreateParams{
		Request: request, Sender: params.Sender, Now: now,
		TextPayload:        &payload,
		MaxActivePerSender: s.options.MaxActiveGuestRequestsPerUser,
		RecentSince:        now.Add(-time.Hour), MaxRecentPerSender: s.options.MaxGuestRequestsPerUserPerHour,
	})
	if err != nil {
		return GuestSession{}, mapGuestRepositoryError(err)
	}
	recent := domain.RecentTarget{LastUsedAt: now}
	switch params.Target.Kind {
	case command.TargetUserID:
		recent.TargetUserID = params.Target.UserID
		recent.DisplayName = fmt.Sprintf("User %d", params.Target.UserID)
	case command.TargetUsername:
		recent.TargetUsername = request.TargetUsername
		recent.DisplayName = "@" + request.TargetUsername
	}
	s.RecordRecentTarget(params.Sender.TelegramUserID, recent)
	return GuestSession{Request: request, Parameter: GuestPrefix + guestToken.Raw}, nil
}

func normalizeUsernameHint(value string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(value), "@"))
}

func (s *Service) MarkGuestEnvelope(ctx context.Context, parameter, inlineMessageID string) error {
	if s.guestStore == nil {
		return ErrGuestUnavailable
	}
	hash, _, err := guestParameterHash(parameter)
	if err != nil {
		return err
	}
	return mapGuestRepositoryError(s.guestStore.MarkGuestEnvelope(ctx, hash, inlineMessageID, s.now()))
}

func (s *Service) CancelGuestRequest(ctx context.Context, senderID int64) (int, error) {
	if s.guestStore == nil {
		return 0, ErrGuestUnavailable
	}
	count, err := s.guestStore.CancelGuestRequest(ctx, repository.CancelGuestParams{SenderID: senderID, Now: s.now()})
	if err != nil {
		return 0, mapGuestRepositoryError(err)
	}
	return count, nil
}

func (s *Service) BeginGuestSession(ctx context.Context, parameter string, actor domain.User) (GuestSession, error) {
	if s.guestStore == nil {
		return GuestSession{}, ErrGuestUnavailable
	}
	hash, raw, err := guestParameterHash(parameter)
	if err != nil {
		return GuestSession{}, err
	}
	request, err := s.guestStore.FindGuestRequestByTokenHash(ctx, hash)
	if err != nil {
		return GuestSession{}, mapGuestRepositoryError(err)
	}
	if !request.ExpiresAt.After(s.now()) || request.State == repository.GuestStateExpired || request.State == repository.GuestStateCancelled {
		return GuestSession{}, ErrGuestExpired
	}
	if actor.TelegramUserID == request.SenderID {
		return GuestSession{Request: request, Parameter: GuestPrefix + raw, Role: GuestRoleSender}, nil
	}
	request, err = s.guestStore.ClaimGuestTarget(ctx, repository.GuestClaimTargetParams{TokenHash: hash, User: actor, Now: s.now()})
	if err != nil {
		return GuestSession{}, mapGuestRepositoryError(err)
	}
	s.RecordRecentTarget(request.SenderID, domain.RecentTarget{
		TargetUserID: actor.TelegramUserID, TargetUsername: request.TargetUsername,
		DisplayName: actor.DisplayName(), LastUsedAt: s.now(),
	})
	return GuestSession{Request: request, Parameter: GuestPrefix + raw, Role: GuestRoleTarget}, nil
}

func (s *Service) ClaimGuestIngestForSender(ctx context.Context, senderID int64) (GuestIngestClaim, error) {
	if s.guestStore == nil {
		return GuestIngestClaim{}, ErrGuestUnavailable
	}
	request, err := s.guestStore.FindAwaitingGuestSecret(ctx, senderID, s.now())
	if err != nil {
		return GuestIngestClaim{}, mapGuestRepositoryError(err)
	}
	now := s.now()
	claimed, err := s.guestStore.ClaimGuestIngest(ctx, repository.GuestClaimIngestParams{
		TokenHash: request.TokenHash, SenderID: senderID, Now: now, LeaseUntil: now.Add(s.options.IngestLease),
	})
	if err != nil {
		return GuestIngestClaim{}, mapGuestRepositoryError(err)
	}
	if claimed.IngestLeaseUntil == nil {
		return GuestIngestClaim{}, ErrGuestUnavailable
	}
	return GuestIngestClaim{Request: claimed, LeaseUntil: *claimed.IngestLeaseUntil}, nil
}

func (s *Service) ReleaseGuestIngest(ctx context.Context, claim GuestIngestClaim) error {
	return mapGuestRepositoryError(s.guestStore.ReleaseGuestIngest(ctx, repository.GuestReleaseIngestParams{
		RequestID: claim.Request.ID, SenderID: claim.Request.SenderID, ExpectedLeaseUntil: claim.LeaseUntil, Now: s.now(),
	}))
}

func (s *Service) FinalizeGuestText(ctx context.Context, claim GuestIngestClaim, text string) (repository.GuestRequest, error) {
	if strings.TrimSpace(text) == "" {
		return repository.GuestRequest{}, ErrUnsupportedContent
	}
	if len([]rune(text)) > MaxSecretTextRunes {
		return repository.GuestRequest{}, ErrTextTooLong
	}
	plaintext := []byte(text)
	defer secretcrypto.Zero(plaintext)
	now := s.now()
	payload, err := s.encryptGuestPayload(secretcrypto.PurposeText, claim.Request.ID, plaintext, "text/plain; charset=utf-8", now.Add(s.options.ContentRetention))
	if err != nil {
		return repository.GuestRequest{}, err
	}
	err = s.guestStore.FinalizeGuest(ctx, repository.GuestFinalizeParams{
		RequestID: claim.Request.ID, SenderID: claim.Request.SenderID, ExpectedLeaseUntil: claim.LeaseUntil,
		Kind: domain.PayloadText, Text: &payload, Now: now,
	})
	if err != nil {
		return repository.GuestRequest{}, mapGuestRepositoryError(err)
	}
	return s.guestStore.FindGuestRequestByTokenHash(ctx, claim.Request.TokenHash)
}

func (s *Service) FinalizeGuestMedia(ctx context.Context, claim GuestIngestClaim, media domain.MediaReference, mediaBytes []byte, caption string) (repository.GuestRequest, error) {
	defer secretcrypto.Zero(mediaBytes)
	if media.Provider != domain.MediaProviderTelegram {
		return repository.GuestRequest{}, ErrUnsupportedContent
	}
	if err := media.Validate(); err != nil {
		return repository.GuestRequest{}, fmt.Errorf("%w: %v", ErrUnsupportedContent, err)
	}
	if len(mediaBytes) == 0 || int64(len(mediaBytes)) > s.options.MaxMediaBytes || media.SizeBytes > s.options.MaxMediaBytes {
		return repository.GuestRequest{}, ErrContentTooLarge
	}
	if len([]rune(caption)) > MaxCaptionRunes {
		return repository.GuestRequest{}, ErrCaptionTooLong
	}
	now := s.now()
	retention := now.Add(s.options.ContentRetention)
	mediaPayload, err := s.encryptGuestPayload(secretcrypto.PurposeMedia, claim.Request.ID, mediaBytes, media.ContentType, retention)
	if err != nil {
		return repository.GuestRequest{}, err
	}
	var captionPayload *repository.GuestPayload
	if caption != "" {
		captionBytes := []byte(caption)
		defer secretcrypto.Zero(captionBytes)
		payload, encryptErr := s.encryptGuestPayload(secretcrypto.PurposeCaption, claim.Request.ID, captionBytes, "text/plain; charset=utf-8", retention)
		if encryptErr != nil {
			return repository.GuestRequest{}, encryptErr
		}
		captionPayload = &payload
	}
	err = s.guestStore.FinalizeGuest(ctx, repository.GuestFinalizeParams{
		RequestID: claim.Request.ID, SenderID: claim.Request.SenderID, ExpectedLeaseUntil: claim.LeaseUntil,
		Kind: domain.PayloadMedia, MediaType: media.Type, TelegramFileID: media.Ref,
		TelegramFileUnique: media.UniqueRef, TelegramContent: media.ContentType, Media: &mediaPayload,
		Caption: captionPayload, Now: now,
	})
	if err != nil {
		return repository.GuestRequest{}, mapGuestRepositoryError(err)
	}
	return s.guestStore.FindGuestRequestByTokenHash(ctx, claim.Request.TokenHash)
}

func (s *Service) ReserveGuestOpen(ctx context.Context, parameter string, actor domain.User) (GuestDelivery, error) {
	if s.guestStore == nil {
		return GuestDelivery{}, ErrGuestUnavailable
	}
	hash, _, err := guestParameterHash(parameter)
	if err != nil {
		return GuestDelivery{}, err
	}
	now := s.now()
	reservation, err := s.guestStore.ClaimGuestOpen(ctx, repository.GuestClaimOpenParams{
		TokenHash: hash, User: actor, Now: now, LeaseUntil: now.Add(s.options.OpenLease),
	})
	if err != nil {
		return GuestDelivery{}, mapGuestRepositoryError(err)
	}
	if reservation.Request.OpeningLeaseUntil == nil {
		return GuestDelivery{}, ErrGuestUnavailable
	}
	content := GuestPlaintextContent{Kind: reservation.Content.Kind, Media: reservation.Content.Media}
	if reservation.Content.Text != nil {
		content.Text, err = s.decryptGuestStored(secretcrypto.PurposeText, reservation.Request.ID, *reservation.Content.Text)
		if err != nil {
			_ = s.FailGuestOpen(ctx, GuestDelivery{Request: reservation.Request, LeaseUntil: *reservation.Request.OpeningLeaseUntil})
			return GuestDelivery{}, err
		}
	}
	if reservation.Content.Caption != nil {
		content.Caption, err = s.decryptGuestStored(secretcrypto.PurposeCaption, reservation.Request.ID, *reservation.Content.Caption)
		if err != nil {
			content.Zero()
			_ = s.FailGuestOpen(ctx, GuestDelivery{Request: reservation.Request, LeaseUntil: *reservation.Request.OpeningLeaseUntil})
			return GuestDelivery{}, err
		}
	}
	return GuestDelivery{Request: reservation.Request, Content: content, LeaseUntil: *reservation.Request.OpeningLeaseUntil}, nil
}

// GuestMediaFallback decrypts the stored media payload for a guest request so
// delivery can re-upload the bytes when Telegram permanently rejects the
// stored file_id. The caller must zero the returned buffer.
func (s *Service) GuestMediaFallback(ctx context.Context, requestID uuid.UUID) ([]byte, domain.MediaType, string, error) {
	if s.guestStore == nil {
		return nil, "", "", ErrGuestUnavailable
	}
	blob, err := s.guestStore.FindGuestMediaPayload(ctx, requestID)
	if err != nil {
		return nil, "", "", mapGuestRepositoryError(err)
	}
	plaintext, err := s.decryptGuestStored(secretcrypto.PurposeMedia, requestID, blob.Stored)
	if err != nil {
		return nil, "", "", err
	}
	return plaintext, blob.MediaType, blob.Stored.ContentType, nil
}

func (s *Service) CompleteGuestOpen(ctx context.Context, delivery GuestDelivery, messageID int64) error {
	deleteAfter := s.GetEphemeralDeleteAfter()
	var deleteAt time.Time
	if deleteAfter > 0 {
		deleteAt = s.now().Add(deleteAfter)
	}
	return mapGuestRepositoryError(s.guestStore.CompleteGuestOpen(ctx, repository.GuestCompleteOpenParams{
		RequestID: delivery.Request.ID, ExpectedLeaseUntil: delivery.LeaseUntil, MessageID: messageID,
		DeleteAt: deleteAt, Now: s.now(),
	}))
}

func (s *Service) FailGuestOpen(ctx context.Context, delivery GuestDelivery) error {
	return mapGuestRepositoryError(s.guestStore.FailGuestOpen(ctx, repository.GuestFailOpenParams{
		RequestID: delivery.Request.ID, ExpectedLeaseUntil: delivery.LeaseUntil, Now: s.now(),
	}))
}

func (s *Service) encryptGuestPayload(purpose secretcrypto.RecordPurpose, requestID uuid.UUID, plaintext []byte, contentType string, retainUntil time.Time) (repository.GuestPayload, error) {
	payloadID := uuid.New()
	aad, err := secretcrypto.AssociatedData(purpose, payloadID, requestID)
	if err != nil {
		return repository.GuestPayload{}, err
	}
	encrypted, err := s.cipher.Encrypt(plaintext, aad)
	if err != nil {
		return repository.GuestPayload{}, err
	}
	return repository.GuestPayload{
		ID: payloadID, RequestID: requestID, Purpose: string(purpose), EncryptionAlgorithm: "AES-256-GCM",
		EncryptionKeyID: encrypted.KeyID, Nonce: encrypted.Nonce, Ciphertext: encrypted.Ciphertext,
		CiphertextSHA256: encrypted.CiphertextSHA256[:], ContentType: contentType, PlaintextSize: int64(len(plaintext)), RetainUntil: retainUntil,
	}, nil
}

func (s *Service) decryptGuestStored(purpose secretcrypto.RecordPurpose, requestID uuid.UUID, payload repository.StoredEncryptedPayload) ([]byte, error) {
	if payload.EncryptionAlgorithm != "AES-256-GCM" || payload.PlaintextSize <= 0 || len(payload.CiphertextSHA256) != sha256.Size {
		return nil, ErrCorruptCiphertext
	}
	digest := sha256.Sum256(payload.Ciphertext)
	if !bytesEqual(digest[:], payload.CiphertextSHA256) {
		return nil, ErrCorruptCiphertext
	}
	aad, err := secretcrypto.AssociatedData(purpose, payload.ID, requestID)
	if err != nil {
		return nil, err
	}
	plaintext, err := s.cipher.Decrypt(payload.EncryptionKeyID, payload.Nonce, payload.Ciphertext, aad)
	if err != nil {
		return nil, err
	}
	if int64(len(plaintext)) != payload.PlaintextSize {
		secretcrypto.Zero(plaintext)
		return nil, ErrCorruptCiphertext
	}
	return plaintext, nil
}

func guestParameterHash(parameter string) ([]byte, string, error) {
	parameter = strings.TrimSpace(parameter)
	if !strings.HasPrefix(parameter, GuestPrefix) {
		return nil, "", ErrInvalidOpenToken
	}
	raw := strings.TrimPrefix(parameter, GuestPrefix)
	hash, err := parseRawTokenHash(raw)
	if err != nil {
		return nil, "", ErrInvalidOpenToken
	}
	return hash, raw, nil
}

func mapGuestRepositoryError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, repository.ErrNotFound):
		return ErrGuestNotFound
	case errors.Is(err, repository.ErrUnauthorized):
		return ErrGuestWrongRecipient
	case errors.Is(err, repository.ErrExpired):
		return ErrGuestExpired
	case errors.Is(err, repository.ErrAlreadyOpened):
		return ErrGuestAlreadyOpened
	case errors.Is(err, repository.ErrNotActive):
		return ErrGuestSecretNotReady
	case errors.Is(err, repository.ErrConflict), errors.Is(err, repository.ErrLeaseLost):
		return ErrGuestSecretNotReady
	case errors.Is(err, repository.ErrGuestActiveLimit):
		return ErrGuestActiveLimit
	case errors.Is(err, repository.ErrGuestRateLimit):
		return ErrGuestRateLimit
	case errors.Is(err, repository.ErrGuestOpeningInProgress):
		return ErrGuestOpeningInProgress
	default:
		return err
	}
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var diff byte
	for index := range left {
		diff |= left[index] ^ right[index]
	}
	return diff == 0
}

var (
	ErrGuestUnavailable       = errors.New("guest delivery is unavailable")
	ErrGuestSourceUnsupported = errors.New("guest delivery requires a group or supergroup source")
	ErrGuestInvalidRequest    = errors.New("invalid guest delivery request")
	ErrGuestNotFound          = errors.New("guest request not found")
	ErrGuestExpired           = errors.New("guest request expired")
	ErrGuestWrongRecipient    = errors.New("guest request belongs to another recipient")
	ErrGuestAlreadyOpened     = errors.New("guest secret was already opened")
	ErrGuestSecretNotReady    = errors.New("guest secret is not ready")
	ErrGuestActiveLimit       = errors.New("too many active guest requests")
	ErrGuestRateLimit         = errors.New("guest request rate limit exceeded")
	ErrGuestOpeningInProgress = errors.New("guest delivery already in progress")
)
