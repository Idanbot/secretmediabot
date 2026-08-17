# Telegram Media Whisper Bot — V1 Build Specification

> **Goal:** Build a small, production-capable Telegram bot in Go that lets a user send a private photo, voice note, video, audio file, or document to one specific person from the context of a Telegram group.
>
> **V1 stack:** Go + Docker Compose + PostgreSQL. Redis is optional and intentionally omitted from the default stack.
>
> **V1 privacy model:** recipient-private in the Telegram UI, **not end-to-end encrypted**.
>
> **Target Bot API:** Telegram Bot API 10.2+ because the design uses Ephemeral Messages added on July 14, 2026.

---

## 1. Product in one sentence

A user replies to another group member with `/whisper`, sends media to the bot privately, and the bot leaves an **Open secret** button in the group; only the intended recipient can open it, and the media is delivered as a Telegram ephemeral message visible only to that user and the bot.

---

## 2. Why this architecture

The V1 deliberately keeps the infrastructure boring:

- **Go** for the bot/backend.
- **PostgreSQL** for users, group context, drafts, whisper metadata, TTLs, and state.
- **Telegram itself** as the media store by retaining Telegram `file_id`s.
- **No Redis initially.**
- **No S3/R2 initially.**
- **No Mini App initially.**
- **No local media volume.**
- Docker Compose runs the bot and PostgreSQL.
- Local development can use **long polling**, so no public HTTPS endpoint is required.
- Production can switch to a **webhook** without changing the domain logic.

This gives a useful V1 before adding cryptography, Cloudflare, queues, object storage, or multiple replicas.

---

# 3. Critical privacy distinction

## V1: Telegram-private delivery

The recipient sees the media privately because Telegram Bot API 10.2 supports outgoing ephemeral messages in groups with `receiver_user_id`.

However:

1. The sender gives the media to the bot.
2. Telegram supplies the bot a `file_id`.
3. The bot stores that `file_id`.

Therefore V1 must **not** claim:

- end-to-end encryption;
- zero knowledge;
- screenshot prevention;
- cryptographic self-destruction.

A truthful product description is:

> Secrets are delivered only to the selected Telegram recipient in the group UI.

`protect_content=true` should be enabled on delivered media to make Telegram clients restrict forwarding/saving where supported, but it is **not a DRM or screenshot guarantee**.

## Future E2EE mode

A later version can use a Telegram Mini App that encrypts media **on the sender's device before upload** and decrypts it **on the recipient's device**. That requires a real client-side key-management design and is intentionally out of scope for V1.

---

# 4. Telegram capability this relies on

Telegram Bot API 10.2 introduced Ephemeral Messages on **July 14, 2026**.

Important behavior:

- Bots can send group/supergroup messages visible only to a specified user and the bot.
- Media methods such as `sendPhoto`, `sendVideo`, `sendVoice`, `sendAudio`, and `sendDocument` accept `receiver_user_id`.
- A bot can respond ephemerally within **15 seconds** of a callback query by supplying `callback_query_id`.
- A chat-admin bot can send an ephemeral message to a non-bot chat member without a callback, but V1 should not require admin rights for basic secret opening.
- Telegram explicitly says ephemeral delivery is not guaranteed, particularly when the recipient is offline.
- Ephemeral messages may disappear automatically or after a client restart.

Official references:

- https://core.telegram.org/bots/api
- https://core.telegram.org/bots/api-changelog

---

# 5. Recommended V1 UX

## 5.1 One-time sender setup

A sender starts the bot in private once:

```text
/start
```

The bot stores the Telegram user ID and responds:

```text
🔐 Ready.

In a group containing this bot:
1. Reply to someone's message with /whisper
2. Send the secret media here when I ask
3. They will be able to open it privately from the group
```

A **recipient does not need to have started the bot** if opening is performed through the group callback flow.

---

## 5.2 Create a whisper

Group:

```text
Bob:
    I still don't believe you have that recording.

Alice:
    ↳ reply to Bob
    /whisper
```

The bot reads:

```text
source chat ID
Alice user ID
Bob user ID
Bob display information
reply/thread/topic context
```

It creates a short-lived draft in PostgreSQL.

If the bot can DM Alice because she has already started it:

```text
Bot → Alice private chat:

🔐 New whisper for Bob
Group: Weekend Chat

Send one:
🎙 voice
📷 photo
🎥 video
🎵 audio
📁 document

Draft expires in 10 minutes.

/cancel to abort.
```

If Alice has **not** started the bot, reply in the group with a Start Bot deep link. Admin mode can delete the original `/whisper` message afterward.

---

## 5.3 Sender submits media

Alice sends a voice message in the bot DM.

The bot:

1. validates that Alice owns an active draft;
2. detects the media type;
3. extracts Telegram `file_id` and `file_unique_id`;
4. moves the draft to `ready`;
5. inserts a `whispers` row;
6. posts the public envelope in the original group.

Example:

```text
🔐 Secret media
From: Alice
To: Bob
Expires in: 24h

[ 👁 Open secret ]
```

Anonymous mode can come later:

```text
🔐 Someone sent Bob a secret
[ 👁 Open secret ]
```

The public message contains **no secret media and no secret caption**.

---

## 5.4 Wrong user presses Open

Eve presses the button.

The callback contains Eve's Telegram user ID.

The backend loads the whisper and compares:

```text
callback.from.id == whisper.recipient_id
```

If false:

```text
answerCallbackQuery:
🔒 This whisper isn't for you.
```

No media is sent.

---

## 5.5 Correct recipient presses Open

Bob presses the button.

The bot:

1. receives the callback query;
2. verifies the opaque token;
3. verifies Bob's Telegram user ID;
4. verifies the whisper has not expired;
5. if `one_time=true`, atomically reserves/marks the open;
6. calls the correct Telegram media API within the callback eligibility window;
7. supplies:
   - original group `chat_id`;
   - Bob's `receiver_user_id`;
   - the callback's `callback_query_id`;
   - stored Telegram `file_id`;
   - `protect_content=true`.

Conceptually:

```json
{
  "chat_id": -1001234567890,
  "receiver_user_id": 99887766,
  "callback_query_id": "callback-query-id",
  "voice": "AwACAg...",
  "protect_content": true
}
```

Telegram renders the media in the group timeline **only for Bob and the bot**.

---

# 6. V1 functional requirements

## Required

- `/start`
- `/help`
- `/whisper` as a reply to another group member
- `/cancel` in private chat
- sender private media collection
- photo support
- voice-note support
- video support
- audio support
- document support
- recipient validation
- opaque callback token
- ephemeral media delivery
- `protect_content=true`
- 10-minute draft TTL
- configurable whisper expiry, default 24 hours
- one-time-open option in schema, even if UI initially defaults to false
- PostgreSQL persistence
- cleanup worker
- graceful shutdown
- structured logs
- health endpoint
- Dockerfile
- Docker Compose
- migrations
- `.env`
- `.env.example`
- tests for state transitions and authorization

## Explicitly not required in V1

- Redis
- Cloudflare R2
- Mini App
- E2EE
- arbitrary username lookup
- payments
- web dashboard
- multi-region
- Kubernetes
- Kafka/NATS
- object-storage copies of Telegram media

---

# 7. Recipient selection

## Primary method: reply-to-user

Prefer:

```text
reply to Bob:
    /whisper
```

over:

```text
/whisper @bob
```

Reasons:

- Telegram numeric user IDs are stable.
- Usernames are optional.
- Usernames can change.
- Bots do not have a convenient arbitrary member directory.
- A replied-to Telegram message gives an authoritative sender ID.

Reject:

- replies to the bot itself;
- anonymous admin/channel messages where a real recipient user cannot be resolved;
- bot recipients;
- missing reply targets.

## Optional later: known username cache

You can allow:

```text
/whisper @bob
```

only when `@bob` maps to a user previously observed by the bot.

Never treat username as the database identity. Use Telegram user ID as the primary identity.

---

# 8. Bot admin permissions

## V1 should work without requiring admin

The important open flow is initiated by the recipient pressing an inline callback button. The bot can use that callback query to send an ephemeral response.

This is preferable because requiring admin rights raises the installation barrier.

## Optional admin-enhanced UX

If the bot is an admin, it can improve UX by:

- deleting `/whisper` command messages;
- sending ephemeral prompts without waiting for callbacks;
- reducing group clutter;
- potentially sending a private-in-group recipient notification directly.

Treat admin mode as an enhancement, not a V1 dependency.

---

# 9. Project structure

```text
telegram-media-whisper/
├── cmd/
│   └── bot/
│       └── main.go
├── internal/
│   ├── app/
│   │   ├── app.go
│   │   └── worker.go
│   ├── config/
│   │   └── config.go
│   ├── domain/
│   │   ├── user.go
│   │   ├── draft.go
│   │   └── whisper.go
│   ├── telegram/
│   │   ├── client.go
│   │   ├── types.go
│   │   ├── updates.go
│   │   ├── handlers.go
│   │   ├── media.go
│   │   └── webhook.go
│   ├── service/
│   │   └── whisper_service.go
│   ├── repository/
│   │   ├── postgres.go
│   │   ├── users.go
│   │   ├── drafts.go
│   │   └── whispers.go
│   ├── httpserver/
│   │   └── server.go
│   └── token/
│       └── token.go
├── migrations/
│   ├── 00001_init.sql
│   └── 00002_indexes.sql
├── scripts/
│   ├── set-webhook.sh
│   └── delete-webhook.sh
├── .env.example
├── .gitignore
├── Dockerfile
├── compose.yaml
├── Makefile
├── go.mod
├── go.sum
└── README.md
```

---

# 10. Go dependency approach

Because Telegram Bot API 10.2 is very recent, do **not** make the architecture depend on a third-party Telegram library immediately supporting every new ephemeral parameter.

A good V1 is:

- `net/http` for Telegram Bot API calls;
- your own small typed Telegram client for the methods you actually use;
- `github.com/jackc/pgx/v5` for PostgreSQL;
- `github.com/google/uuid` or another UUID implementation;
- migration tool of choice, e.g. `github.com/pressly/goose/v3`;
- standard `log/slog` for structured logging.

The Telegram API wrapper only needs a small surface:

```text
getUpdates
setWebhook
deleteWebhook
sendMessage
sendPhoto
sendVideo
sendVoice
sendAudio
sendDocument
answerCallbackQuery
deleteMessage                  optional
getMe
```

Keeping this thin makes the new Bot API 10.2 fields easy to add yourself.

---

# 11. Configuration

Create a real `.env` locally and never commit it.

## `.env.example`

```dotenv
# -----------------------------------------------------------------------------
# Application
# -----------------------------------------------------------------------------
APP_ENV=development
LOG_LEVEL=debug
HTTP_ADDR=:8080

# polling | webhook
TELEGRAM_UPDATE_MODE=polling

# -----------------------------------------------------------------------------
# Telegram
# -----------------------------------------------------------------------------
TELEGRAM_BOT_TOKEN=replace_me
TELEGRAM_BOT_USERNAME=replace_me_bot

# Required only in webhook mode.
TELEGRAM_WEBHOOK_PUBLIC_URL=https://bot.example.com/telegram/webhook
TELEGRAM_WEBHOOK_SECRET=replace_with_long_random_value

# Long polling
TELEGRAM_POLL_TIMEOUT_SECONDS=30

# -----------------------------------------------------------------------------
# PostgreSQL
# -----------------------------------------------------------------------------
POSTGRES_DB=secretmediabot
POSTGRES_USER=secretmediabot
POSTGRES_PASSWORD=replace_me

DATABASE_URL=postgres://secretmediabot:replace_me@postgres:5432/secretmediabot?sslmode=disable

# -----------------------------------------------------------------------------
# Product behavior
# -----------------------------------------------------------------------------
DRAFT_TTL=10m
DEFAULT_WHISPER_TTL=24h
MAX_WHISPER_TTL=168h
DEFAULT_ONE_TIME=false
PROTECT_CONTENT=true

# Basic anti-abuse defaults
MAX_ACTIVE_DRAFTS_PER_USER=3
MAX_WHISPERS_PER_USER_PER_HOUR=30

# Optional comma-separated Telegram chat IDs.
# Empty means all groups are allowed.
ALLOWED_CHAT_IDS=

# Cleanup
CLEANUP_INTERVAL=5m
METADATA_RETENTION=168h

# -----------------------------------------------------------------------------
# Optional Redis - unused in default V1
# -----------------------------------------------------------------------------
REDIS_ENABLED=false
REDIS_URL=redis://redis:6379/0
```

Generate secrets with, for example:

```bash
openssl rand -hex 32
```

`.gitignore`:

```gitignore
.env
.env.*
!.env.example

bin/
dist/
coverage.out

.idea/
.vscode/
.DS_Store
```

---

# 12. Docker Compose

`compose.yaml`:

```yaml
services:
  bot:
    build:
      context: .
      dockerfile: Dockerfile
    restart: unless-stopped
    env_file:
      - .env
    depends_on:
      postgres:
        condition: service_healthy
    ports:
      - "127.0.0.1:8080:8080"
    networks:
      - whisper
    security_opt:
      - no-new-privileges:true
    read_only: true
    tmpfs:
      - /tmp
    stop_grace_period: 20s

  postgres:
    image: postgres:17-alpine
    restart: unless-stopped
    environment:
      POSTGRES_DB: ${POSTGRES_DB}
      POSTGRES_USER: ${POSTGRES_USER}
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
    volumes:
      - postgres_data:/var/lib/postgresql/data
    networks:
      - whisper
    healthcheck:
      test:
        [
          "CMD-SHELL",
          "pg_isready -U ${POSTGRES_USER} -d ${POSTGRES_DB}"
        ]
      interval: 5s
      timeout: 5s
      retries: 10
    security_opt:
      - no-new-privileges:true

  # Optional. Do not enable until the application actually uses Redis.
  redis:
    image: redis:7-alpine
    restart: unless-stopped
    profiles:
      - redis
    command:
      [
        "redis-server",
        "--appendonly",
        "no",
        "--save",
        ""
      ]
    networks:
      - whisper
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 3s
      retries: 10

volumes:
  postgres_data:

networks:
  whisper:
```

Run default V1:

```bash
docker compose up -d --build
```

Run later with Redis profile:

```bash
docker compose --profile redis up -d
```

---

# 13. Dockerfile

```dockerfile
FROM golang:1.26-alpine AS build

WORKDIR /src

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown

RUN CGO_ENABLED=0 GOOS=linux \
    go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/secretmediabot \
    ./cmd/bot


FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/secretmediabot /app/secretmediabot

USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/app/secretmediabot"]
```

If the chosen migration tool needs files available at runtime, either:

1. embed SQL migrations into the Go binary with `//go:embed`; or
2. run migrations as a separate Compose job.

Embedding is convenient for this project.

---

# 14. Database schema

## 14.1 Initial migration

`migrations/00001_init.sql`:

```sql
-- +goose Up

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE users (
    telegram_user_id BIGINT PRIMARY KEY,
    username TEXT,
    first_name TEXT,
    last_name TEXT,
    has_started_private_chat BOOLEAN NOT NULL DEFAULT FALSE,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE chats (
    telegram_chat_id BIGINT PRIMARY KEY,
    chat_type TEXT NOT NULL,
    title TEXT,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE whisper_drafts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    sender_id BIGINT NOT NULL REFERENCES users(telegram_user_id),
    recipient_id BIGINT NOT NULL REFERENCES users(telegram_user_id),
    source_chat_id BIGINT NOT NULL REFERENCES chats(telegram_chat_id),

    source_thread_id BIGINT,
    source_reply_message_id BIGINT,

    state TEXT NOT NULL DEFAULT 'awaiting_media'
        CHECK (state IN ('awaiting_media', 'completed', 'cancelled', 'expired')),

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,

    CHECK (sender_id <> recipient_id)
);

CREATE TABLE whispers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Short unguessable token placed in callback_data.
    -- Store only a hash if you want callback tokens to be non-recoverable
    -- from a DB dump.
    open_token_hash BYTEA NOT NULL UNIQUE,

    sender_id BIGINT NOT NULL REFERENCES users(telegram_user_id),
    recipient_id BIGINT NOT NULL REFERENCES users(telegram_user_id),
    source_chat_id BIGINT NOT NULL REFERENCES chats(telegram_chat_id),

    source_thread_id BIGINT,

    media_type TEXT NOT NULL
        CHECK (media_type IN ('photo', 'voice', 'video', 'audio', 'document')),

    telegram_file_id TEXT NOT NULL,
    telegram_file_unique_id TEXT,

    one_time BOOLEAN NOT NULL DEFAULT FALSE,
    protect_content BOOLEAN NOT NULL DEFAULT TRUE,

    status TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'opened', 'expired', 'revoked')),

    public_message_id BIGINT,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    opened_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,

    CHECK (sender_id <> recipient_id)
);

CREATE TABLE whisper_open_events (
    id BIGSERIAL PRIMARY KEY,
    whisper_id UUID NOT NULL REFERENCES whispers(id) ON DELETE CASCADE,
    telegram_user_id BIGINT NOT NULL,
    allowed BOOLEAN NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose Down

DROP TABLE IF EXISTS whisper_open_events;
DROP TABLE IF EXISTS whispers;
DROP TABLE IF EXISTS whisper_drafts;
DROP TABLE IF EXISTS chats;
DROP TABLE IF EXISTS users;
```

## 14.2 Index migration

`migrations/00002_indexes.sql`:

```sql
-- +goose Up

CREATE INDEX idx_drafts_sender_active
    ON whisper_drafts (sender_id, expires_at)
    WHERE state = 'awaiting_media';

CREATE INDEX idx_drafts_expiry
    ON whisper_drafts (expires_at)
    WHERE state = 'awaiting_media';

CREATE INDEX idx_whispers_recipient_active
    ON whispers (recipient_id, expires_at)
    WHERE status = 'active';

CREATE INDEX idx_whispers_expiry
    ON whispers (expires_at)
    WHERE status = 'active';

CREATE INDEX idx_open_events_whisper
    ON whisper_open_events (whisper_id, created_at DESC);

-- +goose Down

DROP INDEX IF EXISTS idx_open_events_whisper;
DROP INDEX IF EXISTS idx_whispers_expiry;
DROP INDEX IF EXISTS idx_whispers_recipient_active;
DROP INDEX IF EXISTS idx_drafts_expiry;
DROP INDEX IF EXISTS idx_drafts_sender_active;
```

---

# 15. Why keep `file_id`, not the actual media

Telegram documents that media already stored on Telegram can be sent again using its `file_id`.

For V1 that means PostgreSQL needs:

```text
telegram_file_id
media_type
sender_id
recipient_id
chat_id
expiry
status
```

rather than:

```text
actual MP4
actual OGG
actual JPEG
```

Advantages:

- no object storage;
- no disk persistence;
- no streaming code;
- no file cleanup;
- no egress costs;
- tiny PostgreSQL database;
- simpler backups.

The privacy consequence is important: this is not zero-knowledge storage.

---

# 16. Token design for callback buttons

Telegram callback data is small, so do not put a large JSON document in the button.

Use:

```text
w:<base64url-random-token>
```

Example:

```text
w:h2jSFKQq8zw44xWJ0SQu8A
```

Generate 16-32 random bytes using `crypto/rand`.

Store:

```text
SHA-256(token)
```

in PostgreSQL instead of the plaintext token.

When callback arrives:

```go
rawToken := strings.TrimPrefix(callback.Data, "w:")
hash := sha256.Sum256([]byte(rawToken))

whisper := repo.FindByOpenTokenHash(ctx, hash[:])
```

This makes a database-only leak less useful for constructing valid callback payloads.

Authorization must still be based on:

```go
callback.From.ID == whisper.RecipientID
```

The token is not a replacement for recipient authorization.

---

# 17. Domain models

```go
type MediaType string

const (
    MediaPhoto    MediaType = "photo"
    MediaVoice    MediaType = "voice"
    MediaVideo    MediaType = "video"
    MediaAudio    MediaType = "audio"
    MediaDocument MediaType = "document"
)

type Whisper struct {
    ID                   uuid.UUID
    SenderID             int64
    RecipientID          int64
    SourceChatID         int64
    SourceThreadID       *int64
    MediaType            MediaType
    TelegramFileID       string
    TelegramFileUniqueID *string
    OneTime              bool
    ProtectContent       bool
    Status               string
    PublicMessageID      *int64
    CreatedAt            time.Time
    ExpiresAt            time.Time
    OpenedAt             *time.Time
}
```

Draft:

```go
type Draft struct {
    ID                   uuid.UUID
    SenderID             int64
    RecipientID          int64
    SourceChatID         int64
    SourceThreadID       *int64
    SourceReplyMessageID *int64
    State                string
    CreatedAt            time.Time
    ExpiresAt            time.Time
}
```

---

# 18. Core state machine

```text
              /whisper reply
                    │
                    ▼
             AWAITING_MEDIA
              │          │
      media   │          │ cancel/TTL
              ▼          ▼
          COMPLETED   CANCELLED/EXPIRED
              │
              ▼
          WHISPER ACTIVE
            │       │
     open    │       │ TTL/revoke
            ▼       ▼
        OPENED*   EXPIRED/REVOKED

* only terminal when one_time=true
```

For reusable whispers, successful opens can create an open event without changing `status` from `active`.

---

# 19. Media extraction

From a private incoming Telegram `Message`:

```go
type ExtractedMedia struct {
    Type         MediaType
    FileID       string
    FileUniqueID string
}
```

Rules:

```text
message.Voice    -> voice
message.Photo    -> photo, choose largest PhotoSize
message.Video    -> video
message.Audio    -> audio
message.Document -> document
```

Ignore or explicitly reject initially:

- stickers;
- animations/GIFs;
- video notes;
- albums/media groups;
- contacts;
- locations;
- polls.

They can be added later.

For photos, Telegram supplies several sizes. Store the `file_id` from the largest appropriate `PhotoSize`.

---

# 20. Telegram API client

Use a generic method:

```go
func (c *Client) call(
    ctx context.Context,
    method string,
    body any,
    out any,
) error
```

Base URL:

```text
https://api.telegram.org/bot<TELEGRAM_BOT_TOKEN>/<METHOD>
```

Never log the full URL because it contains the bot token.

Prefer constructing it internally and log only:

```text
telegram_method=sendVoice
status=200
duration_ms=42
```

---

# 21. Ephemeral media request types

Example for voice:

```go
type SendVoiceRequest struct {
    ChatID          int64  `json:"chat_id"`
    ReceiverUserID  int64  `json:"receiver_user_id"`
    CallbackQueryID string `json:"callback_query_id"`
    Voice           string `json:"voice"`
    ProtectContent  bool   `json:"protect_content,omitempty"`
}
```

Photo:

```go
type SendPhotoRequest struct {
    ChatID          int64  `json:"chat_id"`
    ReceiverUserID  int64  `json:"receiver_user_id"`
    CallbackQueryID string `json:"callback_query_id"`
    Photo           string `json:"photo"`
    ProtectContent  bool   `json:"protect_content,omitempty"`
}
```

Repeat for video/audio/document.

A dispatcher keeps business logic clean:

```go
func (c *Client) SendEphemeralMedia(
    ctx context.Context,
    chatID int64,
    recipientID int64,
    callbackQueryID string,
    mediaType MediaType,
    fileID string,
    protectContent bool,
) error {
    switch mediaType {
    case MediaVoice:
        return c.sendVoice(...)
    case MediaPhoto:
        return c.sendPhoto(...)
    case MediaVideo:
        return c.sendVideo(...)
    case MediaAudio:
        return c.sendAudio(...)
    case MediaDocument:
        return c.sendDocument(...)
    default:
        return ErrUnsupportedMedia
    }
}
```

---

# 22. Callback handler

Pseudo-production logic:

```go
func (h *Handler) HandleOpenWhisper(
    ctx context.Context,
    q CallbackQuery,
) error {
    token, ok := parseOpenToken(q.Data)
    if !ok {
        return h.tg.AnswerCallbackQuery(ctx, q.ID, "Invalid whisper.")
    }

    whisper, err := h.whispers.FindByToken(ctx, token)
    if errors.Is(err, repository.ErrNotFound) {
        return h.tg.AnswerCallbackQuery(ctx, q.ID, "This whisper no longer exists.")
    }
    if err != nil {
        return err
    }

    if q.From.ID != whisper.RecipientID {
        _ = h.whispers.RecordOpenAttempt(ctx, whisper.ID, q.From.ID, false)
        return h.tg.AnswerCallbackQuery(
            ctx,
            q.ID,
            "🔒 This whisper isn't for you.",
        )
    }

    now := time.Now()

    if now.After(whisper.ExpiresAt) || whisper.Status == "expired" {
        return h.tg.AnswerCallbackQuery(ctx, q.ID, "⌛ This whisper expired.")
    }

    if whisper.Status == "revoked" {
        return h.tg.AnswerCallbackQuery(ctx, q.ID, "🚫 This whisper was revoked.")
    }

    if whisper.OneTime {
        claimed, err := h.whispers.ClaimOneTimeOpen(
            ctx,
            whisper.ID,
            whisper.RecipientID,
            now,
        )
        if err != nil {
            return err
        }
        if !claimed {
            return h.tg.AnswerCallbackQuery(
                ctx,
                q.ID,
                "This one-time whisper was already opened.",
            )
        }
    }

    err = h.tg.SendEphemeralMedia(
        ctx,
        whisper.SourceChatID,
        whisper.RecipientID,
        q.ID,
        whisper.MediaType,
        whisper.TelegramFileID,
        whisper.ProtectContent,
    )
    if err != nil {
        // Decide whether a failed Telegram delivery should release a
        // one-time claim. Prefer an explicit state such as opening/delivered
        // if you need perfect semantics.
        return err
    }

    _ = h.whispers.RecordOpenAttempt(ctx, whisper.ID, q.From.ID, true)

    return nil
}
```

---

# 23. One-time open atomicity

Do not implement:

```text
SELECT status
if active:
    UPDATE opened
```

as two independent operations.

Two near-simultaneous callback requests could both pass.

Use one atomic PostgreSQL statement:

```sql
UPDATE whispers
SET
    status = 'opened',
    opened_at = NOW()
WHERE
    id = $1
    AND recipient_id = $2
    AND one_time = TRUE
    AND status = 'active'
    AND expires_at > NOW()
RETURNING id;
```

If no row is returned, the one-time open was already consumed, expired, or revoked.

For a more exact delivery guarantee later, introduce:

```text
active -> opening -> opened
```

and a short lease so a transient Telegram API failure does not permanently consume the secret.

V1 can accept the simpler semantics and document them.

---

# 24. Creating a draft

On a group `/whisper` command:

Validate:

```text
message.chat.type in {group, supergroup}
message.from is present
message.reply_to_message is present
reply target is a real user
target is not a bot
target != sender
chat is allowed
sender has <= MAX_ACTIVE_DRAFTS_PER_USER
```

Upsert sender, recipient, and chat metadata.

Insert:

```sql
INSERT INTO whisper_drafts (
    sender_id,
    recipient_id,
    source_chat_id,
    source_thread_id,
    source_reply_message_id,
    expires_at
)
VALUES (
    $1,
    $2,
    $3,
    $4,
    $5,
    NOW() + INTERVAL '10 minutes'
)
RETURNING *;
```

Then try to DM the sender.

If Telegram rejects the private DM because the user has never started the bot, respond with a deep link:

```text
https://t.me/<bot_username>?start=compose_<opaque_draft_token>
```

Do not expose raw sequential IDs.

---

# 25. Handling private media

When media arrives in a private bot chat:

1. identify sender;
2. find their newest non-expired `awaiting_media` draft;
3. if zero drafts: explain how to create one;
4. if multiple drafts: choose the latest only if your UX guarantees this, otherwise ask which draft;
5. extract media;
6. create whisper;
7. mark draft completed;
8. send the envelope to the group.

A stricter design stores `active_draft_id` associated with the private conversation. PostgreSQL is enough.

---

# 26. Envelope message

Example:

```text
🔐 Secret media
From: Alice
To: Bob
Expires in 24h
```

Inline keyboard:

```json
{
  "inline_keyboard": [
    [
      {
        "text": "👁 Open secret",
        "callback_data": "w:OPAQUE_TOKEN"
      }
    ]
  ]
}
```

Do not include:

- original caption;
- file name if it could leak secret content;
- duration if you want maximum metadata privacy;
- thumbnail;
- media type if you want to hide that metadata.

You can choose whether the envelope says:

```text
Secret voice message
```

or only:

```text
Secret media
```

V1 recommendation: use **Secret media**.

---

# 27. PostgreSQL cleanup worker

A simple Go ticker is sufficient:

```go
ticker := time.NewTicker(cfg.CleanupInterval)
defer ticker.Stop()

for {
    select {
    case <-ctx.Done():
        return
    case <-ticker.C:
        if err := cleanup(ctx); err != nil {
            logger.Error("cleanup failed", "err", err)
        }
    }
}
```

Cleanup queries:

```sql
UPDATE whisper_drafts
SET state = 'expired'
WHERE
    state = 'awaiting_media'
    AND expires_at <= NOW();
```

```sql
UPDATE whispers
SET status = 'expired'
WHERE
    status = 'active'
    AND expires_at <= NOW();
```

Delete stale event metadata after your configured retention:

```sql
DELETE FROM whisper_open_events
WHERE created_at < NOW() - INTERVAL '7 days';
```

Optionally delete expired whisper metadata completely:

```sql
DELETE FROM whispers
WHERE
    status IN ('expired', 'revoked', 'opened')
    AND expires_at < NOW() - INTERVAL '7 days';
```

Because PostgreSQL may retain deleted data in backups/WAL for some period, do not promise immediate cryptographic erasure.

---

# 28. Long polling vs webhook

## Development: long polling

Recommended first.

Benefits:

- no domain;
- no tunnel;
- no TLS;
- no reverse proxy;
- works inside Docker Compose;
- easy debugging.

Telegram update loop:

```text
getUpdates(offset, timeout=30)
process updates
advance offset
```

Only one active poller should consume the bot updates.

## Production: webhook

Use when you have a stable public HTTPS endpoint.

Endpoint:

```text
POST /telegram/webhook
```

Set a Telegram webhook secret and verify the expected Telegram secret header before parsing the body.

Also expose:

```text
GET /healthz
GET /readyz
```

Do not expose metrics containing Telegram IDs unless you deliberately pseudonymize them.

---

# 29. Webhook setup script

`scripts/set-webhook.sh`:

```bash
#!/usr/bin/env sh
set -eu

: "${TELEGRAM_BOT_TOKEN:?missing TELEGRAM_BOT_TOKEN}"
: "${TELEGRAM_WEBHOOK_PUBLIC_URL:?missing TELEGRAM_WEBHOOK_PUBLIC_URL}"
: "${TELEGRAM_WEBHOOK_SECRET:?missing TELEGRAM_WEBHOOK_SECRET}"

curl --fail-with-body \
  --request POST \
  "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/setWebhook" \
  --header 'Content-Type: application/json' \
  --data "$(cat <<EOF
{
  "url": "${TELEGRAM_WEBHOOK_PUBLIC_URL}",
  "secret_token": "${TELEGRAM_WEBHOOK_SECRET}",
  "allowed_updates": ["message", "callback_query"]
}
EOF
)"
```

Delete webhook before returning to polling:

```bash
curl --fail-with-body \
  "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/deleteWebhook?drop_pending_updates=false"
```

---

# 30. HTTP server

Recommended endpoints:

```text
GET  /healthz
GET  /readyz
POST /telegram/webhook
```

`/healthz`:

```json
{"status":"ok"}
```

`/readyz` should validate:

- application initialized;
- PostgreSQL reachable;
- update mode configured.

Do not call Telegram on every readiness probe.

---

# 31. Logging

Use `log/slog`.

Good:

```text
level=INFO
event=whisper_created
whisper_id=019...
sender_user_hash=...
recipient_user_hash=...
chat_hash=...
media_type=voice
expires_at=...
```

Avoid logging:

- Telegram bot token;
- webhook secret;
- database password;
- full incoming Telegram update JSON;
- captions;
- filenames;
- Telegram `file_id`;
- private message content;
- callback raw token.

For development, it is tempting to dump entire Telegram payloads. Make this an explicit temporary debug option and never enable it in production.

---

# 32. Pseudonymizing IDs in logs

Instead of:

```text
sender_id=123456789
```

derive:

```text
HMAC-SHA256(LOG_HASH_KEY, decimalTelegramID)
```

and log the first several bytes.

Add later if desired:

```dotenv
LOG_HASH_KEY=replace_me
```

This lets you correlate events without writing raw Telegram IDs into log storage.

---

# 33. Security checklist

## Secrets

- `.env` in `.gitignore`.
- Never bake `.env` into the image.
- Restrict host filesystem permissions on `.env`.
- Rotate the Telegram bot token if leaked.
- Use a strong PostgreSQL password.
- Use a strong webhook secret.
- Do not print `DATABASE_URL` if it contains credentials.

## Network

Default Compose:

- PostgreSQL has **no host port**.
- Redis, if enabled, has **no host port**.
- bot HTTP port binds to `127.0.0.1` by default.
- use a reverse proxy/tunnel for webhook mode.

## Telegram authorization

Never authorize based on:

```text
username
button text
display name
callback token alone
```

Authorize based on:

```text
callback.from.id == stored recipient Telegram user ID
```

## Media

- reject unsupported media;
- set reasonable file/duration policies;
- do not download media in V1 unless needed;
- never create a public Telegram media message containing the secret;
- set `protect_content=true`.

## Database

- parameterized SQL only;
- least-privileged DB user in production;
- no plaintext application secrets stored in the database;
- consider encrypted backups;
- keep metadata retention short.

## HTTP

- request body size limit;
- sensible server timeouts;
- webhook secret validation;
- no debug endpoints in production;
- rate limit abusive endpoints if public.

---

# 34. Recommended HTTP timeouts

```go
srv := &http.Server{
    Addr:              cfg.HTTPAddr,
    Handler:           router,
    ReadHeaderTimeout: 5 * time.Second,
    ReadTimeout:       10 * time.Second,
    WriteTimeout:      15 * time.Second,
    IdleTimeout:       60 * time.Second,
}
```

Telegram callback handling should be quick. Do not enqueue recipient-open handling behind a slow worker because ephemeral callback-based sending is time-sensitive.

---

# 35. Rate limiting without Redis

PostgreSQL is enough at first.

For example:

```sql
SELECT COUNT(*)
FROM whispers
WHERE
    sender_id = $1
    AND created_at >= NOW() - INTERVAL '1 hour';
```

Reject above the configured threshold.

For moderate traffic this is fine with an index.

You can also use a small in-process token bucket if you run one replica.

---

# 36. Do you need Redis?

## No, not for V1

PostgreSQL already handles:

- drafts;
- TTL timestamps;
- authorization state;
- one-time open atomicity;
- rate-limit counters at low/moderate scale;
- cleanup;
- multi-process consistency.

## Add Redis when one of these becomes real

1. **High-volume rate limiting**
2. **Very high churn temporary state**
3. **Distributed locks**
4. **Multiple bot replicas with hot shared state**
5. **Queues**
6. **Short-lived caches**
7. **Expiring keys where Redis TTL semantics simplify code**

Possible later keys:

```text
rate:user:<telegram-id>
draft-active:<telegram-id>
callback-dedupe:<callback-id>
open-lock:<whisper-id>
```

Do not add Redis only because "bots usually use Redis."

---

# 37. Concurrent update handling

Telegram may deliver unrelated updates concurrently.

A good model:

```text
poll/webhook
    │
    ▼
parse update
    │
    ├── message handler
    └── callback handler
```

Keep update handlers stateless except through repositories.

Do not keep critical draft ownership only in:

```go
map[int64]Draft
```

because a process restart would lose it.

PostgreSQL is the source of truth.

---

# 38. Idempotency

Webhook requests may be retried and polling code can accidentally process an update twice.

Add a processed update table if duplicates become problematic:

```sql
CREATE TABLE processed_updates (
    update_id BIGINT PRIMARY KEY,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

Then transactionally claim the update.

For a small V1, you can initially ensure update offsets are handled carefully in polling and make operations naturally idempotent. Before public launch, explicit update deduplication is recommended.

---

# 39. Transactions

Use a transaction when private media finalizes a draft:

```text
BEGIN
  lock draft
  validate active + not expired
  create whisper
  mark draft completed
COMMIT
post group envelope
save public_message_id
```

There is a distributed boundary between PostgreSQL and Telegram.

If Telegram envelope posting fails after DB commit, mark the whisper as `delivery_failed` or retry.

For V1, adding this state is useful:

```text
pending_publish
active
opened
expired
revoked
publish_failed
```

If you want a minimal schema, log and retry publication from a small worker.

---

# 40. Suggested status model for a slightly stronger V1

Instead of the minimal status check, use:

```text
pending_publish
active
opened
expired
revoked
publish_failed
```

Flow:

```text
create DB row as pending_publish
        ↓
send public envelope
        ↓
store public_message_id + set active
```

Retry `pending_publish` rows periodically.

This is worth implementing if the bot will be used by anyone besides you.

---

# 41. User commands

Private:

```text
/start
/help
/cancel
/privacy
```

Group:

```text
/whisper
```

Optional later:

```text
/whisper_once
/whisper_1h
/whisper_24h
```

Better long-term UX is one command plus buttons rather than many commands.

---

# 42. Privacy command

`/privacy` should explain plainly:

```text
🔐 Privacy

• The secret is shown only to the selected recipient through Telegram's
  ephemeral-message feature.
• "Protected content" reduces forwarding/saving but cannot prevent screenshots
  or recording from another device.
• Expired metadata is cleaned according to the service retention policy.
```

This is a product feature, not merely legal boilerplate.

---

# 43. Abuse considerations

If this becomes a public bot, add:

- per-user quotas;
- group opt-out;
- `/report` flow;
- ban list;
- maximum TTL;
- no public file browsing;
- no admin endpoint that casually previews secrets;
- short metadata retention;
- explicit Terms/Privacy page.

The bot should never let a callback token alone reveal media through your own HTTP endpoint.

---

# 44. Makefile

```makefile
.PHONY: up down build logs test fmt vet migrate migrate-down psql

up:
	docker compose up -d --build

down:
	docker compose down

build:
	go build ./...

logs:
	docker compose logs -f bot

test:
	go test ./...

fmt:
	gofmt -w $$(find . -name '*.go' -type f)

vet:
	go vet ./...

migrate:
	go run github.com/pressly/goose/v3/cmd/goose@latest \
		-dir migrations postgres "$$DATABASE_URL" up

migrate-down:
	go run github.com/pressly/goose/v3/cmd/goose@latest \
		-dir migrations postgres "$$DATABASE_URL" down

psql:
	docker compose exec postgres \
		psql -U "$${POSTGRES_USER}" -d "$${POSTGRES_DB}"
```

If migrations are embedded and run automatically, replace the migration targets accordingly.

---

# 45. Local development sequence

## Step 1 — create Telegram bot

Use `@BotFather`:

```text
/newbot
```

Save:

```text
bot token
bot username
```

Never commit the token.

## Step 2 — configure

```bash
cp .env.example .env
```

Fill:

```dotenv
TELEGRAM_BOT_TOKEN=...
TELEGRAM_BOT_USERNAME=...
POSTGRES_PASSWORD=...
DATABASE_URL=...
```

For initial development:

```dotenv
TELEGRAM_UPDATE_MODE=polling
```

## Step 3 — start infrastructure

```bash
docker compose up -d postgres
```

## Step 4 — migrations

```bash
export $(grep -v '^#' .env | xargs)
make migrate
```

Or have the bot migrate at boot.

## Step 5 — run bot

Host:

```bash
go run ./cmd/bot
```

or fully containerized:

```bash
docker compose up -d --build
docker compose logs -f bot
```

## Step 6 — Telegram test

1. Start bot privately as Alice.
2. Add bot to a private test group.
3. Add Bob.
4. Bob posts any message.
5. Alice replies `/whisper`.
6. Alice receives bot DM.
7. Alice sends voice/photo/video.
8. Group receives envelope.
9. Eve presses Open -> denied.
10. Bob presses Open -> ephemeral secret appears to Bob.

---

# 46. Test plan

## Unit tests

### Recipient authorization

```text
correct recipient -> allowed
wrong recipient   -> denied
bot user          -> denied
```

### Expiry

```text
expires_at > now -> active
expires_at <= now -> denied
```

### One-time

```text
first open  -> claimed
second open -> denied
```

### Token parser

```text
valid w:<token> -> accepted
wrong prefix    -> rejected
empty token     -> rejected
oversized token -> rejected
```

### Media extraction

```text
voice
photo
video
audio
document
unsupported sticker
```

### Drafts

```text
create
cancel
expire
complete
sender mismatch
```

## Repository integration tests

Use a temporary PostgreSQL container.

Test:

- unique token hashes;
- one-time atomic update under concurrency;
- expiry queries;
- foreign keys;
- cleanup;
- transaction rollback.

## Telegram manual integration tests

Test at least:

- Android;
- desktop;
- recipient offline then online;
- group;
- supergroup;
- forum topic if you plan to support it;
- private group;
- recipient presses button twice;
- two people press simultaneously.

Telegram documents that ephemeral delivery is not guaranteed, especially while offline, so design failure messages and retries accordingly.

---

# 47. Observability

Counters:

```text
telegram_updates_total
whisper_drafts_created_total
whispers_created_total
whisper_open_attempts_total{result=allowed|denied|expired}
telegram_api_requests_total{method,status}
cleanup_rows_total{type}
```

Histograms:

```text
telegram_api_request_duration_seconds
update_handler_duration_seconds
```

Gauges:

```text
active_drafts
active_whispers
db_pool_connections
```

Do not label metrics by Telegram user ID, username, group ID, or whisper ID; that creates cardinality and privacy problems.

---

# 48. PostgreSQL pool

With `pgxpool`:

```go
poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
if err != nil {
    return err
}

poolConfig.MaxConns = 10
poolConfig.MinConns = 1
poolConfig.MaxConnLifetime = time.Hour
poolConfig.MaxConnIdleTime = 15 * time.Minute

pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
```

A tiny bot does not need a huge pool.

---

# 49. Graceful shutdown

Handle:

```text
SIGINT
SIGTERM
```

Sequence:

```text
stop accepting webhook requests / stop polling
cancel update workers
finish in-flight callbacks quickly
stop cleanup worker
shutdown HTTP server
close PostgreSQL pool
exit
```

Keep the shutdown window short because callback-based ephemeral sends are time-sensitive.

---

# 50. Production deployment options before Cloudflare migration

## Cheapest simple option

One small VPS:

```text
Docker
├── secretmediabot
└── PostgreSQL
```

For webhook mode:

```text
Internet
   │
HTTPS reverse proxy
   │
Go bot :8080
   │
PostgreSQL
```

Or:

```text
Telegram
   │
Cloudflare Tunnel
   │
Go bot on your machine/VPS
```

If using long polling, the bot needs outbound Internet only; Telegram does not need to reach your server.

---

# 51. Cloudflare free-tier path

Cloudflare is useful later, but V1 should not be distorted around it.

Current official documentation at the time this spec was written states:

- R2 Standard free tier: **10 GB-month storage/month**
- R2: **1 million Class A operations/month**
- R2: **10 million Class B operations/month**
- R2 Internet egress: **free**
- Workers Free: **100,000 Worker requests/day**
- static-asset requests through Workers are **free and unlimited**
- R2 supports object lifecycle rules for automatic expiry

Always re-check pricing before relying on it in production:

- https://developers.cloudflare.com/r2/pricing/
- https://developers.cloudflare.com/r2/buckets/object-lifecycles/
- https://developers.cloudflare.com/workers/platform/limits/
- https://developers.cloudflare.com/workers/static-assets/

---

# 52. Cloudflare migration stage A — keep Go + PostgreSQL

This is the migration I recommend first.

```text
Telegram
   │
   ▼
Cloudflare DNS / Tunnel
   │
   ▼
Go bot
   │
   ▼
PostgreSQL
```

Benefits:

- keep the Go application;
- keep PostgreSQL;
- avoid opening inbound ports if using Tunnel;
- easy custom hostname;
- Cloudflare handles the public edge.

Nothing about your persistence model needs to change.

---

# 53. Cloudflare migration stage B — static Mini App

If you later add a web UI or E2EE Mini App:

```text
Telegram Mini App
       │
       ▼
Cloudflare static assets
       │
       ▼
Go API
```

A small TypeScript/React or plain TypeScript app can be served as static assets.

This is where Cloudflare becomes especially useful because the client application can be globally served while the Go API remains unchanged.

---

# 54. Cloudflare migration stage C — R2

Do **not** move native V1 Telegram `file_id` media into R2 merely because R2 is free.

Use R2 when you need to store your own blobs, especially:

- E2EE ciphertext;
- attachments not retained by Telegram;
- generated thumbnails that are safe to store;
- encrypted exports.

Recommended future E2EE path:

```text
Sender Mini App
     │
     │ encrypt locally
     ▼
ciphertext
     │
     ▼
R2
     │
     ▼
Recipient Mini App
     │
     │ decrypt locally
     ▼
plaintext
```

The Go service stores only metadata and object keys.

R2 should use a **private bucket**. Issue short-lived presigned URLs rather than exposing a public bucket.

Add lifecycle cleanup as defense in depth.

---

# 55. Cloudflare migration stage D — should PostgreSQL become D1?

Not automatically.

D1 can be attractive for a Worker-first architecture, but if the core app is a normal Go process, PostgreSQL remains the natural database.

Keep:

```text
Go + PostgreSQL
```

unless you deliberately decide to rewrite/restructure the backend around Cloudflare Workers.

Do not add an HTTP database abstraction simply to force a small Postgres workload into D1.

---

# 56. Should the whole Go bot move into Cloudflare Workers?

Not for this V1.

Cloudflare Workers are excellent for Worker-native JavaScript/TypeScript applications. A conventional Go service is easier to operate as a normal binary/container.

Possible future architectures:

```text
A. Keep Go:
Telegram -> Go -> PostgreSQL
                 -> R2 later
```

```text
B. Worker rewrite:
Telegram -> Worker -> D1
                  -> R2
```

Architecture B can be cheap and elegant, but it changes the nature of the project and removes much of the reason to choose Go.

---

# 57. Optional Cloudflare Tunnel

Cloudflare Tunnel can be useful if you run the Go bot on:

- a home server;
- spare laptop;
- private VPS network;
- machine behind NAT.

Webhook path:

```text
Telegram
   │ HTTPS
   ▼
bot.example.com
   │
Cloudflare
   │ Tunnel
   ▼
localhost:8080
```

For long-polling mode, you do not need any ingress or Tunnel.

---

# 58. Future true-E2EE architecture

This is **not V1**, but keep the boundary clean now so migration is possible.

```text
                 ┌──────────────────────┐
                 │ Telegram group       │
                 │ public envelope      │
                 └──────────┬───────────┘
                            │
                open Mini App / callback
                            │
          ┌─────────────────┴─────────────────┐
          │                                   │
          ▼                                   ▼
 Sender Mini App                       Recipient Mini App
 generate random key                    holds private key
 encrypt media                          decrypts locally
          │                                   ▲
          ▼                                   │
       ciphertext ─────────── R2 ─────────────┘
          │
          ▼
       Go API
 metadata only
```

To truthfully claim the server cannot decrypt, the server must **never receive the plaintext decryption key**.

This means key exchange is a real cryptographic feature, not merely:

```text
AES encrypt -> send AES key to same server
```

If the same server stores both ciphertext and decrypting key, the design is encrypted-at-rest, not zero knowledge.

---

# 59. E2EE migration compatibility choices to make now

V1 should already separate:

```text
MediaReference interface
```

from Telegram business logic.

Example:

```go
type MediaReference struct {
    Provider string // telegram | r2
    Type     MediaType
    Ref      string
}
```

V1:

```text
Provider=telegram
Ref=<file_id>
```

Future:

```text
Provider=r2
Ref=whispers/<uuid>.ciphertext
```

This prevents media storage assumptions from spreading through every handler.

Do the same for delivery:

```go
type SecretDelivery interface {
    Deliver(ctx context.Context, whisper Whisper, open OpenContext) error
}
```

V1 implementation:

```text
TelegramEphemeralDelivery
```

Future:

```text
MiniAppEncryptedDelivery
```

---

# 60. Recommended implementation milestones

## Milestone 0 — Telegram spike

Before building repositories:

- create bot;
- add to test group;
- hard-code Bob user ID;
- send a button;
- press callback as Bob;
- call `sendVoice` or `sendPhoto` with:
  - `receiver_user_id`;
  - `callback_query_id`;
  - known `file_id`;
- confirm ephemeral behavior on your Telegram clients.

**Do this first.**

It validates the newest/most unusual API feature before you build around it.

## Milestone 1 — skeleton

- config;
- logger;
- Postgres connection;
- migrations;
- health endpoint;
- graceful shutdown.

## Milestone 2 — Telegram updates

- long polling;
- `/start`;
- `/help`;
- user/chat upserts.

## Milestone 3 — draft creation

- reply `/whisper`;
- validate recipient;
- create draft;
- DM sender;
- `/cancel`.

## Milestone 4 — media

- voice;
- photo;
- video;
- audio;
- document;
- `file_id` extraction.

## Milestone 5 — envelope/open

- opaque token;
- callback button;
- recipient validation;
- ephemeral media delivery;
- wrong-user denial.

## Milestone 6 — lifecycle

- expiry;
- cleanup worker;
- one-time open;
- revoke/cancel;
- stale metadata cleanup.

## Milestone 7 — production hardening

- webhook;
- secret validation;
- rate limits;
- idempotency;
- retry/publish state;
- metrics;
- privacy text;
- structured logs.

## Milestone 8 — optional Cloudflare

- Tunnel/custom hostname;
- static site/Mini App;
- R2 only if own blob storage is needed.

---

# 61. Suggested first GitHub issues

```text
#1 Bootstrap Go service and configuration
#2 Add PostgreSQL Compose service
#3 Add DB migrations and repositories
#4 Implement Telegram Bot API client
#5 Add polling update loop
#6 Implement /start and /help
#7 Implement reply-based /whisper draft creation
#8 Implement private media ingestion
#9 Implement public envelope callback
#10 Implement Bot API 10.2 ephemeral media delivery
#11 Enforce recipient authorization
#12 Add whisper expiration cleanup
#13 Add one-time open
#14 Add rate limiting
#15 Add webhook mode
#16 Add integration tests
#17 Add privacy/security documentation
#18 Optional Cloudflare deployment documentation
```

---

# 62. Definition of Done for V1

V1 is done when all of the following work:

- [ ] Bot starts from `docker compose up -d`.
- [ ] PostgreSQL is the only required backing service.
- [ ] `.env` contains runtime secrets.
- [ ] `.env.example` contains no real secrets.
- [ ] Sender can `/start` the bot privately.
- [ ] Sender can reply `/whisper` to a real user in a group.
- [ ] Bot creates an expiring draft.
- [ ] Sender receives a private composer prompt.
- [ ] Sender can submit voice.
- [ ] Sender can submit photo.
- [ ] Sender can submit video.
- [ ] Sender can submit audio.
- [ ] Sender can submit document.
- [ ] No media bytes are persisted by your service.
- [ ] Telegram `file_id` is stored in PostgreSQL.
- [ ] Group receives a media-free envelope.
- [ ] Wrong user pressing Open receives only a denial.
- [ ] Intended recipient receives Telegram ephemeral media.
- [ ] `protect_content=true` is set on delivery.
- [ ] Expired secrets cannot be opened.
- [ ] One-time secrets cannot be successfully opened twice.
- [ ] Drafts expire automatically.
- [ ] Expired metadata is cleaned automatically.
- [ ] Raw Telegram updates are not logged in production.
- [ ] Bot token never appears in logs.
- [ ] Unit tests cover authorization/state logic.
- [ ] PostgreSQL integration tests cover one-time-open atomicity.
- [ ] README explicitly states that V1 is not E2EE.

---

# 63. Things I would *not* overengineer yet

Do not start with:

```text
Kubernetes
Helm
Argo CD
Kafka
NATS
Redis Cluster
Vault
service mesh
multiple microservices
CQRS
event sourcing
three databases
custom object storage
Mini App
E2EE
```

The V1 should be one Go process plus PostgreSQL.

A clean monolith is the correct architecture here.

---

# 64. Upgrade path after V1

Order I would use:

```text
V1.0
Go + PostgreSQL
Telegram file_id
native ephemeral group delivery

        ↓

V1.1
better expiry controls
anonymous sender mode
one-time mode
admin-enhanced clean UX
metrics + abuse controls

        ↓

V1.2
Cloudflare Tunnel / custom domain
webhook production deployment

        ↓

V2
Telegram Mini App
Cloudflare static hosting
R2 ciphertext storage
client-side encryption

        ↓

V2.x
real public-key E2EE design
device registration / key rotation
multi-device strategy
encrypted attachment streaming

        ↓

Scale only if needed
Redis
multiple replicas
queue
managed PostgreSQL
```

---

# 65. Optional Redis migration

When Redis is justified:

```text
Go bot
├── PostgreSQL   durable truth
└── Redis        disposable hot state
```

PostgreSQL remains authoritative.

Good Redis responsibilities:

```text
rate-limit counters
dedupe markers
very short draft cache
distributed mutex/lease
temporary callback state
job queue
```

Bad Redis responsibility:

```text
only copy of a whisper
only copy of recipient authorization
only copy of user registration
```

If Redis disappears, the bot should lose performance/convenience, not correctness or durable data.

---

# 66. Backup policy

For a privacy-oriented bot, "back up everything forever" conflicts with the product.

Recommended:

- PostgreSQL backup retention kept short.
- Media is not stored in PostgreSQL.
- Open-event retention short.
- Expired whisper rows deleted after a defined period.
- State the metadata retention policy.
- If you later use R2, make bucket lifecycle expiration part of the design.

If you promise 24-hour deletion but keep 90-day database backups containing whisper metadata, document that distinction accurately.

---

# 67. Data minimization

You probably do **not** need:

- phone number;
- email;
- full Telegram profile history;
- contact graph;
- IP addresses in long-term logs;
- message text from groups;
- original group messages;
- plaintext secret captions.

Store only what enables the feature.

Recommended user table fields are deliberately small.

---

# 68. Error messages

Examples:

### No reply target

```text
Reply to the person you want to whisper to, then send /whisper.
```

### Target is bot

```text
You can only whisper to a human member.
```

### Sender hasn't started bot

```text
Open the bot once so I can receive your secret privately.
[ Open bot ]
```

### Draft expired

```text
⌛ That draft expired. Create a new /whisper from the group.
```

### Unsupported media

```text
Send a voice note, photo, video, audio file, or document.
```

### Wrong recipient

```text
🔒 This whisper isn't for you.
```

### Whisper expired

```text
⌛ This whisper has expired.
```

### Already opened

```text
🔓 This one-time whisper was already opened.
```

### Telegram ephemeral delivery failure

```text
I couldn't display the secret ephemerally. Try Open again while Telegram is active.
```

Do not fall back to a normal public group media message.

---

# 69. Important failure rule

If ephemeral delivery fails:

**Never** do this:

```text
ephemeral failed -> send normal media to group
```

That would turn a privacy failure into content disclosure.

Fail closed:

```text
ephemeral failed -> show error / allow retry
```

---

# 70. Repository API sketch

```go
type UserRepository interface {
    Upsert(ctx context.Context, user TelegramUser, startedPrivate bool) error
    MarkPrivateStarted(ctx context.Context, userID int64) error
}

type DraftRepository interface {
    Create(ctx context.Context, draft Draft) (Draft, error)
    ActiveForSender(ctx context.Context, senderID int64) ([]Draft, error)
    Complete(ctx context.Context, id uuid.UUID) error
    Cancel(ctx context.Context, id uuid.UUID, senderID int64) error
    ExpireDue(ctx context.Context, now time.Time) (int64, error)
}

type WhisperRepository interface {
    Create(ctx context.Context, whisper Whisper, tokenHash []byte) (Whisper, error)
    FindByTokenHash(ctx context.Context, tokenHash []byte) (Whisper, error)
    ClaimOneTimeOpen(ctx context.Context, id uuid.UUID, recipientID int64, now time.Time) (bool, error)
    RecordOpenAttempt(ctx context.Context, id uuid.UUID, userID int64, allowed bool) error
    ExpireDue(ctx context.Context, now time.Time) (int64, error)
    SetPublicMessageID(ctx context.Context, id uuid.UUID, messageID int64) error
}
```

---

# 71. Service API sketch

```go
type WhisperService struct {
    users    UserRepository
    drafts   DraftRepository
    whispers WhisperRepository
    telegram *telegram.Client
    clock    Clock
    tokens   TokenGenerator
}

func (s *WhisperService) CreateDraft(...)
func (s *WhisperService) AcceptMedia(...)
func (s *WhisperService) Open(...)
func (s *WhisperService) CancelDraft(...)
func (s *WhisperService) Cleanup(...)
```

Keep Telegram update parsing in the Telegram package and business rules in the service package.

---

# 72. Context/timeouts

Each Telegram request should have a timeout:

```go
ctx, cancel := context.WithTimeout(parent, 8*time.Second)
defer cancel()
```

Database operations:

```go
ctx, cancel := context.WithTimeout(parent, 3*time.Second)
defer cancel()
```

Do not let a broken Telegram request pin a handler indefinitely.

---

# 73. Retry policy

Safe-ish to retry:

- `getMe`
- polling requests
- database reads
- publication jobs with idempotency protection

Be careful retrying:

- public envelope creation;
- one-time open delivery;
- any operation that can duplicate user-visible messages.

Use returned Telegram message IDs and database state to dedupe.

---

# 74. Media size policy

Telegram limits vary by media type/API path and may change.

V1 does not download the file, so your own memory/disk is not the bottleneck, but you should still:

- let Telegram enforce its API limits;
- optionally reject unusually large `file_size` values based on configuration;
- avoid hard-coding business logic to today's maximum Telegram sizes unless needed.

Possible config later:

```dotenv
MAX_MEDIA_BYTES=52428800
```

---

# 75. Forum topics

If the source group is a Telegram forum/supergroup topic, persist:

```text
message_thread_id
```

When posting the public envelope, use the same topic.

When sending ephemeral media in response to the callback, preserve the relevant chat/topic context when Telegram's method semantics require it.

This makes the bot feel native in topic-based groups.

---

# 76. Group allowlist

For private testing:

```dotenv
ALLOWED_CHAT_IDS=-1001234567890,-1009988776655
```

When set, reject any other group.

This is a useful launch control before making the bot public.

---

# 77. Recommended launch modes

## Dev

```text
polling
local Docker Compose
test group allowlist
debug logs
```

## Private beta

```text
polling or webhook
small VPS/home server
PostgreSQL
allowlisted groups
INFO logs
rate limiting
```

## Public

```text
webhook
public HTTPS
PostgreSQL backup policy
abuse controls
privacy page
metrics
idempotency
publish retries
short retention
```

---

# 78. Cloudflare R2 lifecycle later

If you add R2 ciphertext storage, use lifecycle rules as defense in depth.

Example concept:

```text
encrypted/24h/*
encrypted/7d/*
```

The application should still explicitly delete expired objects. Lifecycle expiration handles failures/orphans.

Do not depend on lifecycle cleanup for "delete exactly at 12:03:04." Cloud object lifecycle systems are retention/cleanup mechanisms, not real-time self-destruct timers.

---

# 79. Cost profile

## V1

Application resources are tiny:

```text
Go process
PostgreSQL
no media disk
no object storage
```

The biggest practical cost is simply wherever PostgreSQL and the Go process run.

If deployed on hardware you already own, software infrastructure cost can be near zero.

## Later Cloudflare

R2 is attractive for ciphertext because its current Standard free tier includes 10 GB-month and free Internet egress, plus sizable free operation allowances.

Workers/static hosting can cheaply host a Mini App/front end.

Do not optimize around free-tier limits before the product needs those components.

---

# 80. Final recommendation

Build **exactly this first**:

```text
Go monolith
    │
    ├── Telegram Bot API 10.2+
    │
    └── PostgreSQL

Docker Compose
    ├── bot
    └── postgres
```

Use:

```text
long polling in development
reply-to-user recipient selection
private bot DM for media composition
Telegram file_id as media reference
public group envelope
callback recipient validation
ephemeral Telegram media on open
protect_content=true
PostgreSQL TTL/state
```

Do **not** add Redis yet.

Do **not** add Cloudflare R2 yet.

Do **not** call V1 E2EE.

After the native ephemeral flow is proven and useful, Cloudflare becomes a strong V2 companion for:

```text
static Telegram Mini App
R2 encrypted blobs
Tunnel/custom domain
possibly Worker edge functions
```

The most important first technical task is a **small Bot API 10.2 spike** proving that a callback-triggered `sendVoice`/`sendPhoto` with `receiver_user_id` and `callback_query_id` behaves exactly as expected on the Telegram clients you care about.

---

# 81. Official reference links

Telegram:

- https://core.telegram.org/bots/api
- https://core.telegram.org/bots/api-changelog

Cloudflare:

- https://developers.cloudflare.com/r2/pricing/
- https://developers.cloudflare.com/r2/buckets/object-lifecycles/
- https://developers.cloudflare.com/r2/get-started/s3/
- https://developers.cloudflare.com/workers/platform/limits/
- https://developers.cloudflare.com/workers/static-assets/
- https://developers.cloudflare.com/workers/static-assets/billing-and-limitations/

Go/PostgreSQL libraries:

- https://github.com/jackc/pgx
- https://github.com/pressly/goose

---

## Short architecture summary for an AI coding agent

```text
Implement a Go Telegram bot using Telegram Bot API 10.2+.

Use PostgreSQL as the only required backing service and Docker Compose for
development/deployment. Do not use Redis by default.

A sender starts the bot privately once. In a group, the sender replies
/whisper to another user's message. Persist an expiring draft containing
sender ID, recipient ID, source chat ID and optional topic/thread ID. Prompt
the sender in the bot's private chat for a photo, voice, video, audio or
document.

When media arrives, store only its Telegram file_id/file_unique_id and
metadata in PostgreSQL. Never persist the media bytes in V1. Create an
unguessable callback token, store its hash, and post a media-free envelope
in the source group with an Open secret inline button.

When the button is clicked, load by token hash and authorize strictly with
callback_query.from.id == stored recipient_id. Wrong users receive only
answerCallbackQuery denial. For the correct recipient, call the matching
sendPhoto/sendVoice/sendVideo/sendAudio/sendDocument Bot API method with the
source chat_id, receiver_user_id, callback_query_id, stored file_id and
protect_content=true so Telegram displays an ephemeral group message only to
that recipient and the bot.

Implement draft TTL, whisper TTL, cleanup, optional one-time atomic opening,
structured logs without content/file IDs, graceful shutdown, long polling for
development, optional webhook mode for production, health/readiness endpoints,
migrations and tests.

V1 is recipient-private in Telegram but not end-to-end encrypted. A future V2 may add a Telegram Mini App, client-side cryptography and Cloudflare R2 ciphertext storage.
```
