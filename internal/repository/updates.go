package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Store) ClaimUpdate(ctx context.Context, params ClaimUpdateParams) (UpdateLease, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return UpdateLease{}, err
	}
	now := nowOr(params.Now)
	leaseUntil := params.LeaseUntil.UTC()
	if params.TelegramUpdateID < 0 || !leaseUntil.After(now) ||
		(len(params.PayloadSHA256) != 0 && len(params.PayloadSHA256) != sha256.Size) {
		return UpdateLease{}, fmt.Errorf("%w: invalid Telegram update lease", ErrInvalidInput)
	}

	var lease UpdateLease
	err = db.Transaction(func(tx *gorm.DB) error {
		row := processedUpdateRow{
			TelegramUpdateID: params.TelegramUpdateID,
			UpdateType:       params.UpdateType,
			PayloadSHA256:    cloneBytes(params.PayloadSHA256),
			State:            "processing",
			AttemptCount:     1,
			LeaseUntil:       timePointer(leaseUntil),
			ReceivedAt:       now,
			UpdatedAt:        now,
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if result.Error != nil {
			return translateError(result.Error)
		}
		if result.RowsAffected == 1 {
			lease = UpdateLease{Acquired: true, Attempts: 1, LeaseUntil: timePointer(leaseUntil)}
			return nil
		}

		var existing processedUpdateRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("telegram_update_id = ?", params.TelegramUpdateID).Take(&existing).Error; err != nil {
			return translateError(err)
		}
		if len(existing.PayloadSHA256) > 0 && len(params.PayloadSHA256) > 0 &&
			!bytes.Equal(existing.PayloadSHA256, params.PayloadSHA256) {
			return fmt.Errorf("%w: update payload hash changed", ErrConflict)
		}
		if existing.State == "processed" {
			lease = UpdateLease{AlreadyDone: true, Attempts: existing.AttemptCount}
			return nil
		}
		if existing.State == "processing" && existing.LeaseUntil != nil && existing.LeaseUntil.After(now) {
			lease = UpdateLease{Attempts: existing.AttemptCount, LeaseUntil: cloneTimePointer(existing.LeaseUntil)}
			return nil
		}
		if params.MaxAttempts > 0 && existing.AttemptCount >= params.MaxAttempts {
			return fmt.Errorf("%w: update %d failed %d times", ErrUpdateDead, existing.TelegramUpdateID, existing.AttemptCount)
		}

		result = tx.Model(&processedUpdateRow{}).
			Where("telegram_update_id = ?", params.TelegramUpdateID).
			Updates(map[string]any{
				"state":         "processing",
				"attempt_count": gorm.Expr("attempt_count + 1"),
				"lease_until":   leaseUntil,
				"last_error":    nil,
				"updated_at":    now,
			})
		if result.Error != nil {
			return translateError(result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrConflict
		}
		lease = UpdateLease{Acquired: true, Attempts: existing.AttemptCount + 1, LeaseUntil: timePointer(leaseUntil)}
		return nil
	})
	return lease, err
}

func (s *Store) CompleteUpdate(ctx context.Context, params FinishUpdateParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	if params.TelegramUpdateID < 0 || params.ExpectedLeaseUntil.IsZero() {
		return fmt.Errorf("%w: invalid completed update", ErrInvalidInput)
	}
	result := db.Model(&processedUpdateRow{}).
		Where("telegram_update_id = ? AND state = 'processing' AND lease_until = ?",
			params.TelegramUpdateID, params.ExpectedLeaseUntil.UTC()).
		Updates(map[string]any{
			"state":        "processed",
			"lease_until":  nil,
			"processed_at": now,
			"updated_at":   now,
		})
	if result.Error != nil {
		return translateError(result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) FailUpdate(ctx context.Context, params FinishUpdateParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	if params.TelegramUpdateID < 0 || params.ExpectedLeaseUntil.IsZero() {
		return fmt.Errorf("%w: invalid failed update", ErrInvalidInput)
	}
	result := db.Model(&processedUpdateRow{}).
		Where("telegram_update_id = ? AND state = 'processing' AND lease_until = ?",
			params.TelegramUpdateID, params.ExpectedLeaseUntil.UTC()).
		Updates(map[string]any{
			"state":       "failed",
			"lease_until": nil,
			"last_error":  safeErrorCode(params.ErrorCode),
			"updated_at":  now,
		})
	if result.Error != nil {
		return translateError(result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) ClaimDueEphemeralDelete(ctx context.Context, params ClaimEphemeralDeleteParams) (EphemeralDeleteJob, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return EphemeralDeleteJob{}, err
	}
	now := nowOr(params.Now)
	leaseUntil := params.LeaseUntil.UTC()
	if !leaseUntil.After(now) {
		return EphemeralDeleteJob{}, fmt.Errorf("%w: deletion lease must end after now", ErrInvalidInput)
	}
	var row ephemeralDeleteJobRow
	err = db.Raw(`
        WITH candidate AS (
            SELECT id
            FROM ephemeral_delete_jobs
            WHERE deleted_at IS NULL
              AND next_attempt_at <= ?
              AND (lease_until IS NULL OR lease_until <= ?)
            ORDER BY next_attempt_at, id
            FOR UPDATE SKIP LOCKED
            LIMIT 1
        )
        UPDATE ephemeral_delete_jobs AS job
        SET lease_until = ?,
            attempt_count = attempt_count + 1,
            updated_at = ?
        FROM candidate
        WHERE job.id = candidate.id
        RETURNING job.*`, now, now, leaseUntil, now).Scan(&row).Error
	if err != nil {
		return EphemeralDeleteJob{}, translateError(err)
	}
	if row.ID == 0 {
		return EphemeralDeleteJob{}, ErrNotFound
	}
	return row.toDomain(), nil
}

func (s *Store) MarkEphemeralDeleted(ctx context.Context, params FinishEphemeralDeleteParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	if params.JobID <= 0 || params.ExpectedLeaseUntil.IsZero() {
		return fmt.Errorf("%w: invalid deletion completion", ErrInvalidInput)
	}
	terminalCode := safeErrorCode(params.ErrorCode)
	result := db.Model(&ephemeralDeleteJobRow{}).
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

func (s *Store) RetryEphemeralDelete(ctx context.Context, params FinishEphemeralDeleteParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	next := params.NextAttemptAt.UTC()
	if params.JobID <= 0 || params.ExpectedLeaseUntil.IsZero() || !next.After(now) {
		return fmt.Errorf("%w: retry requires a future attempt", ErrInvalidInput)
	}
	result := db.Model(&ephemeralDeleteJobRow{}).
		Where("id = ? AND deleted_at IS NULL AND lease_until = ?", params.JobID, params.ExpectedLeaseUntil.UTC()).
		Updates(map[string]any{
			"lease_until":     nil,
			"next_attempt_at": next,
			"last_error":      safeErrorCode(params.ErrorCode),
			"updated_at":      now,
		})
	if result.Error != nil {
		return translateError(result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrLeaseLost
	}
	return nil
}
