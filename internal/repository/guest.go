package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/secretcrypto"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	GuestStateAwaitingSecret  = "awaiting_secret"
	GuestStateIngestingSecret = "ingesting_secret"
	GuestStateReady           = "ready"
	GuestStateOpening         = "opening"
	GuestStateOpened          = "opened"
	GuestStateExpired         = "expired"
	GuestStateCancelled       = "cancelled"
)

type GuestRequest struct {
	ID                 uuid.UUID
	TokenHash          []byte
	SenderID           int64
	TargetUserID       *int64
	TargetUsername     string
	SourceChatID       *int64
	SourceThreadID     *int64
	SourceMessageID    *int64
	GuestQueryID       string
	InlineQueryID      string
	InlineMessageID    string
	State              string
	PayloadKind        domain.PayloadKind
	MediaType          domain.MediaType
	TelegramFileID     string
	TelegramFileUnique string
	TelegramContent    string
	TargetClaimedAt    *time.Time
	IngestStartedAt    *time.Time
	IngestLeaseUntil   *time.Time
	SecretReadyAt      *time.Time
	OpeningReservedAt  *time.Time
	OpeningLeaseUntil  *time.Time
	OpenedAt           *time.Time
	DeliveryMessageID  *int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
	ExpiresAt          time.Time
	RetentionDeleteAt  time.Time
}

type GuestPayload struct {
	ID                  uuid.UUID
	RequestID           uuid.UUID
	Purpose             string
	EncryptionAlgorithm string
	EncryptionKeyID     string
	Nonce               []byte
	Ciphertext          []byte
	CiphertextSHA256    []byte
	ContentType         string
	PlaintextSize       int64
	RetainUntil         time.Time
}

type GuestDeliveryContent struct {
	Kind    domain.PayloadKind
	Text    *StoredEncryptedPayload
	Media   *DeliveryMedia
	Caption *StoredEncryptedPayload
}

type GuestOpenReservation struct {
	Request GuestRequest
	Content GuestDeliveryContent
}

// GuestMediaBlob pairs a guest request's stored encrypted media payload with
// the request's media metadata for the re-upload fallback path.
type GuestMediaBlob struct {
	RequestID uuid.UUID
	MediaType domain.MediaType
	Stored    StoredEncryptedPayload
}

type GuestPrivateDeleteJob struct {
	ID            int64
	RequestID     *uuid.UUID
	ChatID        int64
	MessageID     int64
	DeleteAfter   time.Time
	AttemptCount  int
	LeaseUntil    time.Time
	NextAttemptAt time.Time
}

type GuestCreateParams struct {
	Request            GuestRequest
	Sender             domain.User
	Chat               *domain.Chat
	TextPayload        *GuestPayload
	Now                time.Time
	// MaxActivePerSender bounds concurrently active requests (awaiting,
	// ingesting, or ready). Zero disables the cap.
	MaxActivePerSender int
	// RecentSince and MaxRecentPerSender bound new-request creation per hour.
	// Zero disables the cap.
	RecentSince        time.Time
	MaxRecentPerSender int
}

type GuestClaimTargetParams struct {
	TokenHash []byte
	User      domain.User
	Now       time.Time
}

type GuestClaimIngestParams struct {
	TokenHash  []byte
	SenderID   int64
	Now        time.Time
	LeaseUntil time.Time
}

type GuestReleaseIngestParams struct {
	RequestID          uuid.UUID
	SenderID           int64
	ExpectedLeaseUntil time.Time
	Now                time.Time
}

type GuestFinalizeParams struct {
	RequestID          uuid.UUID
	SenderID           int64
	ExpectedLeaseUntil time.Time
	Kind               domain.PayloadKind
	MediaType          domain.MediaType
	TelegramFileID     string
	TelegramFileUnique string
	TelegramContent    string
	Text               *GuestPayload
	Media              *GuestPayload
	Caption            *GuestPayload
	Now                time.Time
}

type GuestClaimOpenParams struct {
	TokenHash  []byte
	User       domain.User
	Now        time.Time
	LeaseUntil time.Time
}

type GuestCompleteOpenParams struct {
	RequestID          uuid.UUID
	ExpectedLeaseUntil time.Time
	MessageID          int64
	DeleteAt           time.Time
	Now                time.Time
}

type GuestFailOpenParams struct {
	RequestID          uuid.UUID
	ExpectedLeaseUntil time.Time
	Now                time.Time
}

type ClaimGuestDeleteParams struct {
	Now        time.Time
	LeaseUntil time.Time
}

type FinishGuestDeleteParams struct {
	JobID              int64
	ExpectedLeaseUntil time.Time
	Now                time.Time
	NextAttemptAt      time.Time
	ErrorCode          string
}

type CancelGuestParams struct {
	SenderID int64
	Now      time.Time
}

type guestRequestRow struct {
	ID                 uuid.UUID  `gorm:"column:id"`
	TokenHash          []byte     `gorm:"column:token_hash"`
	SenderID           int64      `gorm:"column:sender_id"`
	TargetUserID       *int64     `gorm:"column:target_user_id"`
	TargetUsername     string     `gorm:"column:target_username"`
	SourceChatID       *int64     `gorm:"column:source_chat_id"`
	SourceThreadID     *int64     `gorm:"column:source_thread_id"`
	SourceMessageID    *int64     `gorm:"column:source_message_id"`
	GuestQueryID       string     `gorm:"column:guest_query_id"`
	InlineQueryID      string     `gorm:"column:inline_query_id"`
	InlineMessageID    string     `gorm:"column:inline_message_id"`
	State              string     `gorm:"column:state"`
	PayloadKind        *string    `gorm:"column:payload_kind"`
	MediaType          *string    `gorm:"column:media_type"`
	TelegramFileID     *string    `gorm:"column:telegram_file_id"`
	TelegramFileUnique *string    `gorm:"column:telegram_file_unique_id"`
	TelegramContent    *string    `gorm:"column:telegram_content_type"`
	TargetClaimedAt    *time.Time `gorm:"column:target_claimed_at"`
	IngestStartedAt    *time.Time `gorm:"column:ingest_started_at"`
	IngestLeaseUntil   *time.Time `gorm:"column:ingest_lease_until"`
	SecretReadyAt      *time.Time `gorm:"column:secret_ready_at"`
	OpeningReservedAt  *time.Time `gorm:"column:opening_reserved_at"`
	OpeningLeaseUntil  *time.Time `gorm:"column:opening_lease_until"`
	OpenedAt           *time.Time `gorm:"column:opened_at"`
	DeliveryMessageID  *int64     `gorm:"column:delivery_message_id"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
	ExpiresAt          time.Time  `gorm:"column:expires_at"`
	RetentionDeleteAt  time.Time  `gorm:"column:retention_delete_at"`
}

func (guestRequestRow) TableName() string { return "guest_secret_requests" }

type guestPayloadRow struct {
	ID                  uuid.UUID `gorm:"column:id"`
	RequestID           uuid.UUID `gorm:"column:request_id"`
	Purpose             string    `gorm:"column:purpose"`
	EncryptionAlgorithm string    `gorm:"column:encryption_algorithm"`
	EncryptionKeyID     string    `gorm:"column:encryption_key_id"`
	Nonce               []byte    `gorm:"column:nonce"`
	Ciphertext          []byte    `gorm:"column:ciphertext"`
	CiphertextSHA256    []byte    `gorm:"column:ciphertext_sha256"`
	ContentType         string    `gorm:"column:content_type"`
	PlaintextSize       int64     `gorm:"column:plaintext_size_bytes"`
	CreatedAt           time.Time `gorm:"column:created_at"`
	RetainUntil         time.Time `gorm:"column:retention_delete_at"`
}

func (guestPayloadRow) TableName() string { return "guest_secret_payloads" }

type guestPrivateDeleteJobRow struct {
	ID            int64      `gorm:"column:id"`
	RequestID     *uuid.UUID `gorm:"column:request_id"`
	ChatID        int64      `gorm:"column:chat_id"`
	MessageID     int64      `gorm:"column:message_id"`
	DeleteAfter   time.Time  `gorm:"column:delete_after"`
	NextAttemptAt time.Time  `gorm:"column:next_attempt_at"`
	AttemptCount  int        `gorm:"column:attempt_count"`
	LeaseUntil    *time.Time `gorm:"column:lease_until"`
	LastError     string     `gorm:"column:last_error"`
	DeletedAt     *time.Time `gorm:"column:deleted_at"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
	UpdatedAt     time.Time  `gorm:"column:updated_at"`
}

func (guestPrivateDeleteJobRow) TableName() string { return "guest_private_delete_jobs" }

// guestAdvisoryLockNamespace separates the guest creation lock from the draft
// lock space while still serializing per sender.
const guestAdvisoryLockNamespace = int64(917_501_000_000_000)

func (s *Store) CreateGuestRequest(ctx context.Context, params GuestCreateParams) (GuestRequest, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return GuestRequest{}, err
	}
	now := nowOr(params.Now)
	request := params.Request
	if request.ID == uuid.Nil || len(request.TokenHash) != sha256.Size || request.SenderID <= 0 ||
		(request.TargetUserID == nil && strings.TrimSpace(request.TargetUsername) == "") ||
		(request.GuestQueryID == "" && request.InlineQueryID == "") || !request.ExpiresAt.After(now) ||
		!request.RetentionDeleteAt.After(now) {
		return GuestRequest{}, fmt.Errorf("%w: invalid guest request", ErrInvalidInput)
	}
	if params.Sender.TelegramUserID != request.SenderID || params.Sender.IsBot {
		return GuestRequest{}, fmt.Errorf("%w: invalid guest sender", ErrInvalidInput)
	}
	if request.TargetUserID != nil && (*request.TargetUserID <= 0 || *request.TargetUserID == request.SenderID) {
		return GuestRequest{}, fmt.Errorf("%w: invalid guest target", ErrInvalidInput)
	}
	if params.Chat != nil && params.Chat.TelegramChatID == 0 {
		return GuestRequest{}, fmt.Errorf("%w: invalid guest source chat", ErrInvalidInput)
	}
	row := guestRequestRowFromDomain(request)
	err = db.Transaction(func(tx *gorm.DB) error {
		// Serialize creation per sender so the active/hourly quotas below are
		// enforced transactionally, mirroring the draft path.
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", guestAdvisoryLockNamespace+request.SenderID).Error; err != nil {
			return err
		}
		var active []guestRequestRow
		if err := tx.Where("sender_id = ? AND state IN (?, ?, ?) AND expires_at > ?", request.SenderID,
			GuestStateAwaitingSecret, GuestStateIngestingSecret, GuestStateReady, now).
			Order("created_at DESC, id DESC").Find(&active).Error; err != nil {
			return err
		}
		for _, existing := range active {
			if existing.State == GuestStateAwaitingSecret && sameGuestTarget(existing, request) {
				// The sender re-typed the same target (inline queries fire on
				// nearly every keystroke). Reuse the pending request instead of
				// creating a row per keystroke; refresh the query IDs so the
				// envelope keeps answering the current query.
				updates := map[string]any{"expires_at": request.ExpiresAt, "updated_at": now}
				if request.GuestQueryID != "" {
					updates["guest_query_id"] = request.GuestQueryID
				}
				if request.InlineQueryID != "" {
					updates["inline_query_id"] = request.InlineQueryID
				}
				if err := tx.Model(&guestRequestRow{}).Where("id = ?", existing.ID).Updates(updates).Error; err != nil {
					return err
				}
				row = existing
				row.ExpiresAt = request.ExpiresAt
				row.UpdatedAt = now
				if request.GuestQueryID != "" {
					row.GuestQueryID = request.GuestQueryID
				}
				if request.InlineQueryID != "" {
					row.InlineQueryID = request.InlineQueryID
				}
				return nil
			}
		}
		for _, existing := range active {
			if existing.State == GuestStateIngestingSecret {
				// A secret is currently being ingested in the private composer:
				// the sender must finish or cancel that active composer draft first.
				return fmt.Errorf("%w: sender %d has an active composer draft", ErrGuestActiveLimit, request.SenderID)
			}
		}
		if params.MaxActivePerSender > 0 && len(active)+1 > params.MaxActivePerSender {
			return fmt.Errorf("%w: sender %d has %d active guest requests", ErrGuestActiveLimit, request.SenderID, len(active))
		}
		if params.MaxRecentPerSender > 0 && !params.RecentSince.IsZero() {
			var recent int64
			if err := tx.Model(&guestRequestRow{}).
				Where("sender_id = ? AND created_at > ?", request.SenderID, params.RecentSince).
				Count(&recent).Error; err != nil {
				return err
			}
			if int(recent) >= params.MaxRecentPerSender {
				return fmt.Errorf("%w: sender %d created %d guest requests since %s",
					ErrGuestRateLimit, request.SenderID, recent, params.RecentSince.Format(time.RFC3339))
			}
		}
		// Pending composer requests awaiting a different target are superseded
		// by the newest one; they never contained a secret.
		if err := tx.Model(&guestRequestRow{}).
			Where("sender_id = ? AND state = ? AND expires_at > ?", request.SenderID, GuestStateAwaitingSecret, now).
			Updates(map[string]any{"state": GuestStateCancelled, "updated_at": now}).Error; err != nil {
			return err
		}
		if err := upsertUser(tx, params.Sender, now); err != nil {
			return err
		}
		if request.TargetUserID != nil {
			if err := upsertUser(tx, domain.User{TelegramUserID: *request.TargetUserID}, now); err != nil {
				return err
			}
		}
		if params.Chat != nil {
			if err := upsertChat(tx, *params.Chat, now); err != nil {
				return err
			}
		}
		if params.TextPayload != nil {
			if err := validateGuestPayload(*params.TextPayload, now); err != nil {
				return err
			}
			payloadRow := guestPayloadRowFromDomain(*params.TextPayload)
			if err := tx.Create(&payloadRow).Error; err != nil {
				return translateError(err)
			}
		}
		return tx.Create(&row).Error
	})
	if err != nil {
		return GuestRequest{}, translateError(err)
	}
	return row.toGuestRequest(), nil
}

func sameGuestTarget(row guestRequestRow, request GuestRequest) bool {
	if request.TargetUserID != nil {
		return row.TargetUserID != nil && *row.TargetUserID == *request.TargetUserID
	}
	return row.TargetUserID == nil && normalizeUsername(request.TargetUsername) == normalizeUsername(row.TargetUsername)
}

func (s *Store) FindGuestRequestByTokenHash(ctx context.Context, tokenHash []byte) (GuestRequest, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return GuestRequest{}, err
	}
	if len(tokenHash) != sha256.Size {
		return GuestRequest{}, fmt.Errorf("%w: guest token hash must be SHA-256", ErrInvalidInput)
	}
	var row guestRequestRow
	if err := db.Where("token_hash = ?", tokenHash).Take(&row).Error; err != nil {
		return GuestRequest{}, translateError(err)
	}
	return row.toGuestRequest(), nil
}

func (s *Store) FindAwaitingGuestSecret(ctx context.Context, senderID int64, now time.Time) (GuestRequest, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return GuestRequest{}, err
	}
	if senderID <= 0 {
		return GuestRequest{}, fmt.Errorf("%w: guest sender must be positive", ErrInvalidInput)
	}
	var row guestRequestRow
	err = db.Where("sender_id = ? AND state = ? AND expires_at > ?", senderID, GuestStateAwaitingSecret, nowOr(now)).
		Order("created_at DESC, id DESC").Take(&row).Error
	if err != nil {
		return GuestRequest{}, translateError(err)
	}
	return row.toGuestRequest(), nil
}

func (s *Store) ClaimGuestTarget(ctx context.Context, params GuestClaimTargetParams) (GuestRequest, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return GuestRequest{}, err
	}
	now := nowOr(params.Now)
	if len(params.TokenHash) != sha256.Size || params.User.TelegramUserID <= 0 || params.User.IsBot {
		return GuestRequest{}, fmt.Errorf("%w: invalid guest target claim", ErrInvalidInput)
	}
	var row guestRequestRow
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("token_hash = ?", params.TokenHash).Take(&row).Error; err != nil {
			return translateError(err)
		}
		if row.State == GuestStateOpened {
			return ErrAlreadyOpened
		}
		if row.ExpiresAt.Before(now) || row.State == GuestStateExpired || row.State == GuestStateCancelled {
			return ErrExpired
		}
		if row.TargetUserID != nil && *row.TargetUserID != params.User.TelegramUserID {
			return ErrUnauthorized
		}
		// Usernames are mutable lookup hints. Once the numeric target ID is
		// bound, it is authoritative: re-verifying the username would lock out
		// a legitimate target who renamed after claiming.
		if row.TargetUserID == nil && row.TargetUsername != "" &&
			normalizeUsername(params.User.Username) != normalizeUsername(row.TargetUsername) {
			return ErrUnauthorized
		}
		if row.SenderID == params.User.TelegramUserID {
			return ErrUnauthorized
		}
		if err := upsertUser(tx, params.User, now); err != nil {
			return err
		}
		updates := map[string]any{"target_user_id": params.User.TelegramUserID, "target_claimed_at": now, "updated_at": now}
		if err := tx.Model(&guestRequestRow{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
			return err
		}
		row.TargetUserID = cloneInt64Pointer(&params.User.TelegramUserID)
		row.TargetClaimedAt = timePointer(now)
		row.UpdatedAt = now
		return nil
	})
	if err != nil {
		return GuestRequest{}, translateError(err)
	}
	return row.toGuestRequest(), nil
}

func (s *Store) ClaimGuestIngest(ctx context.Context, params GuestClaimIngestParams) (GuestRequest, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return GuestRequest{}, err
	}
	now := nowOr(params.Now)
	lease := params.LeaseUntil.UTC()
	if len(params.TokenHash) != sha256.Size || params.SenderID <= 0 || !lease.After(now) {
		return GuestRequest{}, fmt.Errorf("%w: invalid guest ingest claim", ErrInvalidInput)
	}
	var row guestRequestRow
	err = db.Raw(`
        UPDATE guest_secret_requests
        SET state = ?, ingest_started_at = ?, ingest_lease_until = ?, updated_at = ?
        WHERE token_hash = ? AND sender_id = ? AND expires_at > ?
          AND (state = ? OR (state = ? AND ingest_lease_until <= ?))
        RETURNING *`, GuestStateIngestingSecret, now, lease, now, params.TokenHash, params.SenderID, now,
		GuestStateAwaitingSecret, GuestStateIngestingSecret, now).Scan(&row).Error
	if err != nil {
		return GuestRequest{}, translateError(err)
	}
	if row.ID == uuid.Nil {
		return GuestRequest{}, ErrNotFound
	}
	return row.toGuestRequest(), nil
}

func (s *Store) ReleaseGuestIngest(ctx context.Context, params GuestReleaseIngestParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	result := db.Model(&guestRequestRow{}).
		Where("id = ? AND sender_id = ? AND state = ? AND ingest_lease_until = ?", params.RequestID, params.SenderID,
			GuestStateIngestingSecret, params.ExpectedLeaseUntil.UTC()).
		Updates(map[string]any{"state": GuestStateAwaitingSecret, "ingest_lease_until": nil, "updated_at": now})
	if result.Error != nil {
		return translateError(result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) FinalizeGuest(ctx context.Context, params GuestFinalizeParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	if params.RequestID == uuid.Nil || params.SenderID <= 0 || params.ExpectedLeaseUntil.IsZero() ||
		(params.Kind != domain.PayloadText && params.Kind != domain.PayloadMedia) {
		return fmt.Errorf("%w: invalid guest finalization", ErrInvalidInput)
	}
	return translateError(db.Transaction(func(tx *gorm.DB) error {
		var row guestRequestRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", params.RequestID).Take(&row).Error; err != nil {
			return translateError(err)
		}
		if row.SenderID != params.SenderID || row.State != GuestStateIngestingSecret || row.IngestLeaseUntil == nil ||
			!row.IngestLeaseUntil.Equal(params.ExpectedLeaseUntil.UTC()) || !row.ExpiresAt.After(now) {
			return ErrLeaseLost
		}
		if params.Kind == domain.PayloadText {
			if params.Text == nil || params.Media != nil || params.Caption != nil {
				return fmt.Errorf("%w: invalid guest text payload", ErrInvalidInput)
			}
		} else if params.Media == nil || params.Text != nil {
			return fmt.Errorf("%w: invalid guest media payload", ErrInvalidInput)
		}
		for _, payload := range []*GuestPayload{params.Text, params.Media, params.Caption} {
			if payload == nil {
				continue
			}
			if err := validateGuestPayload(*payload, now); err != nil {
				return err
			}
			payloadRow := guestPayloadRowFromDomain(*payload)
			if err := tx.Create(&payloadRow).Error; err != nil {
				return translateError(err)
			}
		}
		updates := map[string]any{
			"state": GuestStateReady, "payload_kind": string(params.Kind), "media_type": nil,
			"telegram_file_id": nil, "telegram_file_unique_id": nil, "telegram_content_type": nil,
			"ingest_lease_until": nil, "secret_ready_at": now, "updated_at": now,
		}
		if params.Kind == domain.PayloadMedia {
			mediaType := string(params.MediaType)
			updates["media_type"] = mediaType
			updates["telegram_file_id"] = params.TelegramFileID
			updates["telegram_file_unique_id"] = params.TelegramFileUnique
			updates["telegram_content_type"] = params.TelegramContent
		}
		return tx.Model(&guestRequestRow{}).Where("id = ? AND state = ? AND ingest_lease_until = ?", params.RequestID,
			GuestStateIngestingSecret, params.ExpectedLeaseUntil.UTC()).Updates(updates).Error
	}))
}

func (s *Store) ClaimGuestOpen(ctx context.Context, params GuestClaimOpenParams) (GuestOpenReservation, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return GuestOpenReservation{}, err
	}
	now := nowOr(params.Now)
	lease := params.LeaseUntil.UTC()
	if len(params.TokenHash) != sha256.Size || params.User.TelegramUserID <= 0 || params.User.IsBot || !lease.After(now) {
		return GuestOpenReservation{}, fmt.Errorf("%w: invalid guest open claim", ErrInvalidInput)
	}
	var reservation GuestOpenReservation
	err = db.Transaction(func(tx *gorm.DB) error {
		var row guestRequestRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("token_hash = ?", params.TokenHash).Take(&row).Error; err != nil {
			return translateError(err)
		}
		if !row.ExpiresAt.After(now) {
			return ErrExpired
		}
		if row.SenderID == params.User.TelegramUserID {
			return ErrUnauthorized
		}
		if row.TargetUserID != nil && *row.TargetUserID != params.User.TelegramUserID {
			return ErrUnauthorized
		}
		// As with target claims: once the numeric target ID is bound the
		// mutable username must not re-gate the open.
		if row.TargetUserID == nil && row.TargetUsername != "" &&
			normalizeUsername(params.User.Username) != normalizeUsername(row.TargetUsername) {
			return ErrUnauthorized
		}
		if row.State == GuestStateOpened {
			return ErrAlreadyOpened
		}
		switch row.State {
		case GuestStateAwaitingSecret, GuestStateIngestingSecret:
			return ErrNotActive
		case GuestStateOpening:
			if row.OpeningLeaseUntil != nil && row.OpeningLeaseUntil.After(now) {
				return ErrGuestOpeningInProgress
			}
			// An expired opening lease means the previous attempt crashed
			// before completing. Take over the reservation immediately
			// instead of waiting for the cleanup sweep.
		case GuestStateReady:
		default:
			return ErrConflict
		}
		if err := upsertUser(tx, params.User, now); err != nil {
			return err
		}
		updates := map[string]any{
			"target_user_id": params.User.TelegramUserID, "target_claimed_at": now,
			"state": GuestStateOpening, "opening_reserved_at": now, "opening_lease_until": lease, "updated_at": now,
		}
		if err := tx.Model(&guestRequestRow{}).
			Where("id = ? AND state IN (?, ?)", row.ID, GuestStateReady, GuestStateOpening).
			Updates(updates).Error; err != nil {
			return translateError(err)
		}
		row.TargetUserID = cloneInt64Pointer(&params.User.TelegramUserID)
		row.TargetClaimedAt = timePointer(now)
		row.State = GuestStateOpening
		row.OpeningReservedAt = timePointer(now)
		row.OpeningLeaseUntil = timePointer(lease)
		content, err := loadGuestDeliveryContent(tx, row)
		if err != nil {
			return err
		}
		reservation = GuestOpenReservation{Request: row.toGuestRequest(), Content: content}
		return nil
	})
	if err != nil {
		return GuestOpenReservation{}, translateError(err)
	}
	return reservation, nil
}

func (s *Store) CompleteGuestOpen(ctx context.Context, params GuestCompleteOpenParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	if params.RequestID == uuid.Nil || params.ExpectedLeaseUntil.IsZero() || params.MessageID <= 0 || !params.DeleteAt.After(now) {
		return fmt.Errorf("%w: invalid guest open completion", ErrInvalidInput)
	}
	return translateError(db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&guestRequestRow{}).
			Where("id = ? AND state = ? AND opening_lease_until = ?", params.RequestID, GuestStateOpening, params.ExpectedLeaseUntil.UTC()).
			Updates(map[string]any{"state": GuestStateOpened, "opening_lease_until": nil, "opened_at": now, "delivery_message_id": params.MessageID, "updated_at": now})
		if result.Error != nil {
			return translateError(result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrLeaseLost
		}
		job := guestPrivateDeleteJobRow{
			RequestID: &params.RequestID, ChatID: 0, MessageID: params.MessageID,
			DeleteAfter: params.DeleteAt.UTC(), NextAttemptAt: params.DeleteAt.UTC(), CreatedAt: now, UpdatedAt: now,
		}
		var request guestRequestRow
		if err := tx.Where("id = ?", params.RequestID).Take(&request).Error; err != nil {
			return translateError(err)
		}
		if request.TargetUserID == nil {
			return fmt.Errorf("%w: guest target was not claimed", ErrConflict)
		}
		job.ChatID = *request.TargetUserID
		return tx.Create(&job).Error
	}))
}

func (s *Store) FailGuestOpen(ctx context.Context, params GuestFailOpenParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	result := db.Model(&guestRequestRow{}).
		Where("id = ? AND state = ? AND opening_lease_until = ?", params.RequestID, GuestStateOpening, params.ExpectedLeaseUntil.UTC()).
		Updates(map[string]any{"state": GuestStateReady, "opening_reserved_at": nil, "opening_lease_until": nil, "updated_at": now})
	if result.Error != nil {
		return translateError(result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) MarkGuestEnvelope(ctx context.Context, tokenHash []byte, inlineMessageID string, now time.Time) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	if len(tokenHash) != sha256.Size || strings.TrimSpace(inlineMessageID) == "" {
		return fmt.Errorf("%w: guest envelope identity is required", ErrInvalidInput)
	}
	result := db.Model(&guestRequestRow{}).Where("token_hash = ?", tokenHash).
		Updates(map[string]any{"inline_message_id": inlineMessageID, "updated_at": nowOr(now)})
	if result.Error != nil {
		return translateError(result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CancelGuestRequest(ctx context.Context, params CancelGuestParams) (int, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return 0, err
	}
	if params.SenderID <= 0 {
		return 0, fmt.Errorf("%w: guest sender must be positive", ErrInvalidInput)
	}
	now := nowOr(params.Now)
	result := db.Model(&guestRequestRow{}).
		Where("sender_id = ? AND state IN (?, ?, ?) AND expires_at > ?", params.SenderID,
			GuestStateAwaitingSecret, GuestStateIngestingSecret, GuestStateReady, now).
		Updates(map[string]any{"state": GuestStateCancelled, "ingest_lease_until": nil, "opening_lease_until": nil, "updated_at": now})
	if result.Error != nil {
		return 0, translateError(result.Error)
	}
	if result.RowsAffected == 0 {
		return 0, ErrNotFound
	}
	// All of a sender's active requests are cancelled together; reporting the
	// count keeps the caller honest about what happened.
	return int(result.RowsAffected), nil
}

func (s *Store) FindGuestMediaPayload(ctx context.Context, requestID uuid.UUID) (GuestMediaBlob, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return GuestMediaBlob{}, err
	}
	if requestID == uuid.Nil {
		return GuestMediaBlob{}, fmt.Errorf("%w: guest request ID is required", ErrInvalidInput)
	}
	var blob GuestMediaBlob
	err = db.Transaction(func(tx *gorm.DB) error {
		var request guestRequestRow
		if err := tx.Select("id", "media_type").
			Where("id = ?", requestID).Take(&request).Error; err != nil {
			return translateError(err)
		}
		if request.MediaType == nil {
			return fmt.Errorf("%w: guest request has no media", ErrConflict)
		}
		var row guestPayloadRow
		if err := tx.Where("request_id = ? AND purpose = 'media'", requestID).Take(&row).Error; err != nil {
			return translateError(err)
		}
		blob = GuestMediaBlob{RequestID: requestID, MediaType: domain.MediaType(*request.MediaType), Stored: row.toStored()}
		return nil
	})
	if err != nil {
		return GuestMediaBlob{}, err
	}
	return blob, nil
}

func (s *Store) ClaimDueGuestDelete(ctx context.Context, params ClaimGuestDeleteParams) (GuestPrivateDeleteJob, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return GuestPrivateDeleteJob{}, err
	}
	now := nowOr(params.Now)
	lease := params.LeaseUntil.UTC()
	if !lease.After(now) {
		return GuestPrivateDeleteJob{}, fmt.Errorf("%w: guest deletion lease must end after now", ErrInvalidInput)
	}
	var row guestPrivateDeleteJobRow
	err = db.Raw(`
        WITH candidate AS (
            SELECT id
            FROM guest_private_delete_jobs
            WHERE deleted_at IS NULL
              AND next_attempt_at <= ?
              AND (lease_until IS NULL OR lease_until <= ?)
            ORDER BY next_attempt_at, id
            FOR UPDATE SKIP LOCKED
            LIMIT 1
        )
        UPDATE guest_private_delete_jobs AS job
        SET lease_until = ?, attempt_count = attempt_count + 1, updated_at = ?
        FROM candidate
        WHERE job.id = candidate.id
        RETURNING job.*`, now, now, lease, now).Scan(&row).Error
	if err != nil {
		return GuestPrivateDeleteJob{}, translateError(err)
	}
	if row.ID == 0 {
		return GuestPrivateDeleteJob{}, ErrNotFound
	}
	return row.toGuestDeleteJob(), nil
}

func (s *Store) MarkGuestDeleted(ctx context.Context, params FinishGuestDeleteParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	terminalCode := safeErrorCode(params.ErrorCode)
	result := db.Model(&guestPrivateDeleteJobRow{}).
		Where("id = ? AND deleted_at IS NULL AND lease_until = ?", params.JobID, params.ExpectedLeaseUntil.UTC()).
		Updates(map[string]any{"deleted_at": now, "lease_until": nil, "last_error": terminalCode, "updated_at": now})
	if result.Error != nil {
		return translateError(result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) RetryGuestDelete(ctx context.Context, params FinishGuestDeleteParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	next := params.NextAttemptAt.UTC()
	if params.JobID <= 0 || params.ExpectedLeaseUntil.IsZero() || !next.After(now) {
		return fmt.Errorf("%w: guest deletion retry requires a future attempt", ErrInvalidInput)
	}
	result := db.Model(&guestPrivateDeleteJobRow{}).
		Where("id = ? AND deleted_at IS NULL AND lease_until = ?", params.JobID, params.ExpectedLeaseUntil.UTC()).
		Updates(map[string]any{"lease_until": nil, "next_attempt_at": next, "last_error": safeErrorCode(params.ErrorCode), "updated_at": now})
	if result.Error != nil {
		return translateError(result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrLeaseLost
	}
	return nil
}

func guestRequestRowFromDomain(request GuestRequest) guestRequestRow {
	var payloadKind, mediaType *string
	if request.PayloadKind != "" {
		value := string(request.PayloadKind)
		payloadKind = &value
	}
	if request.MediaType != "" {
		value := string(request.MediaType)
		mediaType = &value
	}
	return guestRequestRow{
		ID: request.ID, TokenHash: cloneBytes(request.TokenHash), SenderID: request.SenderID,
		TargetUserID: cloneInt64Pointer(request.TargetUserID), TargetUsername: request.TargetUsername,
		SourceChatID: cloneInt64Pointer(request.SourceChatID), SourceThreadID: cloneInt64Pointer(request.SourceThreadID),
		SourceMessageID: cloneInt64Pointer(request.SourceMessageID), GuestQueryID: request.GuestQueryID,
		InlineQueryID: request.InlineQueryID, InlineMessageID: request.InlineMessageID, State: request.State,
		PayloadKind: payloadKind, MediaType: mediaType, TelegramFileID: optionalString(request.TelegramFileID),
		TelegramFileUnique: optionalString(request.TelegramFileUnique), TelegramContent: optionalString(request.TelegramContent),
		TargetClaimedAt: request.TargetClaimedAt, IngestStartedAt: request.IngestStartedAt,
		IngestLeaseUntil: request.IngestLeaseUntil, SecretReadyAt: request.SecretReadyAt,
		OpeningReservedAt: request.OpeningReservedAt, OpeningLeaseUntil: request.OpeningLeaseUntil,
		OpenedAt: request.OpenedAt, DeliveryMessageID: request.DeliveryMessageID, CreatedAt: request.CreatedAt.UTC(),
		UpdatedAt: request.UpdatedAt.UTC(), ExpiresAt: request.ExpiresAt.UTC(), RetentionDeleteAt: request.RetentionDeleteAt.UTC(),
	}
}

func (r guestRequestRow) toGuestRequest() GuestRequest {
	request := GuestRequest{
		ID: r.ID, TokenHash: cloneBytes(r.TokenHash), SenderID: r.SenderID, TargetUserID: cloneInt64Pointer(r.TargetUserID),
		TargetUsername: r.TargetUsername, SourceChatID: cloneInt64Pointer(r.SourceChatID), SourceThreadID: cloneInt64Pointer(r.SourceThreadID),
		SourceMessageID: cloneInt64Pointer(r.SourceMessageID), GuestQueryID: r.GuestQueryID, InlineQueryID: r.InlineQueryID,
		InlineMessageID: r.InlineMessageID, State: r.State, TelegramFileID: stringValue(r.TelegramFileID),
		TelegramFileUnique: stringValue(r.TelegramFileUnique), TelegramContent: stringValue(r.TelegramContent), TargetClaimedAt: cloneTimePointer(r.TargetClaimedAt),
		IngestStartedAt: cloneTimePointer(r.IngestStartedAt), IngestLeaseUntil: cloneTimePointer(r.IngestLeaseUntil), SecretReadyAt: cloneTimePointer(r.SecretReadyAt),
		OpeningReservedAt: cloneTimePointer(r.OpeningReservedAt), OpeningLeaseUntil: cloneTimePointer(r.OpeningLeaseUntil), OpenedAt: cloneTimePointer(r.OpenedAt),
		DeliveryMessageID: cloneInt64Pointer(r.DeliveryMessageID), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, ExpiresAt: r.ExpiresAt, RetentionDeleteAt: r.RetentionDeleteAt,
	}
	if r.PayloadKind != nil {
		request.PayloadKind = domain.PayloadKind(*r.PayloadKind)
	}
	if r.MediaType != nil {
		request.MediaType = domain.MediaType(*r.MediaType)
	}
	return request
}

func guestPayloadRowFromDomain(payload GuestPayload) guestPayloadRow {
	return guestPayloadRow{
		ID: payload.ID, RequestID: payload.RequestID, Purpose: payload.Purpose,
		EncryptionAlgorithm: payload.EncryptionAlgorithm, EncryptionKeyID: payload.EncryptionKeyID,
		Nonce: cloneBytes(payload.Nonce), Ciphertext: cloneBytes(payload.Ciphertext), CiphertextSHA256: cloneBytes(payload.CiphertextSHA256),
		ContentType: payload.ContentType, PlaintextSize: payload.PlaintextSize, CreatedAt: time.Now().UTC(), RetainUntil: payload.RetainUntil.UTC(),
	}
}

func (r guestPayloadRow) toStored() StoredEncryptedPayload {
	var digest [sha256.Size]byte
	copy(digest[:], r.CiphertextSHA256)
	return StoredEncryptedPayload{ID: r.ID, EncryptionAlgorithm: r.EncryptionAlgorithm, EncryptionKeyID: r.EncryptionKeyID,
		Nonce: cloneBytes(r.Nonce), Ciphertext: cloneBytes(r.Ciphertext), CiphertextSHA256: digest[:],
		ContentType: r.ContentType, PlaintextSize: r.PlaintextSize, RetainUntil: r.RetainUntil}
}

func (r guestPrivateDeleteJobRow) toGuestDeleteJob() GuestPrivateDeleteJob {
	leaseUntil := time.Time{}
	if r.LeaseUntil != nil {
		leaseUntil = *r.LeaseUntil
	}
	return GuestPrivateDeleteJob{ID: r.ID, RequestID: cloneUUIDPointer(r.RequestID), ChatID: r.ChatID, MessageID: r.MessageID,
		DeleteAfter: r.DeleteAfter, AttemptCount: r.AttemptCount, LeaseUntil: leaseUntil, NextAttemptAt: r.NextAttemptAt}
}

func validateGuestPayload(payload GuestPayload, now time.Time) error {
	if payload.ID == uuid.Nil || payload.RequestID == uuid.Nil || payload.Purpose == "" || payload.EncryptionKeyID == "" ||
		len(payload.Nonce) != secretcrypto.NonceSize || len(payload.Ciphertext) == 0 || len(payload.CiphertextSHA256) != sha256.Size ||
		payload.PlaintextSize <= 0 || !payload.RetainUntil.After(now) {
		return fmt.Errorf("%w: malformed guest encrypted payload", ErrInvalidInput)
	}
	return nil
}

func loadGuestDeliveryContent(tx *gorm.DB, request guestRequestRow) (GuestDeliveryContent, error) {
	content := GuestDeliveryContent{Kind: domain.PayloadKind(*request.PayloadKind)}
	switch content.Kind {
	case domain.PayloadText:
		var row guestPayloadRow
		if err := tx.Where("request_id = ? AND purpose = 'text'", request.ID).Take(&row).Error; err != nil {
			return GuestDeliveryContent{}, translateError(err)
		}
		stored := row.toStored()
		content.Text = &stored
	case domain.PayloadMedia:
		if request.MediaType == nil || request.TelegramFileID == nil || *request.TelegramFileID == "" {
			return GuestDeliveryContent{}, fmt.Errorf("%w: guest media handle is missing", ErrConflict)
		}
		media := DeliveryMedia{BlobID: request.ID, Type: domain.MediaType(*request.MediaType), TelegramFileID: *request.TelegramFileID,
			TelegramFileUniqueID: stringValue(request.TelegramFileUnique), ContentType: stringValue(request.TelegramContent)}
		content.Media = &media
		var caption guestPayloadRow
		err := tx.Where("request_id = ? AND purpose = 'caption'", request.ID).Take(&caption).Error
		if err == nil {
			stored := caption.toStored()
			content.Caption = &stored
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return GuestDeliveryContent{}, translateError(err)
		}
	default:
		return GuestDeliveryContent{}, fmt.Errorf("%w: unsupported guest payload kind", ErrConflict)
	}
	return content, nil
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
