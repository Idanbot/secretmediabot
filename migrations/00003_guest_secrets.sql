-- +goose Up

CREATE TABLE guest_secret_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash BYTEA NOT NULL UNIQUE
        CHECK (octet_length(token_hash) = 32),
    sender_id BIGINT NOT NULL REFERENCES users(telegram_user_id),
    target_user_id BIGINT REFERENCES users(telegram_user_id),
    target_username TEXT,
    source_chat_id BIGINT REFERENCES chats(telegram_chat_id),
    source_thread_id BIGINT,
    source_message_id BIGINT,
    guest_query_id TEXT UNIQUE,
    inline_query_id TEXT UNIQUE,
    inline_message_id TEXT,
    state TEXT NOT NULL DEFAULT 'awaiting_secret'
        CHECK (state IN ('awaiting_secret', 'ingesting_secret', 'ready', 'opening', 'opened', 'expired', 'cancelled')),
    payload_kind TEXT CHECK (payload_kind IS NULL OR payload_kind IN ('text', 'media')),
    media_type TEXT CHECK (media_type IS NULL OR media_type IN ('photo', 'voice', 'video', 'audio', 'document')),
    telegram_file_id TEXT,
    telegram_file_unique_id TEXT,
    telegram_content_type TEXT,
    target_claimed_at TIMESTAMPTZ,
    ingest_started_at TIMESTAMPTZ,
    ingest_lease_until TIMESTAMPTZ,
    secret_ready_at TIMESTAMPTZ,
    opening_reserved_at TIMESTAMPTZ,
    opening_lease_until TIMESTAMPTZ,
    opened_at TIMESTAMPTZ,
    delivery_message_id BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    retention_delete_at TIMESTAMPTZ NOT NULL,
    CHECK (target_user_id IS NOT NULL OR NULLIF(btrim(target_username), '') IS NOT NULL),
    CHECK (source_chat_id IS NULL OR source_chat_id <> 0),
    CHECK (guest_query_id IS NOT NULL OR inline_query_id IS NOT NULL),
    CHECK ((state = 'ingesting_secret') = (ingest_lease_until IS NOT NULL)),
    CHECK (ingest_lease_until IS NULL OR (ingest_started_at IS NOT NULL AND ingest_lease_until > ingest_started_at)),
    CHECK ((state = 'opening') = (opening_lease_until IS NOT NULL)),
    CHECK (expires_at > created_at),
    CONSTRAINT guest_secret_requests_retention_check CHECK (retention_delete_at >= created_at),
    CHECK (
        (payload_kind IS NULL AND media_type IS NULL AND telegram_file_id IS NULL AND telegram_file_unique_id IS NULL AND telegram_content_type IS NULL)
        OR
        (payload_kind = 'text' AND media_type IS NULL AND telegram_file_id IS NULL AND telegram_file_unique_id IS NULL)
        OR
        (payload_kind = 'media' AND media_type IS NOT NULL AND telegram_file_id IS NOT NULL)
    ),
    CHECK (state <> 'opened' OR (opened_at IS NOT NULL AND delivery_message_id IS NOT NULL))
);

CREATE TABLE guest_secret_payloads (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id UUID NOT NULL REFERENCES guest_secret_requests(id) ON DELETE CASCADE,
    purpose TEXT NOT NULL CHECK (purpose IN ('text', 'media', 'caption')),
    encryption_algorithm TEXT NOT NULL DEFAULT 'AES-256-GCM',
    encryption_key_id TEXT NOT NULL,
    nonce BYTEA NOT NULL CHECK (octet_length(nonce) = 12),
    ciphertext BYTEA NOT NULL CHECK (octet_length(ciphertext) > 0),
    ciphertext_sha256 BYTEA NOT NULL CHECK (octet_length(ciphertext_sha256) = 32),
    content_type TEXT,
    plaintext_size_bytes BIGINT NOT NULL CHECK (plaintext_size_bytes > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    retention_delete_at TIMESTAMPTZ NOT NULL,
    UNIQUE (request_id, purpose),
    CHECK (retention_delete_at >= created_at)
);

CREATE TABLE guest_private_delete_jobs (
    id BIGSERIAL PRIMARY KEY,
    request_id UUID REFERENCES guest_secret_requests(id) ON DELETE SET NULL,
    chat_id BIGINT NOT NULL CHECK (chat_id > 0),
    message_id BIGINT NOT NULL CHECK (message_id > 0),
    delete_after TIMESTAMPTZ NOT NULL,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    lease_until TIMESTAMPTZ,
    last_error TEXT,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (chat_id, message_id),
    CHECK (next_attempt_at >= delete_after),
    CHECK (deleted_at IS NULL OR lease_until IS NULL)
);

CREATE INDEX idx_guest_requests_sender_state
    ON guest_secret_requests (sender_id, state, created_at DESC);

CREATE INDEX idx_guest_requests_target_state
    ON guest_secret_requests (target_user_id, state, created_at DESC);

-- +goose Down

DROP TABLE IF EXISTS guest_private_delete_jobs;
DROP TABLE IF EXISTS guest_secret_payloads;
DROP TABLE IF EXISTS guest_secret_requests;
