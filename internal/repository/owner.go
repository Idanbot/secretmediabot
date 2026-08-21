package repository

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// OwnerListWhispers returns metadata only. The service must authenticate the
// configured owner ID before calling any Owner-prefixed method.
func (s *Store) OwnerListWhispers(ctx context.Context, params OwnerListWhispersParams) ([]domain.Whisper, error) {
	details, err := s.OwnerListWhisperDetails(ctx, params)
	if err != nil {
		return nil, err
	}
	whispers := make([]domain.Whisper, 0, len(details))
	for _, detail := range details {
		whispers = append(whispers, detail.Whisper)
	}
	return whispers, nil
}

// OwnerListWhisperDetails returns metadata plus the latest non-secret
// display labels for both participants. It never selects encrypted payloads,
// Telegram file IDs, or decrypted content.
func (s *Store) OwnerListWhisperDetails(ctx context.Context, params OwnerListWhispersParams) ([]domain.OwnerWhisper, error) {
	limit, offset, err := normalizeOwnerListPage(params.Limit, params.Offset)
	if err != nil {
		return nil, err
	}
	db, err := s.withContext(ctx)
	if err != nil {
		return nil, err
	}
	if params.OwnerTelegramUserID <= 0 {
		return nil, fmt.Errorf("%w: owner ID must be positive", ErrInvalidInput)
	}
	if params.SenderID != nil && *params.SenderID <= 0 {
		return nil, fmt.Errorf("%w: sender ID must be positive", ErrInvalidInput)
	}
	senderUsername := normalizeUsername(params.SenderUsername)
	if params.SenderUsername != "" && senderUsername == "" {
		return nil, fmt.Errorf("%w: sender username is required", ErrInvalidInput)
	}
	for _, mediaType := range params.MediaTypes {
		if !mediaType.IsValid() {
			return nil, fmt.Errorf("%w: invalid owner media type", ErrInvalidInput)
		}
	}
	var details []domain.OwnerWhisper
	err = db.Transaction(func(tx *gorm.DB) error {
		query := ownerWhisperMetadataQuery(tx).
			Order("w.created_at DESC, w.id DESC").
			Limit(limit).
			Offset(offset)
		if params.Before != nil {
			query = query.Where("w.created_at < ?", params.Before.UTC())
		}
		if params.SenderID != nil {
			query = query.Where("w.sender_id = ?", *params.SenderID)
		}
		if senderUsername != "" {
			query = query.Where("owner_sender.username_normalized = ?", senderUsername)
		}
		if len(params.MediaTypes) > 0 {
			query = query.Where("w.payload_kind = ? AND w.media_type IN ?", domain.PayloadMedia, params.MediaTypes)
		}
		var records []whisperProjection
		if err := query.Find(&records).Error; err != nil {
			return translateError(err)
		}
		details = make([]domain.OwnerWhisper, 0, len(records))
		for _, record := range records {
			detail, err := record.toOwnerDomain()
			if err != nil {
				return err
			}
			details = append(details, detail)
		}
		return insertOwnerAudit(tx, params.OwnerTelegramUserID, nil, domain.OwnerAuditViewMetadata, params.Reason,
			map[string]any{"result_count": len(details), "limit": limit, "offset": offset})
	})
	if err != nil {
		return nil, err
	}
	return details, nil
}

func normalizeOwnerListPage(limit, offset int) (int, int, error) {
	if offset < 0 {
		return 0, 0, fmt.Errorf("%w: owner list offset must be nonnegative", ErrInvalidInput)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	return limit, offset, nil
}

func (s *Store) OwnerGetWhisper(ctx context.Context, params OwnerGetWhisperParams) (domain.Whisper, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return domain.Whisper{}, err
	}
	if params.OwnerTelegramUserID <= 0 || params.WhisperID == uuid.Nil {
		return domain.Whisper{}, fmt.Errorf("%w: owner and whisper IDs are required", ErrInvalidInput)
	}
	var result domain.Whisper
	err = db.Transaction(func(tx *gorm.DB) error {
		record, err := loadWhisperProjection(tx, params.WhisperID)
		if err != nil {
			return err
		}
		result, err = record.toDomain(nil)
		if err != nil {
			return err
		}
		return insertOwnerAudit(tx, params.OwnerTelegramUserID, &params.WhisperID,
			domain.OwnerAuditViewMetadata, params.Reason, nil)
	})
	return result, err
}

func (s *Store) OwnerFetchEncryptedContent(ctx context.Context, params OwnerGetWhisperParams) (StoredContent, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return StoredContent{}, err
	}
	if params.OwnerTelegramUserID <= 0 || params.WhisperID == uuid.Nil {
		return StoredContent{}, fmt.Errorf("%w: owner and whisper IDs are required", ErrInvalidInput)
	}
	var content StoredContent
	err = db.Transaction(func(tx *gorm.DB) error {
		var whisper whisperRow
		if err := tx.Where("id = ?", params.WhisperID).Take(&whisper).Error; err != nil {
			return translateError(err)
		}
		loaded, err := loadStoredContent(tx, whisper, true)
		if err != nil {
			return err
		}
		content = loaded
		action := ownerRetrieveAction(content.Kind)
		return insertOwnerAudit(tx, params.OwnerTelegramUserID, &params.WhisperID,
			action, params.Reason, map[string]any{"payload_kind": whisper.PayloadKind})
	})
	return content, err
}

func ownerRetrieveAction(kind domain.PayloadKind) domain.OwnerAuditAction {
	if kind == domain.PayloadText {
		return domain.OwnerAuditRetrieveContent
	}
	return domain.OwnerAuditRetrieveMedia
}

func (s *Store) OwnerDeleteWhisper(ctx context.Context, params OwnerDeleteWhisperParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	if params.OwnerTelegramUserID <= 0 || params.WhisperID == uuid.Nil {
		return fmt.Errorf("%w: owner and whisper IDs are required", ErrInvalidInput)
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var whisper whisperRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", params.WhisperID).Take(&whisper).Error; err != nil {
			return translateError(err)
		}
		if err := insertOwnerAudit(tx, params.OwnerTelegramUserID, &params.WhisperID,
			domain.OwnerAuditDeleteWhisper, params.Reason, map[string]any{"deleted_at": now}); err != nil {
			return err
		}
		result := tx.Where("id = ?", params.WhisperID).Delete(&whisperRow{})
		if result.Error != nil {
			return translateError(result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrNotFound
		}
		return nil
	})
}

func (s *Store) OwnerUpdateRetention(ctx context.Context, params OwnerUpdateRetentionParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	retainUntil := params.RetainUntil.UTC()
	if params.OwnerTelegramUserID <= 0 || params.WhisperID == uuid.Nil || !retainUntil.After(now) {
		return fmt.Errorf("%w: owner, whisper, and future retention deadline are required", ErrInvalidInput)
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var whisper whisperRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", params.WhisperID).Take(&whisper).Error; err != nil {
			return translateError(err)
		}
		if err := tx.Model(&whisperRow{}).Where("id = ?", params.WhisperID).
			Updates(map[string]any{"retention_delete_at": retainUntil, "updated_at": now}).Error; err != nil {
			return translateError(err)
		}
		if err := tx.Model(&mediaBlobRow{}).Where("whisper_id = ?", params.WhisperID).
			Update("retention_delete_at", retainUntil).Error; err != nil {
			return translateError(err)
		}
		if err := tx.Model(&encryptedTextPayloadRow{}).Where("whisper_id = ?", params.WhisperID).
			Update("retention_delete_at", retainUntil).Error; err != nil {
			return translateError(err)
		}
		return insertOwnerAudit(tx, params.OwnerTelegramUserID, &params.WhisperID,
			domain.OwnerAuditUpdateRetention, params.Reason, map[string]any{"retain_until": retainUntil})
	})
}

func insertOwnerAudit(tx *gorm.DB, ownerID int64, whisperID *uuid.UUID, action domain.OwnerAuditAction, reason string, details map[string]any) error {
	if ownerID <= 0 || !action.IsValid() {
		return fmt.Errorf("%w: invalid owner audit event", ErrInvalidInput)
	}
	if details == nil {
		details = make(map[string]any)
	}
	if reason != "" {
		details["reason"] = reason
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode owner audit details: %w", err)
	}
	return translateError(tx.Exec(`
        INSERT INTO owner_audit_events (
            owner_telegram_user_id, action, whisper_id, success, details, created_at
        ) VALUES (?, ?, ?, TRUE, ?::jsonb, NOW())`,
		ownerID, string(action), whisperID, string(encoded)).Error)
}

// WhisperMediaBlob is the stored encrypted media payload of a whisper plus its
// delivery metadata, for the re-upload fallback when Telegram permanently
// rejects the stored file_id.
type WhisperMediaBlob struct {
	WhisperID    uuid.UUID
	MediaType    domain.MediaType
	ContentType  string
	TelegramFile *string
	Stored       StoredEncryptedPayload
}

// FetchWhisperMedia loads a whisper's encrypted media blob without any owner
// gate; callers must already hold a reserved one-time open for the whisper.
func (s *Store) FetchWhisperMedia(ctx context.Context, whisperID uuid.UUID) (WhisperMediaBlob, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return WhisperMediaBlob{}, err
	}
	if whisperID == uuid.Nil {
		return WhisperMediaBlob{}, fmt.Errorf("%w: whisper ID is required", ErrInvalidInput)
	}
	var blob WhisperMediaBlob
	err = db.Transaction(func(tx *gorm.DB) error {
		var whisper whisperRow
		if err := tx.Select("id", "media_type", "telegram_file_id").
			Where("id = ?", whisperID).Take(&whisper).Error; err != nil {
			return translateError(err)
		}
		if whisper.MediaType == nil {
			return fmt.Errorf("%w: whisper has no media", ErrConflict)
		}
		var row mediaBlobRow
		if err := tx.Where("whisper_id = ?", whisperID).Take(&row).Error; err != nil {
			return translateError(err)
		}
		blob = WhisperMediaBlob{
			WhisperID: whisperID, MediaType: domain.MediaType(*whisper.MediaType),
			ContentType: row.ContentType, TelegramFile: whisper.TelegramFileID, Stored: row.toStored(),
		}
		return nil
	})
	if err != nil {
		return WhisperMediaBlob{}, err
	}
	return blob, nil
}
