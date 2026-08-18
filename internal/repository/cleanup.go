package repository

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// RunCleanup bounds every retention sweep. All state-machine sweeps are
// batched: a large backlog after downtime must not produce one unbounded
// transaction with long row locks and a WAL spike.
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
	identityBefore := now
	if params.IdentityRetention > 0 {
		identityBefore = now.Add(-params.IdentityRetention)
	}

	var cleaned CleanupResult
	err = db.Transaction(func(tx *gorm.DB) error {
		// Expire stale drafts and remember their senders so the worker can
		// send a best-effort expiry notice instead of a silent dead end.
		if err := tx.Raw(`
            UPDATE whisper_drafts AS draft
            SET state = 'expired', ingest_lease_until = NULL, updated_at = ?
            WHERE draft.id IN (
                SELECT id FROM whisper_drafts
                WHERE state IN ('awaiting_media', 'ingesting_media') AND expires_at <= ?
                ORDER BY expires_at, id
                FOR UPDATE SKIP LOCKED
                LIMIT ?
            )
            RETURNING draft.sender_id`, now, now, batchSize).Scan(&cleaned.ExpiredDraftSenderIDs).Error; err != nil {
			return err
		}
		cleaned.ExpiredDrafts = int64(len(cleaned.ExpiredDraftSenderIDs))

		result := tx.Exec(`
            UPDATE whisper_drafts AS draft
            SET state = 'awaiting_media', ingest_lease_until = NULL, updated_at = ?
            WHERE draft.id IN (
                SELECT id FROM whisper_drafts
                WHERE state = 'ingesting_media' AND ingest_lease_until <= ? AND expires_at > ?
                ORDER BY ingest_lease_until, id
                FOR UPDATE SKIP LOCKED
                LIMIT ?
            )`, now, now, now, batchSize)
		if result.Error != nil {
			return result.Error
		}
		cleaned.ReleasedDraftIngests = result.RowsAffected

		result = tx.Exec(`
            UPDATE whispers AS whisper
            SET status = 'expired',
                opening_callback_query_id = NULL,
                opening_reserved_at = NULL,
                opening_lease_until = NULL,
                publish_state = CASE WHEN publish_state = 'publishing' THEN 'failed' ELSE publish_state END,
                publish_lease_until = CASE WHEN publish_state = 'publishing' THEN NULL ELSE publish_lease_until END,
                last_publish_error = CASE WHEN publish_state = 'publishing' THEN 'expired' ELSE last_publish_error END,
                updated_at = ?
            WHERE whisper.id IN (
                SELECT id FROM whispers
                WHERE status IN ('active', 'opening') AND expires_at <= ?
                ORDER BY expires_at, id
                FOR UPDATE SKIP LOCKED
                LIMIT ?
            )`, now, now, batchSize)
		if result.Error != nil {
			return result.Error
		}
		cleaned.ExpiredWhispers = result.RowsAffected

		result = tx.Exec(`
            UPDATE whispers AS whisper
            SET status = 'active',
                opening_callback_query_id = NULL,
                opening_reserved_at = NULL,
                opening_lease_until = NULL,
                updated_at = ?
            WHERE whisper.id IN (
                SELECT id FROM whispers
                WHERE status = 'opening' AND opening_lease_until <= ? AND expires_at > ?
                ORDER BY opening_lease_until, id
                FOR UPDATE SKIP LOCKED
                LIMIT ?
            )`, now, now, now, batchSize)
		if result.Error != nil {
			return result.Error
		}
		cleaned.ReleasedOpenLeases = result.RowsAffected

		result = tx.Exec(`
            UPDATE whispers AS whisper
            SET publish_state = 'retry_wait',
                publish_lease_until = NULL,
                next_publish_attempt_at = ?,
                last_publish_error = 'publish_lease_expired',
                updated_at = ?
            WHERE whisper.id IN (
                SELECT id FROM whispers
                WHERE publish_state = 'publishing' AND publish_lease_until <= ? AND status = 'active' AND expires_at > ?
                ORDER BY publish_lease_until, id
                FOR UPDATE SKIP LOCKED
                LIMIT ?
            )`, now, now, now, now, batchSize)
		if result.Error != nil {
			return result.Error
		}
		cleaned.ReleasedPublishLeases = result.RowsAffected

		if err := tx.Raw(`
            UPDATE guest_secret_requests AS request
            SET state = 'expired', ingest_lease_until = NULL, opening_lease_until = NULL, updated_at = ?
            WHERE request.id IN (
                SELECT id FROM guest_secret_requests
                WHERE state IN ('awaiting_secret', 'ingesting_secret', 'ready', 'opening') AND expires_at <= ?
                ORDER BY expires_at, id
                FOR UPDATE SKIP LOCKED
                LIMIT ?
            )
            RETURNING request.sender_id`, now, now, batchSize).Scan(&cleaned.ExpiredGuestSenderIDs).Error; err != nil {
			return err
		}

		result = tx.Exec(`
            UPDATE guest_secret_requests AS request
            SET state = 'awaiting_secret', ingest_lease_until = NULL, updated_at = ?
            WHERE request.id IN (
                SELECT id FROM guest_secret_requests
                WHERE state = 'ingesting_secret' AND ingest_lease_until <= ? AND expires_at > ?
                ORDER BY ingest_lease_until, id
                FOR UPDATE SKIP LOCKED
                LIMIT ?
            )`, now, now, now, batchSize)
		if result.Error != nil {
			return result.Error
		}

		result = tx.Exec(`
            UPDATE guest_secret_requests AS request
            SET state = 'ready', opening_reserved_at = NULL, opening_lease_until = NULL, updated_at = ?
            WHERE request.id IN (
                SELECT id FROM guest_secret_requests
                WHERE state = 'opening' AND opening_lease_until <= ? AND expires_at > ?
                ORDER BY opening_lease_until, id
                FOR UPDATE SKIP LOCKED
                LIMIT ?
            )`, now, now, now, batchSize)
		if result.Error != nil {
			return result.Error
		}

		// Terminal drafts carry no secret; drop them once they are old and no
		// longer referenced by a retained whisper.
		result = tx.Exec(`
            WITH doomed AS (
                SELECT draft.id
                FROM whisper_drafts draft
                WHERE draft.state IN ('completed', 'cancelled', 'expired')
                  AND draft.updated_at < ?
                  AND NOT EXISTS (SELECT 1 FROM whispers w WHERE w.draft_id = draft.id)
                ORDER BY draft.updated_at, draft.id
                FOR UPDATE SKIP LOCKED
                LIMIT ?
            )
            DELETE FROM whisper_drafts AS draft
            USING doomed
            WHERE draft.id = doomed.id`, processedBefore, batchSize)
		if result.Error != nil {
			return result.Error
		}
		cleaned.DeletedDrafts = result.RowsAffected

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

		// Prune processed updates regardless of final state: rows in 'failed'
		// (including dead-lettered ones) and abandoned 'processing' leases have
		// no consumer once Telegram has moved on.
		result = tx.Exec(`
            WITH doomed AS (
                SELECT telegram_update_id
                FROM processed_updates
                WHERE updated_at < ?
                ORDER BY updated_at, telegram_update_id
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

		if params.IdentityRetention > 0 {
			if err := pruneObservedIdentities(tx, identityBefore, batchSize, &cleaned); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return CleanupResult{}, translateError(err)
	}
	return cleaned, nil
}

// pruneObservedIdentities removes long-unseen chat memberships and the users
// and chats that nothing references anymore. Composite foreign keys from
// drafts and whispers keep members that still have live references.
func pruneObservedIdentities(tx *gorm.DB, before time.Time, batchSize int, cleaned *CleanupResult) error {
	result := tx.Exec(`
        WITH doomed AS (
            SELECT member.chat_id, member.user_id
            FROM observed_chat_members member
            WHERE member.last_seen_at < ?
              AND NOT EXISTS (
                  SELECT 1 FROM whisper_drafts draft
                  WHERE draft.source_chat_id = member.chat_id
                    AND (draft.sender_id = member.user_id OR draft.recipient_id = member.user_id)
              )
              AND NOT EXISTS (
                  SELECT 1 FROM whispers whisper
                  WHERE whisper.source_chat_id = member.chat_id
                    AND (whisper.sender_id = member.user_id OR whisper.recipient_id = member.user_id)
              )
            ORDER BY member.last_seen_at, member.chat_id, member.user_id
            FOR UPDATE SKIP LOCKED
            LIMIT ?
        )
        DELETE FROM observed_chat_members AS member
        USING doomed
        WHERE member.chat_id = doomed.chat_id AND member.user_id = doomed.user_id`, before, batchSize)
	if result.Error != nil {
		return result.Error
	}
	cleaned.DeletedMembers = result.RowsAffected

	result = tx.Exec(`
        WITH doomed AS (
            SELECT "user".telegram_user_id
            FROM users "user"
            WHERE "user".last_seen_at < ?
              AND NOT "user".has_started_private_chat
              AND NOT EXISTS (SELECT 1 FROM observed_chat_members m WHERE m.user_id = "user".telegram_user_id)
              AND NOT EXISTS (
                  SELECT 1 FROM whisper_drafts draft
                  WHERE draft.sender_id = "user".telegram_user_id OR draft.recipient_id = "user".telegram_user_id
              )
              AND NOT EXISTS (
                  SELECT 1 FROM whispers whisper
                  WHERE whisper.sender_id = "user".telegram_user_id OR whisper.recipient_id = "user".telegram_user_id
              )
              AND NOT EXISTS (
                  SELECT 1 FROM guest_secret_requests guest
                  WHERE guest.sender_id = "user".telegram_user_id
                     OR guest.target_user_id = "user".telegram_user_id
              )
              AND NOT EXISTS (
                  SELECT 1 FROM ephemeral_delete_jobs job WHERE job.recipient_id = "user".telegram_user_id
              )
            ORDER BY "user".last_seen_at, "user".telegram_user_id
            FOR UPDATE SKIP LOCKED
            LIMIT ?
        )
        DELETE FROM users AS "user"
        USING doomed
        WHERE "user".telegram_user_id = doomed.telegram_user_id`, before, batchSize)
	if result.Error != nil {
		return result.Error
	}
	cleaned.DeletedUsers = result.RowsAffected

	result = tx.Exec(`
        WITH doomed AS (
            SELECT chat.telegram_chat_id
            FROM chats chat
            WHERE chat.last_seen_at < ?
              AND NOT EXISTS (SELECT 1 FROM observed_chat_members m WHERE m.chat_id = chat.telegram_chat_id)
              AND NOT EXISTS (SELECT 1 FROM whisper_drafts draft WHERE draft.source_chat_id = chat.telegram_chat_id)
              AND NOT EXISTS (SELECT 1 FROM whispers whisper WHERE whisper.source_chat_id = chat.telegram_chat_id)
              AND NOT EXISTS (
                  SELECT 1 FROM guest_secret_requests guest WHERE guest.source_chat_id = chat.telegram_chat_id
              )
            ORDER BY chat.last_seen_at, chat.telegram_chat_id
            FOR UPDATE SKIP LOCKED
            LIMIT ?
        )
        DELETE FROM chats AS chat
        USING doomed
        WHERE chat.telegram_chat_id = doomed.telegram_chat_id`, before, batchSize)
	if result.Error != nil {
		return result.Error
	}
	cleaned.DeletedChats = result.RowsAffected
	return nil
}
