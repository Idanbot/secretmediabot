# Secret Media Bot

Secret Media Bot is a runnable Go/PostgreSQL Telegram bot for sending secret text or one secret media item to one person in a shared group. A media secret may include an optional caption. The intended recipient opens a public envelope through a one-time button and receives the secret through Telegram's ephemeral-message API.

The privacy claim is deliberately narrow: the Telegram UI limits delivery to the selected recipient.

See [Architecture](docs/architecture.md) for the data flow, state machines, storage model, and threat model. The original build specification remains at [docs/telegram-media-whisper-v1.md](docs/telegram-media-whisper-v1.md).

## Repository status

The V1 application is wired and runnable through `cmd/bot`, `make run`, the compiled binary, or the Compose stack. Implemented paths include:

- polling and authenticated-webhook update transport with durable update leasing;
- Telegram Guest Mode mentions and Inline Mode locked-envelope requests for groups where the bot is not a member;
- group command handling, observed same-chat recipient lookup, one private draft per sender, encrypted text/media ingestion, and retryable envelope publication;
- atomic one-time recipient reservation, ephemeral delivery, idempotent completion, and durable best-effort deletion jobs;
- retention changes, hard deletion, and audit writes;
- embedded Goose migrations, GORM/explicit-SQL repositories, cleanup/publication/deletion workers, health probes, and graceful shutdown;
- unit tests across the application packages and an opt-in PostgreSQL integration suite covering constraints, concurrency, rollback, one-time opens, durable deletion jobs, and retention cascades.

This has **not** been tested end to end with a live Telegram bot. The HTTP/TestServer and PostgreSQL tests verify local behavior, but deployment-specific Bot API behavior—especially ephemeral-message methods—still needs real group/supergroup testing.

## Implemented V1 behavior

1. A sender replies to a group message with `/whisper`, or invokes `/whisper @username` or `/whisper 123456789`. Resolution is restricted to users the bot has observed in that same group; usernames are lookup hints, while Telegram numeric IDs remain the identity.
2. The bot collects either text or one photo, voice note, video, audio file, or document in private chat. Media is limited to 20 MiB and may have one caption.
3. Text, captions, and media are encrypted with AES-256-GCM before being stored in PostgreSQL. The 32-byte key is supplied to the process and is never stored in the database.
4. The group receives an envelope without secret content. The selected recipient presses its opaque callback button.
5. The service checks the callback actor's numeric Telegram ID and atomically reserves the one-time open. Availability defaults to 24 hours.
6. Telegram receives an ephemeral text/media response with protected content enabled. On successful completion, PostgreSQL records a durable job to request deletion after 30 seconds by default; retries survive process restarts, but deletion remains best effort.
7. Live whisper rows and encrypted payload rows are hard-deleted 30 days after creation by the cleanup worker. Audit records survive that deletion.

Telegram does not provide a dependable “the user read this secret” receipt. A callback press and successful send only establish that delivery was requested and accepted by Telegram. They do not prove that the recipient viewed it.

## Bot commands

User commands:

| Command | Where | Behavior |
| --- | --- | --- |
| `/whisper` | Group/supergroup | Reply to a person's message to start a whisper. Configured as an ephemeral command where Telegram supports it. |
| `/whisper @username` | Group/supergroup | Targets a username observed by the bot in that same chat. |
| `/whisper 123456789` | Group/supergroup | Targets a numeric user ID observed in that same chat. |
| `/start` | Private chat | Shows private composer guidance. A generated `/start compose_<token>` deep link resumes the sender's matching draft. |
| `/cancel` | Private chat | Cancels the sender's one active draft. |
| `/privacy` | Group/supergroup or private chat | Explains encryption, retention, and Telegram/deletion limitations. |

Only one draft may be in `awaiting_media` or `ingesting_media` for a sender. After `/whisper`, the bot either opens the existing private chat directly or provides a deep link. The sender then sends text, or exactly one photo, voice note, video, audio file, or document with an optional caption. `/help` is also available in private and group chats.

## Guest and inline requests

Guest Mode and Inline Mode must be enabled for the bot in BotFather. In a group where the bot is not a member, send `@bot_username @target` or `@bot_username 123456789`; do not include the secret in that group message. Telegram sends a guest update, and the bot posts one opaque locked envelope. The sender follows its private link to add text or media. The target follows the private link to claim the request by numeric Telegram ID or username, receives the content in private chat, and the normal private message is deleted 30 seconds after Telegram accepts it.

Inline Mode supports the same locked envelope from any chat. Inline results must never contain the secret or secret media: selecting a media result would make it visible to the group. Numeric IDs are authoritative; usernames are accepted as lookup hints and are verified when the target opens privately. Guest Mode does not provide group history or a participant directory, and it permits only the response to the triggering mention.

## Privacy summary

- Ordinary user-facing access is limited to the sender and selected recipient. The envelope itself contains no secret.
- PostgreSQL stores ciphertext, nonces, key IDs, integrity hashes, Telegram identifiers, participant IDs, state, and timestamps. The AES key stays outside PostgreSQL.
- The bot necessarily handles plaintext while collecting or delivering it. Telegram also receives the original upload and the delivered copy.
- Deleting the PostgreSQL row cannot remove the sender's original Telegram message, screenshots, client caches, or data retained by Telegram. `deleteEphemeralMessage` is a best-effort cleanup call, not a deletion guarantee.
- `protect_content=true` discourages forwarding/saving in supporting clients; it cannot prevent screenshots or recording with another device.
- PostgreSQL backups, WAL, replicas, and infrastructure snapshots may outlive deletion from the live database. This project does not claim immediate cryptographic erasure.

## Prerequisites

- Docker Engine with Docker Compose v2.
- Go 1.26.6 for host-side development, matching `go.mod` and the Docker build image.
- A Telegram bot token and username from `@BotFather`.
- A positive Telegram user ID for at least one privileged account.
- For webhook mode, a public HTTPS URL terminating at `/telegram/webhook`.

The V1 Telegram client targets Bot API 10.2+ ephemeral-message fields.

## Configuration

Create the local file and generate secrets:

```sh
cp .env.example .env
openssl rand -hex 24
openssl rand -base64 32
openssl rand -hex 32
```

Use the first value as the PostgreSQL password, the base64 value as `MEDIA_ENCRYPTION_KEY`, and the final value as `TELEGRAM_WEBHOOK_SECRET` when using webhooks. Edit `.env`; do not commit it.

Required application values are:

| Variable | Meaning |
| --- | --- |
| `TELEGRAM_BOT_TOKEN` | BotFather token; secret. |
| `TELEGRAM_BOT_USERNAME` | Bot username without `@` preferred. |
| `OWNER_TELEGRAM_IDS` | Comma-separated positive Telegram user IDs authorized for privileged operator commands; at least one is required. |
| `DATABASE_URL` | PostgreSQL DSN. The Compose value uses host `postgres`. |
| `MEDIA_ENCRYPTION_KEY_ID` | Identifier stored beside new ciphertext, default `v1`. |
| `MEDIA_ENCRYPTION_KEY` | Base64-encoded 32-byte AES key; secret and outside the database. |
| `POSTGRES_DB`, `POSTGRES_USER`, `POSTGRES_PASSWORD` | Required by the Compose PostgreSQL container. |

The checked-in example defines the complete configuration surface:

| Area | Variables and example defaults |
| --- | --- |
| Application | `APP_ENV=development`, `LOG_LEVEL=debug` |
| HTTP | `HTTP_ADDR=:8080`, `HTTP_READ_HEADER_TIMEOUT=5s`, `HTTP_READ_TIMEOUT=15s`, `HTTP_WRITE_TIMEOUT=20s`, `HTTP_IDLE_TIMEOUT=60s`, `HTTP_SHUTDOWN_TIMEOUT=15s` |
| Telegram | `TELEGRAM_UPDATE_MODE=polling`, `TELEGRAM_API_BASE_URL=https://api.telegram.org`, `TELEGRAM_POLL_TIMEOUT=30s`, `TELEGRAM_REQUEST_TIMEOUT=15s` |
| Webhook | `TELEGRAM_WEBHOOK_PUBLIC_URL`, `TELEGRAM_WEBHOOK_SECRET` (required only for webhook mode), `TELEGRAM_WEBHOOK_MAX_CONNECTIONS=4` |
| Database pool | `DB_MAX_CONNS=10`, `DB_MIN_CONNS=2`, `DB_MAX_CONN_LIFETIME=1h`, `DB_MAX_CONN_IDLE_TIME=30m`, `DB_CONNECT_TIMEOUT=10s` |
| Whisper | `DRAFT_TTL=10m`, `DEFAULT_WHISPER_TTL=24h`, `MAX_WHISPER_TTL=168h`, `DEFAULT_ONE_TIME=true`, `PROTECT_CONTENT=true`, `PUBLISH_LEASE_TIMEOUT=2m`, `PUBLISH_INTERVAL=2s` |
| Delivery deletion | `EPHEMERAL_DELETE_AFTER=30s`, `EPHEMERAL_DELETE_INTERVAL=2s` |
| Limits | `MAX_ACTIVE_DRAFTS_PER_USER=1` (V1 requires exactly one), `MAX_WHISPERS_PER_USER_PER_HOUR=30`, `ALLOWED_CHAT_IDS=` (empty allows all groups) |
| Storage | `MEDIA_STORAGE=postgres`, `MAX_MEDIA_BYTES=20971520`, `MEDIA_RETENTION=720h`, `MEDIA_DOWNLOAD_TIMEOUT=2m` |
| Cleanup | `CLEANUP_INTERVAL=5m`, `CLEANUP_BATCH_SIZE=500`, `PROCESSED_UPDATE_RETENTION=168h` |

Compose-only substitutions are `ENV_FILE`, `VERSION`, `COMMIT`, `HTTP_BIND_HOST` (default `127.0.0.1`), and `HTTP_PORT` (default `8080`). `ENV_FILE` selects the bot container's env file; Compose still resolves PostgreSQL `${...}` substitutions from the shell or project `.env`. PostgreSQL is not published to the host.

## Bootstrap and Compose

Validate the Compose interpolation, then build and start the complete stack:

```sh
docker compose config --quiet
make compose-up
docker compose ps
docker compose exec postgres sh -c 'pg_isready -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
```

Follow logs and stop the stack with:

```sh
make compose-logs
make compose-down
```

Equivalent direct commands are:

```sh
docker compose up --build -d
docker compose logs -f bot postgres
docker compose down
```

`docker compose down` keeps the named PostgreSQL volume. Adding `--volumes` destroys local database data and should only be done intentionally.

The bot container runs as a non-root distroless user with a read-only filesystem, dropped Linux capabilities, and a small `/tmp` tmpfs. Its HTTP port binds to loopback by default. Compose intentionally does not set Docker's `no-new-privileges` option because some valid Docker runtimes reject container process execution when it is enabled; deployments with a compatible runtime may add it in a local or production override.

## Database migrations

Goose migrations are versioned in `migrations/` and embedded in the Go package. GORM AutoMigrate is intentionally not used.

For a PostgreSQL DSN reachable from the host:

```sh
set -a
. ./.env
set +a
make migrate-status
make migrate-up
```

Rollback is explicit and destructive:

```sh
make migrate-down
```

The default Compose DSN uses the internal hostname `postgres`, while the database has no host port. Therefore host-side `make migrate-*` targets need a different reachable DSN. Bot startup runs embedded migrations inside the Compose network before processing updates or starting workers.

## Polling mode

Polling is the development default:

```dotenv
TELEGRAM_UPDATE_MODE=polling
```

At startup the bot verifies `getMe` against `TELEGRAM_BOT_USERNAME`, configures `/whisper` as an ephemeral command, deletes any existing webhook without dropping pending updates, and starts long polling. Updates pass through a durable PostgreSQL lease before dispatch; the poll offset advances only after successful processing. Polling requests `message`, `callback_query`, `guest_message`, and `inline_query` updates.

## Webhook mode

Configure an exact public endpoint and a 32–256 character secret containing only letters, numbers, `_`, or `-`:

```dotenv
TELEGRAM_UPDATE_MODE=webhook
TELEGRAM_WEBHOOK_PUBLIC_URL=https://bot.example.com/telegram/webhook
TELEGRAM_WEBHOOK_SECRET=replace_with_64_hex_characters
TELEGRAM_WEBHOOK_MAX_CONNECTIONS=4
HTTP_WRITE_TIMEOUT=3m
```

Webhook processing is synchronous, so `HTTP_WRITE_TIMEOUT` must be at least `MEDIA_DOWNLOAD_TIMEOUT + 3*TELEGRAM_REQUEST_TIMEOUT` (2m45s with the example defaults). This covers media metadata, download, envelope publication, and acknowledgement; the example uses 3m. `TELEGRAM_WEBHOOK_MAX_CONNECTIONS` is validated from 1 through Telegram's maximum of 100 and defaults to 4 to bound concurrent database and media work. At startup the bot registers the configured webhook for `message` and `callback_query` updates, including the secret token and connection limit, and does not start the poller. The handler accepts `POST /telegram/webhook` with an `application/json` content type, compares `X-Telegram-Bot-Api-Secret-Token` in constant time, limits bodies to 1 MiB, rejects trailing JSON, and returns an error so Telegram can retry failed processing. A reverse proxy or tunnel must provide public TLS.

Health routes implemented by the HTTP package are:

```text
GET /healthz
GET /readyz
POST /telegram/webhook   # webhook mode only
```

Readiness checks PostgreSQL with a short timeout and does not call Telegram.

In either mode, startup opens PostgreSQL, applies embedded migrations, initializes the AES keyring, verifies the bot identity, configures Telegram commands and transport, and starts the HTTP server plus cleanup, publication, ephemeral-deletion, and guest-private-deletion workers. `SIGINT`/`SIGTERM` cancels all runners, shuts down HTTP within `HTTP_SHUTDOWN_TIMEOUT`, and waits for workers to exit.

## Tests and developer commands

Canonical repository commands are:

```sh
go mod download
make test
make test-race
make vet
make fmt-check
make check
```

All unit suites run without Telegram credentials. The PostgreSQL integration test is skipped unless `TEST_DATABASE_URL` is set:

```sh
go test ./...

TEST_DATABASE_URL='postgres://user:password@127.0.0.1:5432/database?sslmode=disable' \
  go test -race ./internal/repository -run '^TestPostgresRepositoryIntegration$' -count=1
```

The PostgreSQL suite applies the embedded migrations to the supplied test database and truncates application tables; use a disposable database. It exercises same-chat lookup, concurrent draft creation, encrypted finalization and rollback, concurrent one-time opening, durable deletion jobs, audit action selection, the 20 MiB database constraint, and retention cascades. There is no automated or manually claimed live-Telegram end-to-end result.

## Package layout

```text
cmd/bot                executable wiring, migrations, transport setup, workers, shutdown
internal/app           polling, durable update processing, publication/cleanup/delete workers
internal/bot           Telegram command, ingestion, publication, and callback handlers
internal/command       slash-command and same-group target parsing
internal/config        environment loading, validation, privileged-account/chat allowlists
internal/domain        users, chats, drafts, content references, states, events
internal/httpserver    health/readiness and authenticated webhook transport
internal/repository    GORM/SQL persistence, leases, encrypted rows, audit, cleanup
internal/service       draft, ingest, publish, recipient-open, and privileged-account use cases
internal/secretcrypto  AES-256-GCM keyring and row/purpose-bound AAD
internal/telegram      typed Bot API client, downloads, ephemeral sends, uploads
internal/token         opaque callback generation, strict parsing, SHA-256 hashes
migrations             authoritative Goose SQL schema and indexes
```

Routine CRUD uses GORM. Concurrency-sensitive operations—update claims, draft ingestion, envelope publication, one-time opening, and retention deletion—use explicit transactions and conditional SQL. See [docs/architecture.md](docs/architecture.md).

## Current caveats

- No live Telegram end-to-end test has been performed. Bot API 10.2+ ephemeral methods and fields must be verified against the intended bot, group types, clients, and deployment region.
- Guest and inline flows require the corresponding BotFather capabilities; guest responses are one-shot and Telegram does not provide a group membership directory or a read receipt.
- “One-time” is one accepted Telegram delivery, not exactly-once human viewing. Database-completion retries reduce ambiguity, but a prolonged database failure after Telegram accepts a send can still permit a later duplicate delivery.
- Envelope publication has the same cross-system boundary: a prolonged failure while recording an accepted Telegram send can produce a duplicate empty envelope on retry.
- Recipient media delivery reuses Telegram's `file_id`; the retained PostgreSQL copy is not used as an automatic recipient-delivery fallback.
- The deletion queue is durable and defaults to 30 seconds after Telegram accepts delivery, but Telegram may reject deletion, the message may already be gone, or the recipient may capture it first. Telegram does not provide a read receipt. If delivery succeeds but its database completion never commits, no durable deletion job exists.
- V1 supports one active encryption key from configuration. Key rotation tooling, external secret-manager integration, backup/WAL erasure procedures, and automated restoration drills are not included.
