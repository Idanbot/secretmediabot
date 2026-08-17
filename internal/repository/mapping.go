package repository

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"gorm.io/gorm"
)

const whisperMetadataSelect = `
    w.id,
    w.draft_id,
    w.open_token_hash,
    w.sender_id,
    w.recipient_id,
    w.source_chat_id,
    w.source_thread_id,
    w.payload_kind,
    w.media_provider,
    w.media_type,
    w.one_time,
    w.protect_content,
    w.status,
    w.publish_state,
    w.publish_attempt_count,
    w.next_publish_attempt_at,
    w.publish_lease_until,
    w.last_publish_error,
    w.public_message_id,
    w.published_at,
    w.opening_callback_query_id,
    w.opening_reserved_at,
    w.opening_lease_until,
    w.opened_at,
    w.revoked_at,
    w.created_at,
    w.updated_at,
    w.expires_at,
    w.retention_delete_at,
    mb.id AS media_blob_id,
    mb.content_type AS media_content_type,
    mb.plaintext_size_bytes AS media_plaintext_size_bytes,
    mb.retention_delete_at AS media_retain_until,
    text_payload.id AS text_blob_id,
    text_payload.retention_delete_at AS text_retain_until,
    caption_payload.id AS caption_blob_id`

const whisperMetadataJoins = `
    LEFT JOIN media_blobs mb ON mb.whisper_id = w.id
    LEFT JOIN encrypted_text_payloads text_payload
        ON text_payload.whisper_id = w.id AND text_payload.purpose = 'text'
    LEFT JOIN encrypted_text_payloads caption_payload
        ON caption_payload.whisper_id = w.id AND caption_payload.purpose = 'caption'`

type whisperProjection struct {
	WhisperRow              `gorm:"embedded"`
	MediaBlobID             *uuid.UUID `gorm:"column:media_blob_id"`
	MediaContentType        *string    `gorm:"column:media_content_type"`
	MediaPlaintextSizeBytes *int64     `gorm:"column:media_plaintext_size_bytes"`
	MediaRetainUntil        *time.Time `gorm:"column:media_retain_until"`
	TextBlobID              *uuid.UUID `gorm:"column:text_blob_id"`
	TextRetainUntil         *time.Time `gorm:"column:text_retain_until"`
	CaptionBlobID           *uuid.UUID `gorm:"column:caption_blob_id"`
}

func whisperMetadataQuery(db *gorm.DB) *gorm.DB {
	return db.Table("whispers AS w").Select(whisperMetadataSelect).Joins(whisperMetadataJoins)
}

func loadWhisperProjection(db *gorm.DB, whisperID uuid.UUID) (whisperProjection, error) {
	var record whisperProjection
	err := whisperMetadataQuery(db).Where("w.id = ?", whisperID).Take(&record).Error
	if err != nil {
		return whisperProjection{}, translateError(err)
	}
	return record, nil
}

func (r whisperProjection) toDomain(openTokenHash []byte) (domain.Whisper, error) {
	if len(openTokenHash) == 0 {
		openTokenHash = r.OpenTokenHash
	}
	content := domain.ContentReference{Kind: domain.PayloadKind(r.PayloadKind)}
	var contentRetainUntil *time.Time
	switch content.Kind {
	case domain.PayloadText:
		if r.TextBlobID == nil {
			return domain.Whisper{}, fmt.Errorf("%w: text whisper %s has no encrypted text row", ErrConflict, r.ID)
		}
		content.TextBlobID = cloneUUIDPointer(r.TextBlobID)
		contentRetainUntil = cloneTimePointer(r.TextRetainUntil)
	case domain.PayloadMedia:
		if r.MediaBlobID == nil || r.MediaProvider == nil || r.MediaType == nil {
			return domain.Whisper{}, fmt.Errorf("%w: media whisper %s has incomplete blob metadata", ErrConflict, r.ID)
		}
		media := domain.MediaReference{
			Provider: domain.MediaProvider(*r.MediaProvider),
			Type:     domain.MediaType(*r.MediaType),
			// For postgres_blob, the provider reference is the non-secret blob
			// UUID. Telegram delivery handles are deliberately loaded only by
			// the recipient delivery path and never by metadata projections.
			Ref:    r.MediaBlobID.String(),
			BlobID: cloneUUIDPointer(r.MediaBlobID),
		}
		if r.MediaContentType != nil {
			media.ContentType = *r.MediaContentType
		}
		if r.MediaPlaintextSizeBytes != nil {
			media.SizeBytes = *r.MediaPlaintextSizeBytes
		}
		content.Media = &media
		content.CaptionBlobID = cloneUUIDPointer(r.CaptionBlobID)
		contentRetainUntil = cloneTimePointer(r.MediaRetainUntil)
	default:
		return domain.Whisper{}, fmt.Errorf("%w: unsupported payload kind %q", ErrConflict, r.PayloadKind)
	}

	nextAttempt := r.NextPublishAttemptAt
	w := domain.Whisper{
		ID:                   r.ID,
		DraftID:              r.DraftID,
		OpenTokenHash:        cloneBytes(openTokenHash),
		SenderID:             r.SenderID,
		RecipientID:          r.RecipientID,
		SourceChatID:         r.SourceChatID,
		SourceThreadID:       cloneInt64Pointer(r.SourceThreadID),
		Content:              content,
		OneTime:              r.OneTime,
		ProtectContent:       r.ProtectContent,
		Status:               domain.WhisperStatus(r.Status),
		PublishState:         domain.PublishState(r.PublishState),
		PublicMessageID:      cloneInt64Pointer(r.PublicMessageID),
		PublishAttemptCount:  r.PublishAttemptCount,
		PublishLeaseUntil:    cloneTimePointer(r.PublishLeaseUntil),
		NextPublishAttemptAt: nextAttempt,
		OpeningReservedAt:    cloneTimePointer(r.OpeningReservedAt),
		OpeningLeaseUntil:    cloneTimePointer(r.OpeningLeaseUntil),
		CreatedAt:            r.CreatedAt,
		UpdatedAt:            r.UpdatedAt,
		PublishedAt:          cloneTimePointer(r.PublishedAt),
		ExpiresAt:            r.ExpiresAt,
		OpenedAt:             cloneTimePointer(r.OpenedAt),
		RevokedAt:            cloneTimePointer(r.RevokedAt),
		ContentRetainUntil:   contentRetainUntil,
		MetadataRetainUntil:  timePointer(r.RetentionDeleteAt),
	}
	if r.LastPublishError != nil {
		w.LastPublishError = *r.LastPublishError
	}
	if r.OpeningCallbackQueryID != nil {
		w.OpeningCallbackQueryID = *r.OpeningCallbackQueryID
	}
	return w, nil
}

func (r userRow) toDomain() domain.User {
	return domain.User{
		TelegramUserID:        r.TelegramUserID,
		Username:              r.Username,
		FirstName:             r.FirstName,
		LastName:              r.LastName,
		LanguageCode:          r.LanguageCode,
		IsBot:                 r.IsBot,
		HasStartedPrivateChat: r.HasStartedPrivateChat,
		FirstSeenAt:           r.FirstSeenAt,
		LastSeenAt:            r.LastSeenAt,
		UpdatedAt:             r.UpdatedAt,
	}
}

func (r chatRow) toDomain() domain.Chat {
	return domain.Chat{
		TelegramChatID: r.TelegramChatID,
		Type:           domain.ChatType(r.ChatType),
		Title:          r.Title,
		Username:       r.Username,
		FirstSeenAt:    r.FirstSeenAt,
		LastSeenAt:     r.LastSeenAt,
		UpdatedAt:      r.UpdatedAt,
	}
}

func (r draftRow) toDomain() domain.Draft {
	return domain.Draft{
		ID:                     r.ID,
		ComposeTokenHash:       cloneBytes(r.ComposeTokenHash),
		SenderID:               r.SenderID,
		RecipientID:            r.RecipientID,
		SourceChatID:           r.SourceChatID,
		SourceThreadID:         cloneInt64Pointer(r.SourceThreadID),
		SourceReplyMessageID:   cloneInt64Pointer(r.SourceReplyMessageID),
		SourceCommandMessageID: cloneInt64Pointer(r.SourceCommandMessageID),
		State:                  domain.DraftState(r.State),
		CreatedAt:              r.CreatedAt,
		UpdatedAt:              r.UpdatedAt,
		ExpiresAt:              r.ExpiresAt,
		IngestStartedAt:        cloneTimePointer(r.IngestStartedAt),
		IngestLeaseUntil:       cloneTimePointer(r.IngestLeaseUntil),
		CompletedAt:            cloneTimePointer(r.CompletedAt),
		CancelledAt:            cloneTimePointer(r.CancelledAt),
	}
}

func (r mediaBlobRow) toStored() StoredEncryptedPayload {
	return StoredEncryptedPayload{
		ID:                  r.ID,
		EncryptionAlgorithm: r.EncryptionAlgorithm,
		EncryptionKeyID:     r.EncryptionKeyID,
		Nonce:               cloneBytes(r.Nonce),
		Ciphertext:          cloneBytes(r.Ciphertext),
		CiphertextSHA256:    cloneBytes(r.CiphertextSHA256),
		ContentType:         r.ContentType,
		PlaintextSize:       r.PlaintextSizeBytes,
		RetainUntil:         r.RetentionDeleteAt,
	}
}

func (r encryptedTextPayloadRow) toStored() StoredEncryptedPayload {
	return StoredEncryptedPayload{
		ID:                  r.ID,
		EncryptionAlgorithm: r.EncryptionAlgorithm,
		EncryptionKeyID:     r.EncryptionKeyID,
		Nonce:               cloneBytes(r.Nonce),
		Ciphertext:          cloneBytes(r.Ciphertext),
		CiphertextSHA256:    cloneBytes(r.CiphertextSHA256),
		PlaintextSize:       r.PlaintextSizeBytes,
		RetainUntil:         r.RetentionDeleteAt,
	}
}

func (r encryptedCallbackTokenRow) toStored() StoredEncryptedPayload {
	return StoredEncryptedPayload{
		ID:                  r.ID,
		EncryptionAlgorithm: r.EncryptionAlgorithm,
		EncryptionKeyID:     r.EncryptionKeyID,
		Nonce:               cloneBytes(r.Nonce),
		Ciphertext:          cloneBytes(r.Ciphertext),
		CiphertextSHA256:    cloneBytes(r.CiphertextSHA256),
		PlaintextSize:       r.PlaintextSizeBytes,
	}
}

func (r ephemeralDeleteJobRow) toDomain() EphemeralDeleteJob {
	job := EphemeralDeleteJob{
		ID:                 r.ID,
		ChatID:             r.ChatID,
		RecipientID:        r.RecipientID,
		EphemeralMessageID: r.EphemeralMessageID,
		WhisperID:          cloneUUIDPointer(r.WhisperID),
		DeleteAfter:        r.DeleteAfter,
		AttemptCount:       r.AttemptCount,
	}
	if r.LeaseUntil != nil {
		job.LeaseUntil = *r.LeaseUntil
	}
	return job
}

func normalizeUsername(username string) string {
	return strings.ToLower(strings.TrimLeft(strings.TrimSpace(username), "@"))
}

func translateError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		return ErrNotFound
	case errors.Is(err, gorm.ErrDuplicatedKey), errors.Is(err, gorm.ErrForeignKeyViolated):
		return fmt.Errorf("%w: %v", ErrConflict, err)
	default:
		return err
	}
}

func cloneBytes(value []byte) []byte {
	return append([]byte(nil), value...)
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneUUIDPointer(value *uuid.UUID) *uuid.UUID {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	return timePointer(*value)
}

func timePointer(value time.Time) *time.Time {
	copy := value
	return &copy
}
