-- +goose Up

CREATE INDEX idx_guest_requests_expiry
    ON guest_secret_requests (expires_at)
    WHERE state IN ('awaiting_secret', 'ingesting_secret', 'ready', 'opening');

CREATE INDEX idx_guest_requests_ingest_leases
    ON guest_secret_requests (ingest_lease_until)
    WHERE state = 'ingesting_secret';

CREATE INDEX idx_guest_requests_opening_leases
    ON guest_secret_requests (opening_lease_until)
    WHERE state = 'opening';

CREATE INDEX idx_guest_requests_retention_cleanup
    ON guest_secret_requests (retention_delete_at, id);

CREATE INDEX idx_guest_payloads_retention_cleanup
    ON guest_secret_payloads (retention_delete_at, request_id);

CREATE INDEX idx_guest_delete_jobs_due
    ON guest_private_delete_jobs (next_attempt_at, id)
    WHERE deleted_at IS NULL;

CREATE INDEX idx_guest_delete_jobs_cleanup
    ON guest_private_delete_jobs (deleted_at, id)
    WHERE deleted_at IS NOT NULL;

CREATE INDEX idx_ephemeral_delete_jobs_cleanup
    ON ephemeral_delete_jobs (deleted_at, id)
    WHERE deleted_at IS NOT NULL;

CREATE INDEX idx_drafts_sender_created
    ON whisper_drafts (sender_id, created_at);

CREATE INDEX idx_drafts_terminal_cleanup
    ON whisper_drafts (updated_at, id)
    WHERE state IN ('completed', 'cancelled', 'expired');

CREATE INDEX idx_processed_updates_updated_cleanup
    ON processed_updates (updated_at, telegram_update_id);

-- +goose Down

DROP INDEX IF EXISTS idx_processed_updates_updated_cleanup;
DROP INDEX IF EXISTS idx_drafts_terminal_cleanup;
DROP INDEX IF EXISTS idx_drafts_sender_created;
DROP INDEX IF EXISTS idx_ephemeral_delete_jobs_cleanup;
DROP INDEX IF EXISTS idx_guest_delete_jobs_cleanup;
DROP INDEX IF EXISTS idx_guest_delete_jobs_due;
DROP INDEX IF EXISTS idx_guest_payloads_retention_cleanup;
DROP INDEX IF EXISTS idx_guest_requests_retention_cleanup;
DROP INDEX IF EXISTS idx_guest_requests_opening_leases;
DROP INDEX IF EXISTS idx_guest_requests_ingest_leases;
DROP INDEX IF EXISTS idx_guest_requests_expiry;
