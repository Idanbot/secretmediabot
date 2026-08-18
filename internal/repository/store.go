package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/secretcrypto"
	"gorm.io/gorm"
)

var (
	ErrInvalidInput  = errors.New("repository invalid input")
	ErrUnauthorized  = errors.New("repository unauthorized")
	ErrExpired       = errors.New("repository record expired")
	ErrNotActive     = errors.New("repository record is not active")
	ErrAlreadyOpened = errors.New("whisper already opened")
	ErrLeaseLost     = errors.New("repository lease lost")
	// ErrOpenAmbiguous marks a one-time whisper whose earlier reservation has
	// neither completed nor failed, so the delivery outcome is unknowable.
	// Re-opening would risk a duplicate delivery, so the open fails closed.
	ErrOpenAmbiguous       = errors.New("delivery outcome is ambiguous")
	ErrAmbiguousRecipient  = errors.New("observed username matches multiple users")
	ErrTooManyActiveDrafts = errors.New("too many active drafts")
	ErrWhisperRateLimit    = errors.New("whisper rate limit exceeded")
	// ErrUpdateDead marks an update that exhausted its retry budget. The
	// processor must acknowledge and skip it so the stream keeps flowing.
	ErrUpdateDead = errors.New("update exceeded its retry budget")
	// ErrGuestActiveLimit and ErrGuestRateLimit bound guest/inline request
	// creation, mirroring the draft and whisper quotas.
	ErrGuestActiveLimit = errors.New("too many active guest requests")
	ErrGuestRateLimit   = errors.New("guest request rate limit exceeded")
	// ErrGuestOpeningInProgress marks a guest request whose delivery is
	// currently reserved by another attempt within its lease.
	ErrGuestOpeningInProgress = errors.New("guest delivery already in progress")
)

// Store owns all application persistence. It never enables GORM's automatic
// associations, keeping encrypted child rows out of ordinary queries.
type Store struct {
	db *gorm.DB
}

func NewStore(database *Database) *Store {
	if database == nil {
		return &Store{}
	}
	return &Store{db: database.db}
}

func (s *Store) withContext(ctx context.Context) (*gorm.DB, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("repository store is not initialized")
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidInput)
	}
	return s.db.WithContext(ctx), nil
}

type ObserveMembershipParams struct {
	User   domain.User
	Chat   domain.Chat
	SeenAt time.Time
}

type ObserveChatMemberParams struct {
	ChatID int64
	UserID int64
	SeenAt time.Time
}

type CreateDraftParams struct {
	Draft                  domain.Draft
	ComposeTokenHash       []byte
	SourceCommandMessageID *int64
	Now                    time.Time
	MaxActiveDrafts        int
	RecentWhispersSince    time.Time
	MaxRecentWhispers      int
}

type ClaimDraftIngestParams struct {
	DraftID    uuid.UUID
	SenderID   int64
	Now        time.Time
	LeaseUntil time.Time
}

type ReleaseDraftIngestParams struct {
	DraftID            uuid.UUID
	SenderID           int64
	ExpectedLeaseUntil time.Time
	Now                time.Time
}

type CancelDraftParams struct {
	DraftID  uuid.UUID
	SenderID int64
	Now      time.Time
}

// EncryptedBlobInput is an encrypted row ready for persistence. Payload bytes
// are never retained on a domain Whisper or selected by metadata queries.
type EncryptedBlobInput struct {
	ID            uuid.UUID
	Payload       secretcrypto.EncryptedPayload
	ContentType   string
	PlaintextSize int64
	RetainUntil   time.Time
}

type FinalizeDraftParams struct {
	DraftID              uuid.UUID
	SenderID             int64
	ExpectedLeaseUntil   time.Time
	Whisper              domain.Whisper
	TelegramFileID       string
	TelegramFileUniqueID string
	CallbackToken        *EncryptedBlobInput
	Text                 *EncryptedBlobInput
	Media                *EncryptedBlobInput
	Caption              *EncryptedBlobInput
	Now                  time.Time
}

type ClaimPublishParams struct {
	WhisperID  uuid.UUID
	Now        time.Time
	LeaseUntil time.Time
}

type PublishClaim struct {
	Whisper       domain.Whisper
	CallbackToken StoredEncryptedPayload
}

type MarkPublishedParams struct {
	WhisperID          uuid.UUID
	ExpectedLeaseUntil time.Time
	PublicMessageID    int64
	Now                time.Time
}

type MarkPublishFailedParams struct {
	WhisperID          uuid.UUID
	ExpectedLeaseUntil time.Time
	Now                time.Time
	RetryAt            *time.Time
	ErrorCode          string
	Terminal           bool
}

type StoredEncryptedPayload struct {
	ID                  uuid.UUID
	EncryptionAlgorithm string
	EncryptionKeyID     string
	Nonce               []byte
	Ciphertext          []byte
	CiphertextSHA256    []byte
	ContentType         string
	PlaintextSize       int64
	RetainUntil         time.Time
}

type StoredContent struct {
	Kind    domain.PayloadKind
	Text    *StoredEncryptedPayload
	Media   *StoredEncryptedPayload
	Caption *StoredEncryptedPayload
}

// DeliveryMedia contains only the Telegram resend handle and non-secret blob
// metadata. Recipient opens never read the potentially large ciphertext row.
type DeliveryMedia struct {
	BlobID               uuid.UUID
	Type                 domain.MediaType
	TelegramFileID       string
	TelegramFileUniqueID string
	ContentType          string
	PlaintextSize        int64
}

type DeliveryContent struct {
	Kind    domain.PayloadKind
	Text    *StoredEncryptedPayload
	Media   *DeliveryMedia
	Caption *StoredEncryptedPayload
}

type ReserveOpenParams struct {
	OpenTokenHash   []byte
	TelegramUserID  int64
	CallbackQueryID string
	Now             time.Time
	LeaseUntil      time.Time
}

type OpenReservation struct {
	Whisper domain.Whisper
	EventID int64
	Content DeliveryContent
}

type CompleteOpenParams struct {
	WhisperID          uuid.UUID
	EventID            int64
	CallbackQueryID    string
	TelegramMessageID  *int64
	EphemeralMessageID *int64
	DeleteAt           time.Time
	Now                time.Time
}

type FailOpenParams struct {
	WhisperID       uuid.UUID
	EventID         int64
	CallbackQueryID string
	ErrorCode       string
	Now             time.Time
}

type OwnerListWhispersParams struct {
	OwnerTelegramUserID int64
	Before              *time.Time
	Limit               int
	Offset              int
	Reason              string
}

type OwnerGetWhisperParams struct {
	OwnerTelegramUserID int64
	WhisperID           uuid.UUID
	Reason              string
}

type OwnerDeleteWhisperParams struct {
	OwnerTelegramUserID int64
	WhisperID           uuid.UUID
	Reason              string
	Now                 time.Time
}

type OwnerUpdateRetentionParams struct {
	OwnerTelegramUserID int64
	WhisperID           uuid.UUID
	RetainUntil         time.Time
	Reason              string
	Now                 time.Time
}

type ClaimUpdateParams struct {
	TelegramUpdateID int64
	UpdateType       string
	PayloadSHA256    []byte
	Now              time.Time
	LeaseUntil       time.Time
	// MaxAttempts bounds redelivery retries for a single update. When the
	// stored attempt count reaches it, ClaimUpdate reports ErrUpdateDead.
	// Zero disables the cap.
	MaxAttempts int
}

type UpdateLease struct {
	Acquired    bool
	AlreadyDone bool
	Attempts    int
	LeaseUntil  *time.Time
}

type FinishUpdateParams struct {
	TelegramUpdateID   int64
	ExpectedLeaseUntil time.Time
	Now                time.Time
	ErrorCode          string
}

type EphemeralDeleteJob struct {
	ID                 int64
	ChatID             int64
	RecipientID        int64
	EphemeralMessageID int64
	WhisperID          *uuid.UUID
	DeleteAfter        time.Time
	AttemptCount       int
	LeaseUntil         time.Time
}

type ClaimEphemeralDeleteParams struct {
	Now        time.Time
	LeaseUntil time.Time
}

type FinishEphemeralDeleteParams struct {
	JobID              int64
	ExpectedLeaseUntil time.Time
	Now                time.Time
	NextAttemptAt      time.Time
	ErrorCode          string
}

type CleanupParams struct {
	Now                    time.Time
	ProcessedUpdatesBefore time.Time
	BatchSize              int
	// IdentityRetention bounds how long unseen chat memberships, users, and
	// chats are kept once nothing references them. Zero skips that prune.
	IdentityRetention time.Duration
}

type CleanupResult struct {
	ExpiredDrafts           int64
	ReleasedDraftIngests    int64
	ExpiredWhispers         int64
	ReleasedOpenLeases      int64
	ReleasedPublishLeases   int64
	DeletedWhispers         int64
	DeletedProcessedUpdates int64
	DeletedEphemeralJobs    int64
	DeletedGuestRequests    int64
	DeletedGuestJobs        int64
	DeletedDrafts           int64
	DeletedMembers          int64
	DeletedUsers            int64
	DeletedChats            int64
	// ExpiredDraftSenderIDs and ExpiredGuestSenderIDs identify the senders
	// whose drafts/requests just expired so the worker can notify them.
	ExpiredDraftSenderIDs []int64
	ExpiredGuestSenderIDs []int64
}

func nowOr(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}
