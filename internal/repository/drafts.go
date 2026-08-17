package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"gorm.io/gorm"
)

func (s *Store) CreateDraft(ctx context.Context, params CreateDraftParams) (domain.Draft, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return domain.Draft{}, err
	}
	composeHash := params.ComposeTokenHash
	if len(composeHash) == 0 {
		composeHash = params.Draft.ComposeTokenHash
	}
	if len(params.Draft.ComposeTokenHash) > 0 && !bytes.Equal(composeHash, params.Draft.ComposeTokenHash) {
		return domain.Draft{}, fmt.Errorf("%w: compose hash parameters disagree", ErrInvalidInput)
	}
	params.Draft.ComposeTokenHash = cloneBytes(composeHash)
	if params.SourceCommandMessageID == nil {
		params.SourceCommandMessageID = params.Draft.SourceCommandMessageID
	}
	if err := params.Draft.Validate(); err != nil {
		return domain.Draft{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if params.Draft.State != domain.DraftAwaitingMedia || len(composeHash) != sha256.Size {
		return domain.Draft{}, fmt.Errorf("%w: new draft must be awaiting media and have a SHA-256 compose hash", ErrInvalidInput)
	}

	now := nowOr(params.Now)
	maxActive := params.MaxActiveDrafts
	if maxActive == 0 {
		maxActive = 1
	}
	maxRecent := params.MaxRecentWhispers
	if maxRecent == 0 {
		maxRecent = 30
	}
	recentSince := params.RecentWhispersSince.UTC()
	if params.RecentWhispersSince.IsZero() {
		recentSince = now.Add(-time.Hour)
	}
	if maxActive != 1 || maxRecent <= 0 || !recentSince.Before(now) {
		return domain.Draft{}, fmt.Errorf("%w: invalid draft quota", ErrInvalidInput)
	}

	row := draftRow{
		ID:                     params.Draft.ID,
		ComposeTokenHash:       cloneBytes(composeHash),
		SenderID:               params.Draft.SenderID,
		RecipientID:            params.Draft.RecipientID,
		SourceChatID:           params.Draft.SourceChatID,
		SourceThreadID:         cloneInt64Pointer(params.Draft.SourceThreadID),
		SourceReplyMessageID:   cloneInt64Pointer(params.Draft.SourceReplyMessageID),
		SourceCommandMessageID: cloneInt64Pointer(params.SourceCommandMessageID),
		State:                  string(params.Draft.State),
		CreatedAt:              params.Draft.CreatedAt.UTC(),
		UpdatedAt:              params.Draft.CreatedAt.UTC(),
		ExpiresAt:              params.Draft.ExpiresAt.UTC(),
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", params.Draft.SenderID).Error; err != nil {
			return err
		}
		var active int64
		if err := tx.Model(&draftRow{}).
			Where("sender_id = ? AND state IN ? AND expires_at > ?", params.Draft.SenderID,
				[]string{string(domain.DraftAwaitingMedia), string(domain.DraftIngestingMedia)}, now).
			Count(&active).Error; err != nil {
			return err
		}
		if active >= int64(maxActive) {
			return ErrTooManyActiveDrafts
		}
		var recent int64
		if err := tx.Model(&whisperRow{}).
			Where("sender_id = ? AND created_at >= ?", params.Draft.SenderID, recentSince).
			Count(&recent).Error; err != nil {
			return err
		}
		if recent >= int64(maxRecent) {
			return ErrWhisperRateLimit
		}
		return tx.Create(&row).Error
	})
	if err != nil {
		return domain.Draft{}, translateError(err)
	}
	return row.toDomain(), nil
}

func (s *Store) CountActiveDrafts(ctx context.Context, senderID int64, now time.Time) (int64, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return 0, err
	}
	if senderID <= 0 {
		return 0, fmt.Errorf("%w: sender ID must be positive", ErrInvalidInput)
	}
	var count int64
	err = db.Model(&draftRow{}).
		Where("sender_id = ? AND state IN ? AND expires_at > ?", senderID,
			[]string{string(domain.DraftAwaitingMedia), string(domain.DraftIngestingMedia)}, nowOr(now)).
		Count(&count).Error
	return count, translateError(err)
}

func (s *Store) CountRecentWhispersBySender(ctx context.Context, senderID int64, since time.Time) (int64, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return 0, err
	}
	if senderID <= 0 || since.IsZero() {
		return 0, fmt.Errorf("%w: sender ID and since time are required", ErrInvalidInput)
	}
	var count int64
	err = db.Model(&whisperRow{}).
		Where("sender_id = ? AND created_at >= ?", senderID, since.UTC()).
		Count(&count).Error
	return count, translateError(err)
}

func (s *Store) FindDraftByComposeTokenHash(ctx context.Context, composeTokenHash []byte) (domain.Draft, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return domain.Draft{}, err
	}
	if len(composeTokenHash) != sha256.Size {
		return domain.Draft{}, fmt.Errorf("%w: compose token hash must be SHA-256", ErrInvalidInput)
	}
	var row draftRow
	if err := db.Where("compose_token_hash = ?", composeTokenHash).Take(&row).Error; err != nil {
		return domain.Draft{}, translateError(err)
	}
	return row.toDomain(), nil
}

func (s *Store) ClaimDraftIngest(ctx context.Context, params ClaimDraftIngestParams) (domain.Draft, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return domain.Draft{}, err
	}
	now := nowOr(params.Now)
	leaseUntil := params.LeaseUntil.UTC()
	if params.DraftID == uuid.Nil || params.SenderID <= 0 || !leaseUntil.After(now) {
		return domain.Draft{}, fmt.Errorf("%w: invalid draft ingest claim", ErrInvalidInput)
	}

	var row draftRow
	err = db.Raw(`
        UPDATE whisper_drafts
        SET state = 'ingesting_media',
            ingest_started_at = ?,
            ingest_lease_until = ?,
            updated_at = ?
        WHERE id = ?
          AND sender_id = ?
          AND expires_at > ?
          AND (
              state = 'awaiting_media'
              OR (state = 'ingesting_media' AND ingest_lease_until <= ?)
          )
        RETURNING *`, now, leaseUntil, now, params.DraftID, params.SenderID, now, now).
		Scan(&row).Error
	if err != nil {
		return domain.Draft{}, translateError(err)
	}
	if row.ID == uuid.Nil {
		return domain.Draft{}, ErrConflict
	}
	return row.toDomain(), nil
}

func (s *Store) ClaimLatestDraftIngest(ctx context.Context, senderID int64, now, leaseUntil time.Time) (domain.Draft, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return domain.Draft{}, err
	}
	now = nowOr(now)
	leaseUntil = leaseUntil.UTC()
	if senderID <= 0 || !leaseUntil.After(now) {
		return domain.Draft{}, fmt.Errorf("%w: invalid latest draft ingest claim", ErrInvalidInput)
	}

	var row draftRow
	err = db.Raw(`
        WITH candidate AS (
            SELECT id
            FROM whisper_drafts
            WHERE sender_id = ?
              AND expires_at > ?
              AND (
                  state = 'awaiting_media'
                  OR (state = 'ingesting_media' AND ingest_lease_until <= ?)
              )
            ORDER BY created_at DESC, id DESC
            FOR UPDATE SKIP LOCKED
            LIMIT 1
        )
        UPDATE whisper_drafts AS draft
        SET state = 'ingesting_media',
            ingest_started_at = ?,
            ingest_lease_until = ?,
            updated_at = ?
        FROM candidate
        WHERE draft.id = candidate.id
        RETURNING draft.*`, senderID, now, now, now, leaseUntil, now).
		Scan(&row).Error
	if err != nil {
		return domain.Draft{}, translateError(err)
	}
	if row.ID == uuid.Nil {
		return domain.Draft{}, ErrNotFound
	}
	return row.toDomain(), nil
}

func (s *Store) ReleaseDraftIngest(ctx context.Context, params ReleaseDraftIngestParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	if params.DraftID == uuid.Nil || params.SenderID <= 0 || params.ExpectedLeaseUntil.IsZero() {
		return fmt.Errorf("%w: invalid draft ingest release", ErrInvalidInput)
	}
	result := db.Model(&draftRow{}).
		Where("id = ? AND sender_id = ? AND state = 'ingesting_media' AND ingest_lease_until = ?",
			params.DraftID, params.SenderID, params.ExpectedLeaseUntil.UTC()).
		Updates(map[string]any{
			"state":              string(domain.DraftAwaitingMedia),
			"ingest_lease_until": nil,
			"updated_at":         now,
		})
	if result.Error != nil {
		return translateError(result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) CancelDraft(ctx context.Context, params CancelDraftParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	now := nowOr(params.Now)
	if params.DraftID == uuid.Nil || params.SenderID <= 0 {
		return fmt.Errorf("%w: invalid draft cancellation", ErrInvalidInput)
	}
	result := db.Model(&draftRow{}).
		Where("id = ? AND sender_id = ? AND state IN ?", params.DraftID, params.SenderID,
			[]string{string(domain.DraftAwaitingMedia), string(domain.DraftIngestingMedia)}).
		Updates(map[string]any{
			"state":              string(domain.DraftCancelled),
			"ingest_lease_until": nil,
			"cancelled_at":       now,
			"updated_at":         now,
		})
	if result.Error != nil {
		return translateError(result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrNotActive
	}
	return nil
}

func (s *Store) CancelLatestDraftForSender(ctx context.Context, senderID int64, now time.Time) (domain.Draft, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return domain.Draft{}, err
	}
	now = nowOr(now)
	if senderID <= 0 {
		return domain.Draft{}, fmt.Errorf("%w: sender ID must be positive", ErrInvalidInput)
	}

	var row draftRow
	err = db.Raw(`
        WITH candidate AS (
            SELECT id
            FROM whisper_drafts
            WHERE sender_id = ?
              AND state IN ('awaiting_media', 'ingesting_media')
              AND expires_at > ?
            ORDER BY created_at DESC, id DESC
            FOR UPDATE SKIP LOCKED
            LIMIT 1
        )
        UPDATE whisper_drafts AS draft
        SET state = 'cancelled',
            ingest_lease_until = NULL,
            cancelled_at = ?,
            updated_at = ?
        FROM candidate
        WHERE draft.id = candidate.id
        RETURNING draft.*`, senderID, now, now, now).
		Scan(&row).Error
	if err != nil {
		return domain.Draft{}, translateError(err)
	}
	if row.ID == uuid.Nil {
		return domain.Draft{}, ErrNotFound
	}
	return row.toDomain(), nil
}
