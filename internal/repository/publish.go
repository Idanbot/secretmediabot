package repository

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"gorm.io/gorm"
)

func (s *Store) ClaimPublish(ctx context.Context, params ClaimPublishParams) (PublishClaim, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return PublishClaim{}, err
	}
	now := nowOr(params.Now)
	leaseUntil := params.LeaseUntil.UTC()
	if params.WhisperID == uuid.Nil || !leaseUntil.After(now) {
		return PublishClaim{}, fmt.Errorf("%w: invalid publish claim", ErrInvalidInput)
	}

	var claim PublishClaim
	err = db.Transaction(func(tx *gorm.DB) error {
		var returned struct {
			ID uuid.UUID `gorm:"column:id"`
		}
		if err := tx.Raw(`
            UPDATE whispers
            SET publish_state = 'publishing',
                publish_attempt_count = publish_attempt_count + 1,
                publish_lease_until = ?,
                last_publish_error = NULL,
                updated_at = ?
            WHERE id = ?
              AND status = 'active'
              AND expires_at > ?
              AND publish_state IN ('pending', 'retry_wait')
              AND next_publish_attempt_at <= ?
            RETURNING id`, leaseUntil, now, params.WhisperID, now, now).
			Scan(&returned).Error; err != nil {
			return err
		}
		if returned.ID == uuid.Nil {
			return ErrNotActive
		}
		loaded, err := loadPublishClaim(tx, returned.ID)
		if err != nil {
			return err
		}
		claim = loaded
		return nil
	})
	return claim, err
}

func (s *Store) ClaimNextPublish(ctx context.Context, now, leaseUntil time.Time) (PublishClaim, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return PublishClaim{}, err
	}
	now = nowOr(now)
	leaseUntil = leaseUntil.UTC()
	if !leaseUntil.After(now) {
		return PublishClaim{}, fmt.Errorf("%w: publish lease must end after now", ErrInvalidInput)
	}

	var claim PublishClaim
	err = db.Transaction(func(tx *gorm.DB) error {
		var returned struct {
			ID uuid.UUID `gorm:"column:id"`
		}
		if err := tx.Raw(`
            WITH candidate AS (
                SELECT id
                FROM whispers
                WHERE status = 'active'
                  AND expires_at > ?
                  AND publish_state IN ('pending', 'retry_wait')
                  AND next_publish_attempt_at <= ?
                ORDER BY next_publish_attempt_at, created_at, id
                FOR UPDATE SKIP LOCKED
                LIMIT 1
            )
            UPDATE whispers AS whisper
            SET publish_state = 'publishing',
                publish_attempt_count = publish_attempt_count + 1,
                publish_lease_until = ?,
                last_publish_error = NULL,
                updated_at = ?
            FROM candidate
            WHERE whisper.id = candidate.id
            RETURNING whisper.id`, now, now, leaseUntil, now).
			Scan(&returned).Error; err != nil {
			return err
		}
		if returned.ID == uuid.Nil {
			return ErrNotFound
		}
		loaded, err := loadPublishClaim(tx, returned.ID)
		if err != nil {
			return err
		}
		claim = loaded
		return nil
	})
	return claim, err
}

func (s *Store) MarkPublished(ctx context.Context, params MarkPublishedParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	if params.WhisperID == uuid.Nil || params.PublicMessageID <= 0 || params.ExpectedLeaseUntil.IsZero() {
		return fmt.Errorf("%w: invalid published marker", ErrInvalidInput)
	}
	return db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&whisperRow{}).
			Where("id = ? AND publish_state = 'publishing' AND publish_lease_until = ?",
				params.WhisperID, params.ExpectedLeaseUntil.UTC()).
			Updates(map[string]any{
				"publish_state":       string(domain.PublishPublished),
				"publish_lease_until": nil,
				"public_message_id":   params.PublicMessageID,
				"published_at":        now,
				"updated_at":          now,
			})
		if result.Error != nil {
			return translateError(result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrLeaseLost
		}
		result = tx.Where("whisper_id = ?", params.WhisperID).Delete(&encryptedCallbackTokenRow{})
		if result.Error != nil {
			return translateError(result.Error)
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: missing encrypted callback token", ErrConflict)
		}
		return nil
	})
}

func (s *Store) MarkPublishFailed(ctx context.Context, params MarkPublishFailedParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	if params.WhisperID == uuid.Nil || params.ExpectedLeaseUntil.IsZero() {
		return fmt.Errorf("%w: invalid publish failure marker", ErrInvalidInput)
	}
	nextState := domain.PublishFailed
	nextAttempt := now
	if !params.Terminal {
		if params.RetryAt == nil || !params.RetryAt.After(now) {
			return fmt.Errorf("%w: retryable publication failure requires a future retry", ErrInvalidInput)
		}
		nextState = domain.PublishRetryWait
		nextAttempt = params.RetryAt.UTC()
	}
	result := db.Model(&whisperRow{}).
		Where("id = ? AND publish_state = 'publishing' AND publish_lease_until = ?",
			params.WhisperID, params.ExpectedLeaseUntil.UTC()).
		Updates(map[string]any{
			"publish_state":           string(nextState),
			"publish_lease_until":     nil,
			"next_publish_attempt_at": nextAttempt,
			"last_publish_error":      safeErrorCode(params.ErrorCode),
			"updated_at":              now,
		})
	if result.Error != nil {
		return translateError(result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrLeaseLost
	}
	return nil
}

func loadPublishClaim(db *gorm.DB, whisperID uuid.UUID) (PublishClaim, error) {
	record, err := loadWhisperProjection(db, whisperID)
	if err != nil {
		return PublishClaim{}, err
	}
	whisper, err := record.toDomain(nil)
	if err != nil {
		return PublishClaim{}, err
	}
	var token encryptedCallbackTokenRow
	if err := db.Where("whisper_id = ?", whisperID).Take(&token).Error; err != nil {
		return PublishClaim{}, translateError(err)
	}
	return PublishClaim{Whisper: whisper, CallbackToken: token.toStored()}, nil
}

func safeErrorCode(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unspecified"
	}
	const maxRunes = 128
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxRunes])
}
