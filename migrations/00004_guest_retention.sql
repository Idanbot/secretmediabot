-- +goose Up

ALTER TABLE guest_secret_requests
    ADD COLUMN IF NOT EXISTS retention_delete_at TIMESTAMPTZ;

UPDATE guest_secret_requests
SET retention_delete_at = COALESCE(retention_delete_at, created_at + INTERVAL '30 days')
WHERE retention_delete_at IS NULL;

ALTER TABLE guest_secret_requests
    ALTER COLUMN retention_delete_at SET NOT NULL;

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'guest_secret_requests_retention_check'
    ) THEN
        ALTER TABLE guest_secret_requests
            ADD CONSTRAINT guest_secret_requests_retention_check
            CHECK (retention_delete_at >= created_at);
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down

ALTER TABLE guest_secret_requests
    DROP CONSTRAINT IF EXISTS guest_secret_requests_retention_check;

ALTER TABLE guest_secret_requests
    DROP COLUMN IF EXISTS retention_delete_at;
