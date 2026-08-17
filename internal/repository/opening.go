package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Store) FindWhisperByOpenTokenHash(ctx context.Context, openTokenHash []byte) (domain.Whisper, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return domain.Whisper{}, err
	}
	if len(openTokenHash) != sha256.Size {
		return domain.Whisper{}, fmt.Errorf("%w: open token hash must be SHA-256", ErrInvalidInput)
	}
	var record whisperProjection
	err = whisperMetadataQuery(db).Where("w.open_token_hash = ?", openTokenHash).Take(&record).Error
	if err != nil {
		return domain.Whisper{}, translateError(err)
	}
	return record.toDomain(openTokenHash)
}

func (s *Store) ReserveOpen(ctx context.Context, params ReserveOpenParams) (OpenReservation, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return OpenReservation{}, err
	}
	now := nowOr(params.Now)
	leaseUntil := params.LeaseUntil.UTC()
	if len(params.OpenTokenHash) != sha256.Size || params.TelegramUserID <= 0 ||
		params.CallbackQueryID == "" || !leaseUntil.After(now) {
		return OpenReservation{}, fmt.Errorf("%w: invalid open reservation", ErrInvalidInput)
	}

	var reservation OpenReservation
	var deniedErr error
	err = db.Transaction(func(tx *gorm.DB) error {
		var whisper whisperRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("open_token_hash = ?", params.OpenTokenHash).Take(&whisper).Error; err != nil {
			return translateError(err)
		}

		if whisper.RecipientID != params.TelegramUserID {
			deniedErr = ErrUnauthorized
			return recordDeniedOpen(tx, whisper.ID, params, domain.OpenDeniedWrongUser)
		}
		if !whisper.ExpiresAt.After(now) {
			if whisper.Status == string(domain.WhisperActive) || whisper.Status == string(domain.WhisperOpening) {
				if err := expireLockedWhisper(tx, whisper.ID, now); err != nil {
					return err
				}
			}
			deniedErr = ErrExpired
			return recordDeniedOpen(tx, whisper.ID, params, domain.OpenDeniedExpired)
		}
		switch domain.WhisperStatus(whisper.Status) {
		case domain.WhisperRevoked:
			deniedErr = ErrNotActive
			return recordDeniedOpen(tx, whisper.ID, params, domain.OpenDeniedRevoked)
		case domain.WhisperOpened:
			deniedErr = ErrAlreadyOpened
			return recordDeniedOpen(tx, whisper.ID, params, domain.OpenDeniedAlreadyOpened)
		case domain.WhisperOpening:
			if whisper.OpeningLeaseUntil == nil || whisper.OpeningLeaseUntil.After(now) {
				deniedErr = ErrNotActive
				return recordDeniedOpen(tx, whisper.ID, params, domain.OpenDeniedNotActive)
			}
			if err := tx.Model(&whisperRow{}).Where("id = ?", whisper.ID).Updates(map[string]any{
				"status":                    string(domain.WhisperActive),
				"opening_callback_query_id": nil,
				"opening_reserved_at":       nil,
				"opening_lease_until":       nil,
				"updated_at":                now,
			}).Error; err != nil {
				return translateError(err)
			}
			whisper.Status = string(domain.WhisperActive)
			whisper.OpeningCallbackQueryID = nil
			whisper.OpeningReservedAt = nil
			whisper.OpeningLeaseUntil = nil
		case domain.WhisperActive:
		default:
			deniedErr = ErrNotActive
			return recordDeniedOpen(tx, whisper.ID, params, domain.OpenDeniedNotActive)
		}
		if whisper.PublishState != string(domain.PublishPublished) {
			deniedErr = ErrNotActive
			return recordDeniedOpen(tx, whisper.ID, params, domain.OpenDeniedNotActive)
		}

		if whisper.OneTime {
			result := tx.Model(&whisperRow{}).Where("id = ? AND status = 'active'", whisper.ID).Updates(map[string]any{
				"status":                    string(domain.WhisperOpening),
				"opening_callback_query_id": params.CallbackQueryID,
				"opening_reserved_at":       now,
				"opening_lease_until":       leaseUntil,
				"updated_at":                now,
			})
			if result.Error != nil {
				return translateError(result.Error)
			}
			if result.RowsAffected != 1 {
				return ErrConflict
			}
		}

		callbackID := params.CallbackQueryID
		event := openEventRow{
			WhisperID:       whisper.ID,
			TelegramUserID:  params.TelegramUserID,
			CallbackQueryID: &callbackID,
			Outcome:         string(domain.OpenAllowed),
			Allowed:         true,
			DeliveryState:   "reserved",
			CreatedAt:       now,
		}
		if err := tx.Create(&event).Error; err != nil {
			return translateError(err)
		}

		record, err := loadWhisperProjection(tx, whisper.ID)
		if err != nil {
			return err
		}
		domainWhisper, err := record.toDomain(params.OpenTokenHash)
		if err != nil {
			return err
		}
		content, err := loadDeliveryContent(tx, whisper)
		if err != nil {
			return err
		}
		reservation = OpenReservation{Whisper: domainWhisper, EventID: event.ID, Content: content}
		return nil
	})
	if err != nil {
		return OpenReservation{}, err
	}
	if deniedErr != nil {
		return OpenReservation{}, deniedErr
	}
	return reservation, nil
}

func (s *Store) CompleteOpen(ctx context.Context, params CompleteOpenParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	if params.WhisperID == uuid.Nil || params.EventID <= 0 || params.CallbackQueryID == "" ||
		params.EphemeralMessageID == nil || *params.EphemeralMessageID <= 0 || params.DeleteAt.IsZero() {
		return fmt.Errorf("%w: completed open requires event, callback, ephemeral message, and deletion deadline", ErrInvalidInput)
	}

	return db.Transaction(func(tx *gorm.DB) error {
		var event openEventRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND whisper_id = ?", params.EventID, params.WhisperID).Take(&event).Error; err != nil {
			return translateError(err)
		}
		if event.CallbackQueryID == nil || *event.CallbackQueryID != params.CallbackQueryID ||
			!event.Allowed || event.Outcome != string(domain.OpenAllowed) {
			return ErrConflict
		}

		var whisper whisperRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", params.WhisperID).Take(&whisper).Error; err != nil {
			return translateError(err)
		}
		if event.DeliveryState == "delivered" {
			var job ephemeralDeleteJobRow
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("whisper_id = ? AND chat_id = ? AND recipient_id = ? AND ephemeral_message_id = ?",
					whisper.ID, whisper.SourceChatID, whisper.RecipientID, *params.EphemeralMessageID).
				Take(&job).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrConflict
				}
				return translateError(err)
			}
			return nil
		}
		if event.DeliveryState != "reserved" {
			return ErrConflict
		}
		if !params.DeleteAt.After(now) {
			return fmt.Errorf("%w: a new completed open requires a future deletion deadline", ErrInvalidInput)
		}
		updates := map[string]any{"opened_at": gorm.Expr("COALESCE(opened_at, ?)", now), "updated_at": now}
		if whisper.OneTime {
			if whisper.Status != string(domain.WhisperOpening) || whisper.OpeningCallbackQueryID == nil ||
				*whisper.OpeningCallbackQueryID != params.CallbackQueryID {
				return ErrLeaseLost
			}
			updates["status"] = string(domain.WhisperOpened)
			updates["opening_callback_query_id"] = nil
			updates["opening_reserved_at"] = nil
			updates["opening_lease_until"] = nil
		}
		if err := tx.Model(&whisperRow{}).Where("id = ?", whisper.ID).Updates(updates).Error; err != nil {
			return translateError(err)
		}

		result := tx.Model(&openEventRow{}).Where("id = ? AND delivery_state = 'reserved'", event.ID).Updates(map[string]any{
			"delivery_state":      "delivered",
			"telegram_message_id": params.TelegramMessageID,
			"completed_at":        now,
		})
		if result.Error != nil {
			return translateError(result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrConflict
		}

		job := ephemeralDeleteJobRow{
			ChatID:             whisper.SourceChatID,
			RecipientID:        whisper.RecipientID,
			EphemeralMessageID: *params.EphemeralMessageID,
			WhisperID:          cloneUUIDPointer(&whisper.ID),
			DeleteAfter:        params.DeleteAt.UTC(),
			NextAttemptAt:      params.DeleteAt.UTC(),
			AttemptCount:       0,
			CreatedAt:          now,
			UpdatedAt:          now,
		}
		if err := tx.Create(&job).Error; err != nil {
			return translateError(err)
		}
		return nil
	})
}

func (s *Store) FailOpen(ctx context.Context, params FailOpenParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	if params.WhisperID == uuid.Nil || params.EventID <= 0 || params.CallbackQueryID == "" {
		return fmt.Errorf("%w: invalid failed open", ErrInvalidInput)
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var event openEventRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND whisper_id = ?", params.EventID, params.WhisperID).Take(&event).Error; err != nil {
			return translateError(err)
		}
		if event.CallbackQueryID == nil || *event.CallbackQueryID != params.CallbackQueryID || event.DeliveryState != "reserved" {
			return ErrConflict
		}
		result := tx.Model(&openEventRow{}).Where("id = ? AND delivery_state = 'reserved'", event.ID).Updates(map[string]any{
			"outcome":        string(domain.OpenDeliveryFailed),
			"allowed":        false,
			"delivery_state": "failed",
			"delivery_error": safeErrorCode(params.ErrorCode),
			"completed_at":   now,
		})
		if result.Error != nil {
			return translateError(result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrConflict
		}

		var whisper whisperRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", params.WhisperID).Take(&whisper).Error; err != nil {
			return translateError(err)
		}
		if whisper.OneTime && whisper.Status == string(domain.WhisperOpening) && whisper.OpeningCallbackQueryID != nil &&
			*whisper.OpeningCallbackQueryID == params.CallbackQueryID {
			status := domain.WhisperActive
			if !whisper.ExpiresAt.After(now) {
				status = domain.WhisperExpired
			}
			if err := tx.Model(&whisperRow{}).Where("id = ?", whisper.ID).Updates(map[string]any{
				"status":                    string(status),
				"opening_callback_query_id": nil,
				"opening_reserved_at":       nil,
				"opening_lease_until":       nil,
				"updated_at":                now,
			}).Error; err != nil {
				return translateError(err)
			}
		}
		return nil
	})
}

func recordDeniedOpen(tx *gorm.DB, whisperID uuid.UUID, params ReserveOpenParams, outcome domain.OpenEventOutcome) error {
	callbackID := params.CallbackQueryID
	reason := string(outcome)
	event := openEventRow{
		WhisperID:       whisperID,
		TelegramUserID:  params.TelegramUserID,
		CallbackQueryID: &callbackID,
		Outcome:         string(outcome),
		Allowed:         false,
		DenialReason:    &reason,
		DeliveryState:   "not_attempted",
		CreatedAt:       nowOr(params.Now),
	}
	if err := tx.Create(&event).Error; err != nil {
		return translateError(err)
	}
	return nil
}

func expireLockedWhisper(tx *gorm.DB, whisperID uuid.UUID, now time.Time) error {
	return translateError(tx.Model(&whisperRow{}).Where("id = ?", whisperID).Updates(map[string]any{
		"status":                    string(domain.WhisperExpired),
		"opening_callback_query_id": nil,
		"opening_reserved_at":       nil,
		"opening_lease_until":       nil,
		"updated_at":                now,
	}).Error)
}

func loadDeliveryContent(db *gorm.DB, whisper whisperRow) (DeliveryContent, error) {
	content := DeliveryContent{Kind: domain.PayloadKind(whisper.PayloadKind)}
	switch content.Kind {
	case domain.PayloadText:
		var row encryptedTextPayloadRow
		if err := db.Where("whisper_id = ? AND purpose = 'text'", whisper.ID).Take(&row).Error; err != nil {
			return DeliveryContent{}, translateError(err)
		}
		stored := row.toStored()
		content.Text = &stored
	case domain.PayloadMedia:
		if whisper.MediaType == nil || whisper.TelegramFileID == nil || *whisper.TelegramFileID == "" {
			return DeliveryContent{}, fmt.Errorf("%w: media delivery handle is missing", ErrConflict)
		}
		var metadata mediaBlobRow
		if err := db.Select("id", "content_type", "plaintext_size_bytes").
			Where("whisper_id = ?", whisper.ID).Take(&metadata).Error; err != nil {
			return DeliveryContent{}, translateError(err)
		}
		media := DeliveryMedia{
			BlobID:         metadata.ID,
			Type:           domain.MediaType(*whisper.MediaType),
			TelegramFileID: *whisper.TelegramFileID,
			ContentType:    metadata.ContentType,
			PlaintextSize:  metadata.PlaintextSizeBytes,
		}
		if whisper.TelegramFileUniqueID != nil {
			media.TelegramFileUniqueID = *whisper.TelegramFileUniqueID
		}
		content.Media = &media

		var caption encryptedTextPayloadRow
		err := db.Where("whisper_id = ? AND purpose = 'caption'", whisper.ID).Take(&caption).Error
		if err == nil {
			stored := caption.toStored()
			content.Caption = &stored
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return DeliveryContent{}, translateError(err)
		}
	default:
		return DeliveryContent{}, fmt.Errorf("%w: unsupported payload kind", ErrConflict)
	}
	return content, nil
}
