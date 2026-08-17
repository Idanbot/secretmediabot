-- +goose Up

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE users (
    telegram_user_id BIGINT PRIMARY KEY CHECK (telegram_user_id > 0),
    username TEXT,
    username_normalized TEXT GENERATED ALWAYS AS (
        NULLIF(lower(ltrim(btrim(username), '@')), '')
    ) STORED,
    first_name TEXT,
    last_name TEXT,
    is_bot BOOLEAN NOT NULL DEFAULT FALSE,
    language_code TEXT,
    has_started_private_chat BOOLEAN NOT NULL DEFAULT FALSE,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (last_seen_at >= first_seen_at)
);

CREATE TABLE chats (
    telegram_chat_id BIGINT PRIMARY KEY CHECK (telegram_chat_id <> 0),
    chat_type TEXT NOT NULL
        CHECK (chat_type IN ('private', 'group', 'supergroup', 'channel')),
    title TEXT,
    username TEXT,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (last_seen_at >= first_seen_at)
);

-- Username and numeric-ID recipient lookup must join through this table. This
-- prevents a username learned in one group from becoming a global directory.
CREATE TABLE observed_chat_members (
    chat_id BIGINT NOT NULL REFERENCES chats(telegram_chat_id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users(telegram_user_id) ON DELETE CASCADE,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (chat_id, user_id),
    CHECK (last_seen_at >= first_seen_at)
);

CREATE TABLE whisper_drafts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- SHA-256 of the opaque token carried by private-chat compose deep links.
    compose_token_hash BYTEA NOT NULL UNIQUE
        CHECK (octet_length(compose_token_hash) = 32),

    sender_id BIGINT NOT NULL REFERENCES users(telegram_user_id),
    recipient_id BIGINT NOT NULL REFERENCES users(telegram_user_id),
    source_chat_id BIGINT NOT NULL REFERENCES chats(telegram_chat_id),
    source_thread_id BIGINT,
    source_reply_message_id BIGINT,
    source_command_message_id BIGINT,

    state TEXT NOT NULL DEFAULT 'awaiting_media'
        CHECK (state IN (
            'awaiting_media',
            'ingesting_media',
            'completed',
            'cancelled',
            'expired'
        )),

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    ingest_started_at TIMESTAMPTZ,
    ingest_lease_until TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    cancelled_at TIMESTAMPTZ,

    FOREIGN KEY (source_chat_id, sender_id)
        REFERENCES observed_chat_members(chat_id, user_id),
    FOREIGN KEY (source_chat_id, recipient_id)
        REFERENCES observed_chat_members(chat_id, user_id),

    CHECK (sender_id <> recipient_id),
    CHECK (expires_at > created_at),
    CHECK ((state = 'ingesting_media') = (ingest_lease_until IS NOT NULL)),
    CHECK (ingest_started_at IS NULL OR ingest_started_at >= created_at),
    CHECK (
        ingest_lease_until IS NULL
        OR (ingest_started_at IS NOT NULL AND ingest_lease_until > ingest_started_at)
    ),
    CHECK (state <> 'completed' OR completed_at IS NOT NULL),
    CHECK (state <> 'cancelled' OR cancelled_at IS NOT NULL)
);

CREATE TABLE whispers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    draft_id UUID NOT NULL UNIQUE REFERENCES whisper_drafts(id),

    -- SHA-256 of the opaque callback token; plaintext callback tokens are not
    -- stored in PostgreSQL.
    open_token_hash BYTEA NOT NULL UNIQUE
        CHECK (octet_length(open_token_hash) = 32),

    sender_id BIGINT NOT NULL REFERENCES users(telegram_user_id),
    recipient_id BIGINT NOT NULL REFERENCES users(telegram_user_id),
    source_chat_id BIGINT NOT NULL REFERENCES chats(telegram_chat_id),
    source_thread_id BIGINT,

    payload_kind TEXT NOT NULL
        CHECK (payload_kind IN ('text', 'media')),
    media_provider TEXT
        CHECK (media_provider IS NULL OR media_provider = 'postgres_blob'),
    media_type TEXT
        CHECK (media_type IS NULL OR media_type IN ('photo', 'voice', 'video', 'audio', 'document')),
    telegram_file_id TEXT,
    telegram_file_unique_id TEXT,

    one_time BOOLEAN NOT NULL DEFAULT TRUE,
    protect_content BOOLEAN NOT NULL DEFAULT TRUE,

    status TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'opening', 'opened', 'expired', 'revoked')),

    -- Public envelope publication is a retryable state machine. A worker may
    -- reclaim rows whose publishing lease has expired.
    publish_state TEXT NOT NULL DEFAULT 'pending'
        CHECK (publish_state IN ('pending', 'publishing', 'published', 'retry_wait', 'failed')),
    publish_attempt_count INTEGER NOT NULL DEFAULT 0
        CHECK (publish_attempt_count >= 0),
    next_publish_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    publish_lease_until TIMESTAMPTZ,
    last_publish_error TEXT,
    public_message_id BIGINT,
    published_at TIMESTAMPTZ,

    -- One-time opens use a short lease so a crashed delivery attempt can be
    -- retried without allowing two concurrent recipients to reserve it.
    opening_callback_query_id TEXT,
    opening_reserved_at TIMESTAMPTZ,
    opening_lease_until TIMESTAMPTZ,
    opened_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    retention_delete_at TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '30 days'),

    FOREIGN KEY (source_chat_id, sender_id)
        REFERENCES observed_chat_members(chat_id, user_id),
    FOREIGN KEY (source_chat_id, recipient_id)
        REFERENCES observed_chat_members(chat_id, user_id),

    CHECK (sender_id <> recipient_id),
    CHECK (
        (payload_kind = 'text'
            AND media_provider IS NULL
            AND media_type IS NULL
            AND telegram_file_id IS NULL
            AND telegram_file_unique_id IS NULL)
        OR
        (payload_kind = 'media'
            AND media_provider = 'postgres_blob'
            AND media_type IS NOT NULL
            AND telegram_file_id IS NOT NULL)
    ),
    CHECK (expires_at > created_at),
    CHECK (retention_delete_at >= created_at),
    CHECK ((publish_state = 'publishing') = (publish_lease_until IS NOT NULL)),
    CHECK ((status = 'opening') = (opening_lease_until IS NOT NULL)),
    CHECK (
        publish_state <> 'published'
        OR (published_at IS NOT NULL AND public_message_id IS NOT NULL)
    ),
    CHECK (status <> 'opened' OR opened_at IS NOT NULL),
    CHECK (status <> 'revoked' OR revoked_at IS NOT NULL)
);

-- Kept separate so ordinary whisper lookups cannot accidentally select media
-- bytes. Deleting a retained whisper deletes its encrypted blob atomically.
CREATE TABLE media_blobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    whisper_id UUID NOT NULL UNIQUE REFERENCES whispers(id) ON DELETE CASCADE,
    encryption_algorithm TEXT NOT NULL DEFAULT 'AES-256-GCM',
    encryption_key_id TEXT NOT NULL,
    nonce BYTEA NOT NULL CHECK (octet_length(nonce) = 12),
    ciphertext BYTEA NOT NULL CHECK (octet_length(ciphertext) > 0),
    ciphertext_sha256 BYTEA NOT NULL
        CHECK (octet_length(ciphertext_sha256) = 32),
    content_type TEXT,
    plaintext_size_bytes BIGINT NOT NULL
        CHECK (plaintext_size_bytes >= 0 AND plaintext_size_bytes <= 20971520),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    retention_delete_at TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '30 days'),
    CHECK (retention_delete_at >= created_at)
);

-- Text-only secrets and optional media captions are encrypted independently.
-- The service transaction must create exactly one 'text' row for payload_kind
-- text, and zero or one 'caption' row for payload_kind media. Plain CHECK
-- constraints cannot assert the existence of a row in another table.
CREATE TABLE encrypted_text_payloads (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    whisper_id UUID NOT NULL REFERENCES whispers(id) ON DELETE CASCADE,
    purpose TEXT NOT NULL CHECK (purpose IN ('text', 'caption')),
    encryption_algorithm TEXT NOT NULL DEFAULT 'AES-256-GCM',
    encryption_key_id TEXT NOT NULL,
    nonce BYTEA NOT NULL CHECK (octet_length(nonce) = 12),
    ciphertext BYTEA NOT NULL CHECK (octet_length(ciphertext) > 0),
    ciphertext_sha256 BYTEA NOT NULL
        CHECK (octet_length(ciphertext_sha256) = 32),
    plaintext_size_bytes BIGINT NOT NULL CHECK (plaintext_size_bytes > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    retention_delete_at TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '30 days'),
    UNIQUE (whisper_id, purpose),
    CHECK (retention_delete_at >= created_at)
);

-- The publisher must reproduce callback_data after a crash, while a database
-- leak must not reveal a forgeable token. The application encrypts the raw
-- token with AAD bound to this immutable row ID and whisper ID. The row is
-- deleted immediately after the public envelope is durably marked published.
CREATE TABLE encrypted_callback_tokens (
    id UUID PRIMARY KEY,
    whisper_id UUID NOT NULL UNIQUE REFERENCES whispers(id) ON DELETE CASCADE,
    encryption_algorithm TEXT NOT NULL DEFAULT 'AES-256-GCM',
    encryption_key_id TEXT NOT NULL,
    nonce BYTEA NOT NULL CHECK (octet_length(nonce) = 12),
    ciphertext BYTEA NOT NULL CHECK (octet_length(ciphertext) > 0),
    ciphertext_sha256 BYTEA NOT NULL
        CHECK (octet_length(ciphertext_sha256) = 32),
    plaintext_size_bytes BIGINT NOT NULL CHECK (plaintext_size_bytes > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE whisper_open_events (
    id BIGSERIAL PRIMARY KEY,
    whisper_id UUID NOT NULL REFERENCES whispers(id) ON DELETE CASCADE,
    telegram_user_id BIGINT NOT NULL CHECK (telegram_user_id > 0),
    callback_query_id TEXT UNIQUE,
    outcome TEXT NOT NULL CHECK (outcome IN (
        'allowed',
        'denied_wrong_user',
        'denied_not_active',
        'denied_expired',
        'denied_revoked',
        'denied_already_opened',
        'delivery_failed'
    )),
    allowed BOOLEAN NOT NULL,
    denial_reason TEXT,
    delivery_state TEXT NOT NULL DEFAULT 'not_attempted'
        CHECK (delivery_state IN ('not_attempted', 'reserved', 'delivered', 'failed')),
    telegram_message_id BIGINT,
    delivery_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    CHECK (allowed = (outcome = 'allowed')),
    CHECK (
        (outcome = 'allowed' AND delivery_state IN ('reserved', 'delivered'))
        OR (outcome = 'delivery_failed' AND delivery_state = 'failed')
        OR (outcome NOT IN ('allowed', 'delivery_failed') AND delivery_state = 'not_attempted')
    )
);

-- Telegram does not expose read receipts for these messages. Delivery queues a
-- short best-effort deletion timer, persisted here so process restarts do not
-- silently turn ephemeral content into long-lived content.
CREATE TABLE ephemeral_delete_jobs (
    id BIGSERIAL PRIMARY KEY,
    chat_id BIGINT NOT NULL CHECK (chat_id <> 0),
    recipient_id BIGINT NOT NULL CHECK (recipient_id > 0),
    ephemeral_message_id BIGINT NOT NULL CHECK (ephemeral_message_id > 0),
    whisper_id UUID REFERENCES whispers(id) ON DELETE SET NULL,
    delete_after TIMESTAMPTZ NOT NULL,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    lease_until TIMESTAMPTZ,
    last_error TEXT,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (chat_id, recipient_id, ephemeral_message_id),
    CHECK (next_attempt_at >= delete_after),
    CHECK (deleted_at IS NULL OR lease_until IS NULL)
);

-- update_id is Telegram's idempotency key. Failed/abandoned processing leases
-- can be reclaimed while completed rows remain until cleanup retention passes.
CREATE TABLE processed_updates (
    telegram_update_id BIGINT PRIMARY KEY,
    update_type TEXT,
    payload_sha256 BYTEA CHECK (
        payload_sha256 IS NULL OR octet_length(payload_sha256) = 32
    ),
    state TEXT NOT NULL DEFAULT 'processing'
        CHECK (state IN ('processing', 'processed', 'failed')),
    attempt_count INTEGER NOT NULL DEFAULT 1 CHECK (attempt_count > 0),
    lease_until TIMESTAMPTZ,
    last_error TEXT,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((state = 'processing') = (lease_until IS NOT NULL)),
    CHECK (state <> 'processed' OR processed_at IS NOT NULL)
);

-- No foreign key is used for whisper_id: audit evidence must survive retention
-- deletion of the whisper and media blob it refers to.
CREATE TABLE owner_audit_events (
    id BIGSERIAL PRIMARY KEY,
    owner_telegram_user_id BIGINT NOT NULL CHECK (owner_telegram_user_id > 0),
    action TEXT NOT NULL CHECK (action IN (
        'view_metadata',
        'retrieve_content',
        'retrieve_media',
        'delete_media',
        'delete_whisper',
        'update_retention',
        'revoke_whisper'
    )),
    whisper_id UUID,
    success BOOLEAN NOT NULL DEFAULT TRUE,
    details JSONB NOT NULL DEFAULT '{}'::JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose Down

DROP TABLE IF EXISTS owner_audit_events;
DROP TABLE IF EXISTS processed_updates;
DROP TABLE IF EXISTS ephemeral_delete_jobs;
DROP TABLE IF EXISTS whisper_open_events;
DROP TABLE IF EXISTS encrypted_callback_tokens;
DROP TABLE IF EXISTS encrypted_text_payloads;
DROP TABLE IF EXISTS media_blobs;
DROP TABLE IF EXISTS whispers;
DROP TABLE IF EXISTS whisper_drafts;
DROP TABLE IF EXISTS observed_chat_members;
DROP TABLE IF EXISTS chats;
DROP TABLE IF EXISTS users;
DROP EXTENSION IF EXISTS pgcrypto;
