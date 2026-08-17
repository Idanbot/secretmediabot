package repository

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

func (s *Store) RunCleanup(ctx context.Context, params CleanupParams) (CleanupResult, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return CleanupResult{}, err
	}
	now := nowOr(params.Now)
	processedBefore := params.ProcessedUpdatesBefore.UTC()
	if params.ProcessedUpdatesBefore.IsZero() {
		processedBefore = now.Add(-7 * 24 * time.Hour)
	}
	batchSize := params.BatchSize
	if batchSize <= 0 {
		batchSize = 500
	}
	if batchSize > 5000 || !processedBefore.Before(now) {
		return CleanupResult{}, fmt.Errorf("%w: invalid cleanup bounds", ErrInvalidInput)
	}

	var cleaned CleanupResult
	err = db.Transaction(func(tx *gorm.DB) error {
		result := tx.Exec(`
            UPDATE whisper_drafts
            SET state = 'expired',
                ingest_lease_until = NULL,
                updated_at = ?
            WHERE state IN ('awaiting_media', 'ingesting_media')
              AND expires_at <= ?`, now, now)
		if result.Error != nil {
			return result.Error
		}
		cleaned.ExpiredDrafts = result.RowsAffected

		result = tx.Exec(`
            UPDATE whisper_drafts
            SET state = 'awaiting_media',
                ingest_lease_until = NULL,
                updated_at = ?
            WHERE state = 'ingesting_media'
              AND ingest_lease_until <= ?
              AND expires_at > ?`, now, now, now)
		if result.Error != nil {
			return result.Error
		}
		cleaned.ReleasedDraftIngests = result.RowsAffected

		result = tx.Exec(`
            UPDATE whispers
            SET status = 'expired',
                opening_callback_query_id = NULL,
                opening_reserved_at = NULL,
                opening_lease_until = NULL,
                publish_state = CASE WHEN publish_state = 'publishing' THEN 'failed' ELSE publish_state END,
                publish_lease_until = CASE WHEN publish_state = 'publishing' THEN NULL ELSE publish_lease_until END,
                last_publish_error = CASE WHEN publish_state = 'publishing' THEN 'expired' ELSE last_publish_error END,
                updated_at = ?
            WHERE status IN ('active', 'opening')
              AND expires_at <= ?`, now, now)
		if result.Error != nil {
			return result.Error
		}
		cleaned.ExpiredWhispers = result.RowsAffected

		result = tx.Exec(`
            UPDATE whispers
            SET status = 'active',
                opening_callback_query_id = NULL,
                opening_reserved_at = NULL,
                opening_lease_until = NULL,
                updated_at = ?
            WHERE status = 'opening'
              AND opening_lease_until <= ?
              AND expires_at > ?`, now, now, now)
		if result.Error != nil {
			return result.Error
		}
		cleaned.ReleasedOpenLeases = result.RowsAffected

		result = tx.Exec(`
            UPDATE whispers
            SET publish_state = 'retry_wait',
                publish_lease_until = NULL,
                next_publish_attempt_at = ?,
                last_publish_error = 'publish_lease_expired',
                updated_at = ?
            WHERE publish_state = 'publishing'
              AND publish_lease_until <= ?
              AND status = 'active'
              AND expires_at > ?`, now, now, now, now)
		if result.Error != nil {
			return result.Error
		}
		cleaned.ReleasedPublishLeases = result.RowsAffected

		result = tx.Exec(`
            UPDATE guest_secret_requests
            SET state = 'expired',
                ingest_lease_until = NULL,
                opening_lease_until = NULL,
                updated_at = ?
            WHERE state IN ('awaiting_secret', 'ingesting_secret', 'ready', 'opening')
              AND expires_at <= ?`, now, now)
		if result.Error != nil {
			return result.Error
		}

		result = tx.Exec(`
            UPDATE guest_secret_requests
            SET state = 'awaiting_secret', ingest_lease_until = NULL, updated_at = ?
            WHERE state = 'ingesting_secret' AND ingest_lease_until <= ? AND expires_at > ?`, now, now, now)
		if result.Error != nil {
			return result.Error
		}

		result = tx.Exec(`
            UPDATE guest_secret_requests
            SET state = 'ready', opening_reserved_at = NULL, opening_lease_until = NULL, updated_at = ?
            WHERE state = 'opening' AND opening_lease_until <= ? AND expires_at > ?`, now, now, now)
		if result.Error != nil {
			return result.Error
		}

		result = tx.Exec(`
            WITH doomed AS (
                SELECT id
                FROM whispers
                WHERE retention_delete_at <= ?
                ORDER BY retention_delete_at, id
                FOR UPDATE SKIP LOCKED
                LIMIT ?
            )
            DELETE FROM whispers AS whisper
            USING doomed
            WHERE whisper.id = doomed.id`, now, batchSize)
		if result.Error != nil {
			return result.Error
		}
		cleaned.DeletedWhispers = result.RowsAffected

		result = tx.Exec(`
            WITH doomed AS (
                SELECT telegram_update_id
                FROM processed_updates
                WHERE state = 'processed'
                  AND processed_at < ?
                ORDER BY processed_at, telegram_update_id
                FOR UPDATE SKIP LOCKED
                LIMIT ?
            )
            DELETE FROM processed_updates AS update_row
            USING doomed
            WHERE update_row.telegram_update_id = doomed.telegram_update_id`, processedBefore, batchSize)
		if result.Error != nil {
			return result.Error
		}
		cleaned.DeletedProcessedUpdates = result.RowsAffected

		result = tx.Exec(`
            WITH doomed AS (
                SELECT id
                FROM ephemeral_delete_jobs
                WHERE deleted_at < ?
                ORDER BY deleted_at, id
                FOR UPDATE SKIP LOCKED
                LIMIT ?
            )
            DELETE FROM ephemeral_delete_jobs AS job
            USING doomed
            WHERE job.id = doomed.id`, processedBefore, batchSize)
		if result.Error != nil {
			return result.Error
		}
		cleaned.DeletedEphemeralJobs = result.RowsAffected

		result = tx.Exec(`
            WITH doomed AS (
                SELECT id
                FROM guest_private_delete_jobs
                WHERE deleted_at < ?
                ORDER BY deleted_at, id
                FOR UPDATE SKIP LOCKED
                LIMIT ?
            )
            DELETE FROM guest_private_delete_jobs AS job
            USING doomed
            WHERE job.id = doomed.id`, processedBefore, batchSize)
		if result.Error != nil {
			return result.Error
		}
		cleaned.DeletedGuestJobs = result.RowsAffected

		result = tx.Exec(`
            WITH doomed AS (
                SELECT id
                FROM guest_secret_requests
                WHERE retention_delete_at <= ?
                ORDER BY retention_delete_at, id
                FOR UPDATE SKIP LOCKED
                LIMIT ?
            )
            DELETE FROM guest_secret_requests AS request
            USING doomed
            WHERE request.id = doomed.id`, now, batchSize)
		if result.Error != nil {
			return result.Error
		}
		cleaned.DeletedGuestRequests = result.RowsAffected
		return nil
	})
	if err != nil {
		return CleanupResult{}, translateError(err)
	}
	return cleaned, nil
}
