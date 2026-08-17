package domain

import (
	"crypto/sha256"
	"errors"
	"time"

	"github.com/google/uuid"
)

const DefaultDraftTTL = 10 * time.Minute

type DraftState string

const (
	DraftAwaitingMedia  DraftState = "awaiting_media"
	DraftIngestingMedia DraftState = "ingesting_media"
	DraftCompleted      DraftState = "completed"
	DraftCancelled      DraftState = "cancelled"
	DraftExpired        DraftState = "expired"
)

func (s DraftState) IsValid() bool {
	switch s {
	case DraftAwaitingMedia, DraftIngestingMedia, DraftCompleted, DraftCancelled, DraftExpired:
		return true
	default:
		return false
	}
}

func (s DraftState) CanTransitionTo(next DraftState) bool {
	if !s.IsValid() || !next.IsValid() || s == next {
		return false
	}
	switch s {
	case DraftAwaitingMedia:
		return next == DraftIngestingMedia || next == DraftCancelled || next == DraftExpired
	case DraftIngestingMedia:
		return next == DraftAwaitingMedia || next == DraftCompleted || next == DraftCancelled || next == DraftExpired
	default:
		return false
	}
}

type Draft struct {
	ID                     uuid.UUID
	ComposeTokenHash       []byte
	SenderID               int64
	RecipientID            int64
	SourceChatID           int64
	SourceThreadID         *int64
	SourceReplyMessageID   *int64
	SourceCommandMessageID *int64
	State                  DraftState
	CreatedAt              time.Time
	UpdatedAt              time.Time
	ExpiresAt              time.Time
	IngestStartedAt        *time.Time
	IngestLeaseUntil       *time.Time
	CompletedAt            *time.Time
	CancelledAt            *time.Time
}

type NewDraftParams struct {
	ID                     uuid.UUID
	ComposeTokenHash       []byte
	SenderID               int64
	RecipientID            int64
	SourceChatID           int64
	SourceThreadID         *int64
	SourceReplyMessageID   *int64
	SourceCommandMessageID *int64
	CreatedAt              time.Time
	ExpiresAt              time.Time
}

var (
	ErrInvalidSender           = errors.New("sender ID must be non-zero")
	ErrInvalidRecipient        = errors.New("recipient ID must be non-zero and different from sender ID")
	ErrInvalidSourceChat       = errors.New("source chat ID must be non-zero")
	ErrInvalidDraftExpiry      = errors.New("draft expiry must be after creation")
	ErrInvalidDraftState       = errors.New("invalid draft state")
	ErrInvalidDraftID          = errors.New("draft ID must be non-zero")
	ErrInvalidComposeTokenHash = errors.New("compose token hash must be a SHA-256 digest")
	ErrInvalidIngestLease      = errors.New("ingesting state requires a valid ingestion lease")
	ErrMissingCompletionTime   = errors.New("completed draft requires a completion timestamp")
	ErrMissingCancellationTime = errors.New("cancelled draft requires a cancellation timestamp")
)

func NewDraft(p NewDraftParams) (Draft, error) {
	createdAt := p.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	expiresAt := p.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = createdAt.Add(DefaultDraftTTL)
	}
	id := p.ID
	if id == uuid.Nil {
		id = uuid.New()
	}

	d := Draft{
		ID:                     id,
		ComposeTokenHash:       append([]byte(nil), p.ComposeTokenHash...),
		SenderID:               p.SenderID,
		RecipientID:            p.RecipientID,
		SourceChatID:           p.SourceChatID,
		SourceThreadID:         cloneInt64(p.SourceThreadID),
		SourceReplyMessageID:   cloneInt64(p.SourceReplyMessageID),
		SourceCommandMessageID: cloneInt64(p.SourceCommandMessageID),
		State:                  DraftAwaitingMedia,
		CreatedAt:              createdAt,
		UpdatedAt:              createdAt,
		ExpiresAt:              expiresAt,
	}
	if err := d.Validate(); err != nil {
		return Draft{}, err
	}
	return d, nil
}

func (d Draft) Validate() error {
	if d.ID == uuid.Nil {
		return ErrInvalidDraftID
	}
	if d.SenderID == 0 {
		return ErrInvalidSender
	}
	if d.RecipientID == 0 || d.RecipientID == d.SenderID {
		return ErrInvalidRecipient
	}
	if d.SourceChatID == 0 {
		return ErrInvalidSourceChat
	}
	if len(d.ComposeTokenHash) != sha256.Size {
		return ErrInvalidComposeTokenHash
	}
	if !d.State.IsValid() {
		return ErrInvalidDraftState
	}
	if d.CreatedAt.IsZero() || !d.ExpiresAt.After(d.CreatedAt) {
		return ErrInvalidDraftExpiry
	}
	if (d.State == DraftIngestingMedia) != (d.IngestLeaseUntil != nil) {
		return ErrInvalidIngestLease
	}
	if d.IngestLeaseUntil != nil && (d.IngestStartedAt == nil || !d.IngestLeaseUntil.After(*d.IngestStartedAt)) {
		return ErrInvalidIngestLease
	}
	if d.State == DraftCompleted && d.CompletedAt == nil {
		return ErrMissingCompletionTime
	}
	if d.State == DraftCancelled && d.CancelledAt == nil {
		return ErrMissingCancellationTime
	}
	return nil
}

// IsActive treats the exact expiry instant as expired.
func (d Draft) IsActive(now time.Time) bool {
	return (d.State == DraftAwaitingMedia || d.State == DraftIngestingMedia) && now.Before(d.ExpiresAt)
}

func (d Draft) IsExpired(now time.Time) bool {
	return d.State == DraftExpired || !now.Before(d.ExpiresAt)
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
