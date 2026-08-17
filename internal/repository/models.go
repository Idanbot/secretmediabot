package repository

import (
	"time"

	"github.com/google/uuid"
)

type userRow struct {
	TelegramUserID        int64     `gorm:"column:telegram_user_id;primaryKey"`
	Username              string    `gorm:"column:username"`
	UsernameNormalized    *string   `gorm:"column:username_normalized;->"`
	FirstName             string    `gorm:"column:first_name"`
	LastName              string    `gorm:"column:last_name"`
	IsBot                 bool      `gorm:"column:is_bot"`
	LanguageCode          string    `gorm:"column:language_code"`
	HasStartedPrivateChat bool      `gorm:"column:has_started_private_chat"`
	FirstSeenAt           time.Time `gorm:"column:first_seen_at"`
	LastSeenAt            time.Time `gorm:"column:last_seen_at"`
	UpdatedAt             time.Time `gorm:"column:updated_at"`
}

func (userRow) TableName() string { return "users" }

type chatRow struct {
	TelegramChatID int64     `gorm:"column:telegram_chat_id;primaryKey"`
	ChatType       string    `gorm:"column:chat_type"`
	Title          string    `gorm:"column:title"`
	Username       string    `gorm:"column:username"`
	FirstSeenAt    time.Time `gorm:"column:first_seen_at"`
	LastSeenAt     time.Time `gorm:"column:last_seen_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
}

func (chatRow) TableName() string { return "chats" }

type observedChatMemberRow struct {
	ChatID      int64     `gorm:"column:chat_id;primaryKey"`
	UserID      int64     `gorm:"column:user_id;primaryKey"`
	FirstSeenAt time.Time `gorm:"column:first_seen_at"`
	LastSeenAt  time.Time `gorm:"column:last_seen_at"`
}

func (observedChatMemberRow) TableName() string { return "observed_chat_members" }

type draftRow struct {
	ID                     uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	ComposeTokenHash       []byte     `gorm:"column:compose_token_hash"`
	SenderID               int64      `gorm:"column:sender_id"`
	RecipientID            int64      `gorm:"column:recipient_id"`
	SourceChatID           int64      `gorm:"column:source_chat_id"`
	SourceThreadID         *int64     `gorm:"column:source_thread_id"`
	SourceReplyMessageID   *int64     `gorm:"column:source_reply_message_id"`
	SourceCommandMessageID *int64     `gorm:"column:source_command_message_id"`
	State                  string     `gorm:"column:state"`
	CreatedAt              time.Time  `gorm:"column:created_at"`
	UpdatedAt              time.Time  `gorm:"column:updated_at"`
	ExpiresAt              time.Time  `gorm:"column:expires_at"`
	IngestStartedAt        *time.Time `gorm:"column:ingest_started_at"`
	IngestLeaseUntil       *time.Time `gorm:"column:ingest_lease_until"`
	CompletedAt            *time.Time `gorm:"column:completed_at"`
	CancelledAt            *time.Time `gorm:"column:cancelled_at"`
}

func (draftRow) TableName() string { return "whisper_drafts" }

type whisperRow struct {
	ID                     uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	DraftID                uuid.UUID  `gorm:"column:draft_id"`
	OpenTokenHash          []byte     `gorm:"column:open_token_hash"`
	SenderID               int64      `gorm:"column:sender_id"`
	RecipientID            int64      `gorm:"column:recipient_id"`
	SourceChatID           int64      `gorm:"column:source_chat_id"`
	SourceThreadID         *int64     `gorm:"column:source_thread_id"`
	PayloadKind            string     `gorm:"column:payload_kind"`
	MediaProvider          *string    `gorm:"column:media_provider"`
	MediaType              *string    `gorm:"column:media_type"`
	TelegramFileID         *string    `gorm:"column:telegram_file_id"`
	TelegramFileUniqueID   *string    `gorm:"column:telegram_file_unique_id"`
	OneTime                bool       `gorm:"column:one_time"`
	ProtectContent         bool       `gorm:"column:protect_content"`
	Status                 string     `gorm:"column:status"`
	PublishState           string     `gorm:"column:publish_state"`
	PublishAttemptCount    int        `gorm:"column:publish_attempt_count"`
	NextPublishAttemptAt   time.Time  `gorm:"column:next_publish_attempt_at"`
	PublishLeaseUntil      *time.Time `gorm:"column:publish_lease_until"`
	LastPublishError       *string    `gorm:"column:last_publish_error"`
	PublicMessageID        *int64     `gorm:"column:public_message_id"`
	PublishedAt            *time.Time `gorm:"column:published_at"`
	OpeningCallbackQueryID *string    `gorm:"column:opening_callback_query_id"`
	OpeningReservedAt      *time.Time `gorm:"column:opening_reserved_at"`
	OpeningLeaseUntil      *time.Time `gorm:"column:opening_lease_until"`
	OpenedAt               *time.Time `gorm:"column:opened_at"`
	RevokedAt              *time.Time `gorm:"column:revoked_at"`
	CreatedAt              time.Time  `gorm:"column:created_at"`
	UpdatedAt              time.Time  `gorm:"column:updated_at"`
	ExpiresAt              time.Time  `gorm:"column:expires_at"`
	RetentionDeleteAt      time.Time  `gorm:"column:retention_delete_at"`
}

// WhisperRow is exported only so GORM can populate it when embedded in an
// internal projection. Repository callers still receive domain/DTO values.
type WhisperRow = whisperRow

func (whisperRow) TableName() string { return "whispers" }

type mediaBlobRow struct {
	ID                  uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	WhisperID           uuid.UUID `gorm:"column:whisper_id"`
	EncryptionAlgorithm string    `gorm:"column:encryption_algorithm"`
	EncryptionKeyID     string    `gorm:"column:encryption_key_id"`
	Nonce               []byte    `gorm:"column:nonce"`
	Ciphertext          []byte    `gorm:"column:ciphertext"`
	CiphertextSHA256    []byte    `gorm:"column:ciphertext_sha256"`
	ContentType         string    `gorm:"column:content_type"`
	PlaintextSizeBytes  int64     `gorm:"column:plaintext_size_bytes"`
	CreatedAt           time.Time `gorm:"column:created_at"`
	RetentionDeleteAt   time.Time `gorm:"column:retention_delete_at"`
}

func (mediaBlobRow) TableName() string { return "media_blobs" }

type encryptedTextPayloadRow struct {
	ID                  uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	WhisperID           uuid.UUID `gorm:"column:whisper_id"`
	Purpose             string    `gorm:"column:purpose"`
	EncryptionAlgorithm string    `gorm:"column:encryption_algorithm"`
	EncryptionKeyID     string    `gorm:"column:encryption_key_id"`
	Nonce               []byte    `gorm:"column:nonce"`
	Ciphertext          []byte    `gorm:"column:ciphertext"`
	CiphertextSHA256    []byte    `gorm:"column:ciphertext_sha256"`
	PlaintextSizeBytes  int64     `gorm:"column:plaintext_size_bytes"`
	CreatedAt           time.Time `gorm:"column:created_at"`
	RetentionDeleteAt   time.Time `gorm:"column:retention_delete_at"`
}

func (encryptedTextPayloadRow) TableName() string { return "encrypted_text_payloads" }

type encryptedCallbackTokenRow struct {
	ID                  uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	WhisperID           uuid.UUID `gorm:"column:whisper_id"`
	EncryptionAlgorithm string    `gorm:"column:encryption_algorithm"`
	EncryptionKeyID     string    `gorm:"column:encryption_key_id"`
	Nonce               []byte    `gorm:"column:nonce"`
	Ciphertext          []byte    `gorm:"column:ciphertext"`
	CiphertextSHA256    []byte    `gorm:"column:ciphertext_sha256"`
	PlaintextSizeBytes  int64     `gorm:"column:plaintext_size_bytes"`
	CreatedAt           time.Time `gorm:"column:created_at"`
}

func (encryptedCallbackTokenRow) TableName() string { return "encrypted_callback_tokens" }

type openEventRow struct {
	ID                int64      `gorm:"column:id;primaryKey"`
	WhisperID         uuid.UUID  `gorm:"column:whisper_id"`
	TelegramUserID    int64      `gorm:"column:telegram_user_id"`
	CallbackQueryID   *string    `gorm:"column:callback_query_id"`
	Outcome           string     `gorm:"column:outcome"`
	Allowed           bool       `gorm:"column:allowed"`
	DenialReason      *string    `gorm:"column:denial_reason"`
	DeliveryState     string     `gorm:"column:delivery_state"`
	TelegramMessageID *int64     `gorm:"column:telegram_message_id"`
	DeliveryError     *string    `gorm:"column:delivery_error"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	CompletedAt       *time.Time `gorm:"column:completed_at"`
}

func (openEventRow) TableName() string { return "whisper_open_events" }

type ephemeralDeleteJobRow struct {
	ID                 int64      `gorm:"column:id;primaryKey"`
	ChatID             int64      `gorm:"column:chat_id"`
	RecipientID        int64      `gorm:"column:recipient_id"`
	EphemeralMessageID int64      `gorm:"column:ephemeral_message_id"`
	WhisperID          *uuid.UUID `gorm:"column:whisper_id"`
	DeleteAfter        time.Time  `gorm:"column:delete_after"`
	NextAttemptAt      time.Time  `gorm:"column:next_attempt_at"`
	AttemptCount       int        `gorm:"column:attempt_count"`
	LeaseUntil         *time.Time `gorm:"column:lease_until"`
	LastError          *string    `gorm:"column:last_error"`
	DeletedAt          *time.Time `gorm:"column:deleted_at"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
}

func (ephemeralDeleteJobRow) TableName() string { return "ephemeral_delete_jobs" }

type processedUpdateRow struct {
	TelegramUpdateID int64      `gorm:"column:telegram_update_id;primaryKey"`
	UpdateType       string     `gorm:"column:update_type"`
	PayloadSHA256    []byte     `gorm:"column:payload_sha256"`
	State            string     `gorm:"column:state"`
	AttemptCount     int        `gorm:"column:attempt_count"`
	LeaseUntil       *time.Time `gorm:"column:lease_until"`
	LastError        *string    `gorm:"column:last_error"`
	ReceivedAt       time.Time  `gorm:"column:received_at"`
	ProcessedAt      *time.Time `gorm:"column:processed_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`
}

func (processedUpdateRow) TableName() string { return "processed_updates" }
