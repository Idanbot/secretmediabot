-- +goose Up

CREATE INDEX idx_users_username_normalized
    ON users (username_normalized)
    WHERE username_normalized IS NOT NULL;

CREATE INDEX idx_observed_chat_members_user
    ON observed_chat_members (user_id, chat_id);

CREATE INDEX idx_observed_chat_members_recent
    ON observed_chat_members (chat_id, last_seen_at DESC);

CREATE UNIQUE INDEX idx_drafts_sender_active
    ON whisper_drafts (sender_id)
    WHERE state IN ('awaiting_media', 'ingesting_media');

CREATE INDEX idx_drafts_expiry
    ON whisper_drafts (expires_at)
    WHERE state IN ('awaiting_media', 'ingesting_media');

CREATE INDEX idx_drafts_ingest_leases
    ON whisper_drafts (ingest_lease_until)
    WHERE state = 'ingesting_media';

CREATE INDEX idx_whispers_recipient_active
    ON whispers (recipient_id, expires_at)
    WHERE status IN ('active', 'opening');

CREATE INDEX idx_whispers_expiry
    ON whispers (expires_at)
    WHERE status IN ('active', 'opening');

CREATE INDEX idx_whispers_publish_queue
    ON whispers (next_publish_attempt_at, created_at)
    WHERE publish_state IN ('pending', 'retry_wait');

CREATE INDEX idx_whispers_publish_leases
    ON whispers (publish_lease_until)
    WHERE publish_state = 'publishing';

CREATE INDEX idx_whispers_retention_cleanup
    ON whispers (retention_delete_at, id);

CREATE INDEX idx_media_blobs_retention_cleanup
    ON media_blobs (retention_delete_at, whisper_id);

CREATE INDEX idx_encrypted_text_payloads_retention_cleanup
    ON encrypted_text_payloads (retention_delete_at, whisper_id);

CREATE INDEX idx_open_events_whisper
    ON whisper_open_events (whisper_id, created_at DESC);

CREATE INDEX idx_open_events_actor
    ON whisper_open_events (telegram_user_id, created_at DESC);

CREATE INDEX idx_ephemeral_delete_jobs_due
    ON ephemeral_delete_jobs (next_attempt_at, id)
    WHERE deleted_at IS NULL;

CREATE INDEX idx_ephemeral_delete_jobs_whisper
    ON ephemeral_delete_jobs (whisper_id)
    WHERE whisper_id IS NOT NULL;

CREATE INDEX idx_processed_updates_cleanup
    ON processed_updates (processed_at)
    WHERE state = 'processed';

CREATE INDEX idx_processed_updates_leases
    ON processed_updates (lease_until)
    WHERE state = 'processing';

CREATE INDEX idx_owner_audit_owner
    ON owner_audit_events (owner_telegram_user_id, created_at DESC);

CREATE INDEX idx_owner_audit_whisper
    ON owner_audit_events (whisper_id, created_at DESC)
    WHERE whisper_id IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS idx_owner_audit_whisper;
DROP INDEX IF EXISTS idx_owner_audit_owner;
DROP INDEX IF EXISTS idx_processed_updates_leases;
DROP INDEX IF EXISTS idx_processed_updates_cleanup;
DROP INDEX IF EXISTS idx_ephemeral_delete_jobs_whisper;
DROP INDEX IF EXISTS idx_ephemeral_delete_jobs_due;
DROP INDEX IF EXISTS idx_open_events_actor;
DROP INDEX IF EXISTS idx_open_events_whisper;
DROP INDEX IF EXISTS idx_encrypted_text_payloads_retention_cleanup;
DROP INDEX IF EXISTS idx_media_blobs_retention_cleanup;
DROP INDEX IF EXISTS idx_whispers_retention_cleanup;
DROP INDEX IF EXISTS idx_whispers_publish_leases;
DROP INDEX IF EXISTS idx_whispers_publish_queue;
DROP INDEX IF EXISTS idx_whispers_expiry;
DROP INDEX IF EXISTS idx_whispers_recipient_active;
DROP INDEX IF EXISTS idx_drafts_ingest_leases;
DROP INDEX IF EXISTS idx_drafts_expiry;
DROP INDEX IF EXISTS idx_drafts_sender_active;
DROP INDEX IF EXISTS idx_observed_chat_members_recent;
DROP INDEX IF EXISTS idx_observed_chat_members_user;
DROP INDEX IF EXISTS idx_users_username_normalized;
