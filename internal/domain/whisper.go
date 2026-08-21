package domain

import (
	"crypto/sha256"
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	DefaultWhisperTTL        = 24 * time.Hour
	DefaultContentRetention  = 30 * 24 * time.Hour
	DefaultMetadataRetention = 30 * 24 * time.Hour
	DefaultOneTime           = true
	DefaultProtectContent    = true
)

type WhisperStatus string

const (
	WhisperActive  WhisperStatus = "active"
	WhisperOpening WhisperStatus = "opening"
	WhisperOpened  WhisperStatus = "opened"
	WhisperExpired WhisperStatus = "expired"
	WhisperRevoked WhisperStatus = "revoked"
)

func (s WhisperStatus) IsValid() bool {
	switch s {
	case WhisperActive, WhisperOpening, WhisperOpened, WhisperExpired, WhisperRevoked:
		return true
	default:
		return false
	}
}

// CanTransitionTo captures legal lifecycle edges. Repository methods remain
// responsible for making those transitions atomically.
func (s WhisperStatus) CanTransitionTo(next WhisperStatus) bool {
	if !s.IsValid() || !next.IsValid() || s == next {
		return false
	}

	switch s {
	case WhisperActive:
		return next == WhisperOpening || next == WhisperExpired || next == WhisperRevoked
	case WhisperOpening:
		return next == WhisperActive || next == WhisperOpened || next == WhisperExpired || next == WhisperRevoked
	default:
		return false
	}
}

type PublishState string

const (
	PublishPending    PublishState = "pending"
	PublishPublishing PublishState = "publishing"
	PublishPublished  PublishState = "published"
	PublishRetryWait  PublishState = "retry_wait"
	PublishFailed     PublishState = "failed"
)

func (s PublishState) IsValid() bool {
	switch s {
	case PublishPending, PublishPublishing, PublishPublished, PublishRetryWait, PublishFailed:
		return true
	default:
		return false
	}
}

func (s PublishState) CanTransitionTo(next PublishState) bool {
	if !s.IsValid() || !next.IsValid() || s == next {
		return false
	}

	switch s {
	case PublishPending:
		return next == PublishPublishing || next == PublishFailed
	case PublishPublishing:
		return next == PublishPublished || next == PublishRetryWait || next == PublishFailed
	case PublishRetryWait:
		return next == PublishPublishing || next == PublishFailed
	case PublishFailed:
		return next == PublishPending
	default:
		return false
	}
}

type Whisper struct {
	ID                     uuid.UUID
	DraftID                uuid.UUID
	OpenTokenHash          []byte
	SenderID               int64
	RecipientID            int64
	SourceChatID           int64
	SourceThreadID         *int64
	Content                ContentReference
	OneTime                bool
	ProtectContent         bool
	Status                 WhisperStatus
	PublishState           PublishState
	PublicMessageID        *int64
	PublishAttemptCount    int
	PublishLeaseUntil      *time.Time
	NextPublishAttemptAt   time.Time
	LastPublishError       string
	OpeningCallbackQueryID string
	OpeningReservedAt      *time.Time
	OpeningLeaseUntil      *time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time
	PublishedAt            *time.Time
	ExpiresAt              time.Time
	OpenedAt               *time.Time
	RevokedAt              *time.Time
	ContentRetainUntil     *time.Time
	MetadataRetainUntil    *time.Time
	ContentDeletedAt       *time.Time
}

// OwnerWhisper is the metadata-only view used by privileged operational
// tooling. Sender and Recipient contain identity labels only; secret text,
// captions, media bytes, and Telegram file handles are never included.
type OwnerWhisper struct {
	Whisper   Whisper
	Sender    User
	Recipient User
}

type NewWhisperParams struct {
	ID                  uuid.UUID
	DraftID             uuid.UUID
	OpenTokenHash       []byte
	SenderID            int64
	RecipientID         int64
	SourceChatID        int64
	SourceThreadID      *int64
	Content             ContentReference
	OneTime             *bool
	ProtectContent      *bool
	CreatedAt           time.Time
	ExpiresAt           time.Time
	ContentRetainUntil  *time.Time
	MetadataRetainUntil *time.Time
}

var (
	ErrInvalidWhisperID         = errors.New("whisper ID must be non-zero")
	ErrInvalidWhisperDraftID    = errors.New("whisper draft ID must be non-zero")
	ErrInvalidOpenTokenHash     = errors.New("open token hash must be a SHA-256 digest")
	ErrInvalidWhisperStatus     = errors.New("invalid whisper status")
	ErrInvalidPublishState      = errors.New("invalid publish state")
	ErrInvalidWhisperExpiry     = errors.New("whisper expiry must be after creation")
	ErrInvalidContentRetention  = errors.New("content retention deadline must be after creation")
	ErrInvalidMetadataRetention = errors.New("metadata retention deadline must be after creation")
	ErrInvalidPublishAttempts   = errors.New("publish attempt count cannot be negative")
	ErrInvalidPublishLease      = errors.New("publishing state requires a publish lease")
	ErrInvalidOpeningLease      = errors.New("opening state requires an opening lease")
	ErrMissingOpenedTime        = errors.New("opened whisper requires an opened timestamp")
	ErrMissingRevokedTime       = errors.New("revoked whisper requires a revoked timestamp")
)

func NewWhisper(p NewWhisperParams) (Whisper, error) {
	createdAt := p.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	expiresAt := p.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = createdAt.Add(DefaultWhisperTTL)
	}

	contentRetainUntil := cloneTime(p.ContentRetainUntil)
	if contentRetainUntil == nil {
		contentRetainUntil = timePointer(createdAt.Add(DefaultContentRetention))
	}
	metadataRetainUntil := cloneTime(p.MetadataRetainUntil)
	if metadataRetainUntil == nil {
		metadataRetainUntil = timePointer(createdAt.Add(DefaultMetadataRetention))
	}

	id := p.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	w := Whisper{
		ID:                   id,
		DraftID:              p.DraftID,
		OpenTokenHash:        append([]byte(nil), p.OpenTokenHash...),
		SenderID:             p.SenderID,
		RecipientID:          p.RecipientID,
		SourceChatID:         p.SourceChatID,
		SourceThreadID:       cloneInt64(p.SourceThreadID),
		Content:              cloneContentReference(p.Content),
		OneTime:              boolDefault(p.OneTime, DefaultOneTime),
		ProtectContent:       boolDefault(p.ProtectContent, DefaultProtectContent),
		Status:               WhisperActive,
		PublishState:         PublishPending,
		NextPublishAttemptAt: createdAt,
		CreatedAt:            createdAt,
		UpdatedAt:            createdAt,
		ExpiresAt:            expiresAt,
		ContentRetainUntil:   contentRetainUntil,
		MetadataRetainUntil:  metadataRetainUntil,
	}
	if err := w.Validate(); err != nil {
		return Whisper{}, err
	}
	return w, nil
}

func (w Whisper) Validate() error {
	if w.ID == uuid.Nil {
		return ErrInvalidWhisperID
	}
	if w.DraftID == uuid.Nil {
		return ErrInvalidWhisperDraftID
	}
	if len(w.OpenTokenHash) != sha256.Size {
		return ErrInvalidOpenTokenHash
	}
	if w.SenderID == 0 {
		return ErrInvalidSender
	}
	if w.RecipientID == 0 || w.RecipientID == w.SenderID {
		return ErrInvalidRecipient
	}
	if w.SourceChatID == 0 {
		return ErrInvalidSourceChat
	}
	if err := w.Content.Validate(); err != nil {
		return err
	}
	if !w.Status.IsValid() {
		return ErrInvalidWhisperStatus
	}
	if !w.PublishState.IsValid() {
		return ErrInvalidPublishState
	}
	if w.PublishAttemptCount < 0 {
		return ErrInvalidPublishAttempts
	}
	if w.NextPublishAttemptAt.IsZero() {
		return ErrInvalidPublishState
	}
	if (w.PublishState == PublishPublishing) != (w.PublishLeaseUntil != nil) {
		return ErrInvalidPublishLease
	}
	if (w.Status == WhisperOpening) != (w.OpeningLeaseUntil != nil) {
		return ErrInvalidOpeningLease
	}
	if w.Status == WhisperOpened && w.OpenedAt == nil {
		return ErrMissingOpenedTime
	}
	if w.Status == WhisperRevoked && w.RevokedAt == nil {
		return ErrMissingRevokedTime
	}
	if w.CreatedAt.IsZero() || !w.ExpiresAt.After(w.CreatedAt) {
		return ErrInvalidWhisperExpiry
	}
	if w.ContentRetainUntil != nil && !w.ContentRetainUntil.After(w.CreatedAt) {
		return ErrInvalidContentRetention
	}
	if w.MetadataRetainUntil != nil && !w.MetadataRetainUntil.After(w.CreatedAt) {
		return ErrInvalidMetadataRetention
	}
	return nil
}

// IsExpired treats the exact expiry instant as expired, even before a cleanup
// worker has persisted the expired state.
func (w Whisper) IsExpired(now time.Time) bool {
	return w.Status == WhisperExpired || !now.Before(w.ExpiresAt)
}

func (w Whisper) CanAttemptOpen(now time.Time) bool {
	return w.Status == WhisperActive && w.PublishState == PublishPublished && !w.IsExpired(now)
}

func (w Whisper) IsParticipant(telegramUserID int64) bool {
	return telegramUserID != 0 && (telegramUserID == w.SenderID || telegramUserID == w.RecipientID)
}

func (w Whisper) CanRecipientOpen(telegramUserID int64, now time.Time) bool {
	return telegramUserID == w.RecipientID && w.CanAttemptOpen(now)
}

// ShouldDeleteContent reports that the configured retention policy is due. A
// nil deadline means an owner explicitly selected indefinite retention.
func (w Whisper) ShouldDeleteContent(now time.Time) bool {
	return w.ContentDeletedAt == nil && w.ContentRetainUntil != nil && !now.Before(*w.ContentRetainUntil)
}

func (w Whisper) ShouldDeleteMetadata(now time.Time) bool {
	return w.MetadataRetainUntil != nil && !now.Before(*w.MetadataRetainUntil)
}

func boolDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	return timePointer(*value)
}

func timePointer(value time.Time) *time.Time {
	return &value
}
