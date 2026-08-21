# Secret Media Bot

[![CI](https://github.com/Idanbot/secretmediabot/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/Idanbot/secretmediabot/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.26.6-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Image](https://img.shields.io/badge/Image-GHCR-black)](https://github.com/Idanbot/secretmediabot/pkgs/container/secretmediabot)

Secret Media Bot is a Go/PostgreSQL Telegram bot that delivers a private text secret — or one media item with an optional caption — to **exactly one person** in a shared group. The group only ever sees a public, content-free envelope with a one-time button; the secret itself arrives as a Telegram ephemeral message with `protect_content` enabled, then the bot requests its deletion.

The privacy claim is deliberately narrow: Telegram UI mechanics limit delivery to the selected recipient. See [Privacy and threat model](#privacy-and-threat-model) and [docs/architecture.md](docs/architecture.md).

> **Status:** V1 is wired, tested (unit + opt-in PostgreSQL integration suite), and published to GHCR by CI. It has **not** been validated end to end against a live Telegram bot — see [Current caveats](#current-caveats).

## Table of contents

- [How it works](#how-it-works)
- [Repository layout](#repository-layout)
- [Getting started](#getting-started)
- [Configuration](#configuration)
- [Usage](#usage)
- [Development](#development)
- [CI/CD and container images](#cicd-and-container-images)
- [Privacy and threat model](#privacy-and-threat-model)
- [Current caveats](#current-caveats)
- [Documentation](#documentation)

## How it works

```text
Sender ----------> Telegram ----------> Bot ----------> PostgreSQL
  |  private text /            encrypted |                  |
  |  one media (<= 20 MiB)     CRUD +    |                  |
  |  + optional caption        leases    |     ciphertext,  |
  |                                     |     metadata,     |
Group <---- content-free envelope ------+     audit        ^
  |        + one-time button                       AES-256-GCM key
Recipient <---- ephemeral delivery ------------- (process only;
  |        protect_content + delete request        never in DB)
```

1. A sender replies to a group message with `/whisper`, or targets `@username` / a numeric ID. Recipient resolution is restricted to users the bot has observed **in that same group**; numeric Telegram IDs are authoritative.
2. The bot collects either text or exactly one photo, voice note, video, audio file, or document (≤ 20 MiB, optional caption) in private chat.
3. Text, captions, and media are encrypted with AES-256-GCM (fresh nonce per record, row/purpose-bound AAD) before storage. The 32-byte key is supplied to the process and never stored in PostgreSQL.
4. The group receives an envelope **without any secret content**. The selected recipient presses its opaque callback button.
5. The service checks the callback actor's numeric ID and atomically reserves the one-time open. Availability defaults to 24 hours.
6. Telegram receives an ephemeral text/media response with `protect_content` enabled. On successful completion, PostgreSQL records a durable job to request deletion after 30 seconds by default; retries survive restarts, but deletion remains best effort.
7. Live whisper rows and encrypted payload rows are hard-deleted 30 days after creation by the cleanup worker. Audit records survive that deletion.

Telegram does not provide a dependable "the user read this secret" receipt. A callback press and successful send only establish that delivery was requested and accepted by Telegram.

## Repository layout

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

Routine CRUD uses GORM. Concurrency-sensitive operations — update claims, draft ingestion, envelope publication, one-time opening, and retention deletion — use explicit transactions and conditional SQL. See [docs/architecture.md](docs/architecture.md).

## Getting started

### Prerequisites

- Docker Engine with Compose v2.
- Go 1.26.6 for host-side development (matches `go.mod` and the Docker build image).
- A Telegram bot token and username from `@BotFather`.
- A positive Telegram user ID for at least one privileged account (`OWNER_TELEGRAM_IDS`).

The V1 Telegram client targets Bot API 10.2+ ephemeral-message fields.

### 1. Clone and configure

```sh
git clone https://github.com/Idanbot/secretmediabot.git
cd secretmediabot

cp .env.example .env
openssl rand -hex 24            # -> PostgreSQL password
openssl rand -base64 32         # -> MEDIA_ENCRYPTION_KEY
openssl rand -hex 32            # -> TELEGRAM_WEBHOOK_SECRET (webhook mode)
```

Fill in the generated values and set `TELEGRAM_BOT_TOKEN`, `TELEGRAM_BOT_USERNAME`, and `OWNER_TELEGRAM_IDS` in `.env`. Never commit `.env`; it is git-ignored.

### 2. Run the stack

```sh
docker compose config --quiet          # validate interpolation
docker compose up --build -d
docker compose ps
```

Polling is the development default (`TELEGRAM_UPDATE_MODE=polling`), so no public URL is needed. Verify health:

```sh
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/readyz
docker compose exec postgres sh -c 'pg_isready -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
```

Stop with `docker compose down`. This keeps the named PostgreSQL volume; add `--volumes` only when you intentionally want to destroy local database data.

### Polling vs. webhook

| Mode | When | Notes |
| --- | --- | --- |
| `polling` | Development, outbound-only deployments | Default. Deletes any existing webhook on start, leases updates durably, advances the offset only after processing. |
| `webhook` | Production behind stable public HTTPS | Registers the webhook on start, synchronous processing, secret-token auth, constant-time header comparison. |

Webhook example:

```dotenv
TELEGRAM_UPDATE_MODE=webhook
TELEGRAM_WEBHOOK_PUBLIC_URL=https://bot.example.com/telegram/webhook
TELEGRAM_WEBHOOK_SECRET=configure-this-locally
HTTP_WRITE_TIMEOUT=3m
```

Webhook processing is synchronous, so `HTTP_WRITE_TIMEOUT` must be at least `MEDIA_DOWNLOAD_TIMEOUT + 3*TELEGRAM_REQUEST_TIMEOUT` (2m45s with the example defaults). A reverse proxy or tunnel must provide public TLS.

Health and metrics routes:

```text
GET /healthz                          # Liveness probe (does not depend on DB)
GET /readyz                           # Readiness probe (verifies PostgreSQL connectivity)
GET /metrics                          # Prometheus metrics endpoint
POST /telegram/webhook                # Telegram webhook endpoint (webhook mode only)
```

## Configuration

Copy `.env.example` to `.env`. Core configuration variables:

| Variable | Meaning | Default / Notes |
| --- | --- | --- |
| `TELEGRAM_BOT_TOKEN` | BotFather bot token; secret. | Required. |
| `TELEGRAM_BOT_USERNAME` | Bot username without `@`. | Required. |
| `OWNER_TELEGRAM_IDS` | Comma-separated positive Telegram user IDs for privileged operator commands. | At least one required. |
| `DATABASE_URL` | PostgreSQL DSN (`sslmode=require` enforced in production). | Required. |
| `MEDIA_ENCRYPTION_KEY_ID` | Identifier stored beside new ciphertext. | `v1` |
| `MEDIA_ENCRYPTION_KEY` | Base64-encoded 32-byte AES-256-GCM key; secret. | Required. |
| `MEDIA_ENCRYPTION_PREVIOUS_KEYS` | Optional comma-separated previous keys for rotation (`id:base64,...`). | Optional. |
| `OBSERVED_IDENTITY_RETENTION` | Retention duration for observed inactive chat members and users. | `2160h` (90 days) |
| `POSTGRES_DB`, `POSTGRES_USER`, `POSTGRES_PASSWORD` | Required by the Compose PostgreSQL service. | Required. |

`.env.example` documents the complete configuration surface, including HTTP timeouts, Telegram polling/webhook behavior, connection pooling, whisper TTLs and rate limits, media storage, and background worker cleanup intervals. Compose-only substitutions are `ENV_FILE`, `VERSION`, `COMMIT`, `HTTP_BIND_HOST` (default `127.0.0.1`), and `HTTP_PORT` (default `8080`). PostgreSQL is not exposed to the public network.

## Usage

### User commands

| Command | Where | Behavior |
| --- | --- | --- |
| `/whisper` | Group / supergroup | Reply to another member's message to start a private whisper draft. |
| `/whisper @username` | Group / supergroup | Targets a specific username observed by the bot in that same group. |
| `/whisper 123456789` | Group / supergroup | Targets a numeric Telegram user ID observed in that same group. |
| `/start` | Private chat | Opens the bot composer or deep links directly to an active draft. |
| `/cancel` | Private chat | Cancels the sender's active whisper draft. |
| `/privacy` | Group or private chat | Displays the encryption model, retention periods, and privacy guarantees. |
| `/help` | Group or private chat | Shows commands and usage guidance. |

### How to send a whisper

You can send whispers using **Inline mode in ANY chat/group (the bot does NOT need to be in the chat!)** or **Group command mode**:

#### Flow 1: Instant Inline Whisper (1 Step — Text)
Send an encrypted text whisper directly from any group, channel discussion, or direct chat:
1. In any chat, type the bot username, the target `@username` or numeric ID, and your message (with or without quotes):
   ```text
   @yourbot @recipient_username "Here is the confidential password: 12345"
   ```
2. Tap the inline result popup **🔒 Send instant secret to @recipient_username**.
3. Telegram posts the locked envelope into the chat with a **`[ 🔓 Open Secret ]`** button.
4. **Recipient Action**: Only the recipient can tap the button to decrypt and view the secret immediately in private.

#### Flow 2: Media / DM Whisper (2 Steps — Photos, Videos, Voice, Files, Long Text)
Send media attachments or compose privately in DM:
1. In any chat, type the bot username and the target without secret text:
   ```text
   @yourbot @recipient_username
   ```
2. Tap the inline result popup **🖼️ / 📁 Add Media/Text in DM for @recipient_username** to post the envelope into the chat.
3. **Sender Action First**: Tap the **`[ ➕ Add or open privately ]`** button on the envelope to open a private composer with the bot in DM.
4. Send your photo, video, voice note, audio file, document (up to 20 MiB), or secret text with an optional caption.
5. **Recipient Action**: Once the sender uploads the secret, the recipient taps the envelope button in the group to decrypt and view the secret.

#### Flow 3: Group Command Mode (Optional, if bot is added to the group)
1. In a shared group where the bot is a member, reply to a user's message with `/whisper`, or type `/whisper @username` / `/whisper 123456789`.
2. The bot opens a private composer with you in DM.
3. Send your secret content in the DM.
4. The bot posts the content-free envelope in the group with an **`Open secret`** callback button for the recipient.

### Operator commands (privileged accounts only)

Accounts configured in `OWNER_TELEGRAM_IDS` have access to operational maintenance commands in private chat with the bot:

| Command | Where | Behavior |
| --- | --- | --- |
| `/owner_menu` | Private chat | Open the operator menu and compact whisper browser. |
| `/owner_list [page-size] [offset]` | Private chat | Browse retained whispers with expandable Telegram buttons and paging. |
| `/owner_sender <id\|@username>` | Private chat | Browse whispers sent by one Telegram user. |
| `/owner_media <image\|video\|recording>` | Private chat | Browse a media category; recording includes voice notes and audio files. |
| `/owner_last [media-type]` | Private chat | Show the latest whisper, optionally limited to a media category. |
| `/owner_open <id>` (`/owner_review`) | Private chat | Review and send decrypted content privately for a specific whisper ID. |
| `/owner_delete <id>` | Private chat | Hard-delete a stored whisper and its encrypted payloads immediately. |
| `/owner_retain <id> <duration>` (`/owner_set_retention`) | Private chat | Adjust retention window for a specific whisper. |
| `/owner_ephemeral <duration\|off>` | Private chat | Configure self-destruction after owner opens content. |

## Development

Canonical commands:

```sh
make check           # fmt-check + vet + test
make test-race       # go test -race ./...
make build           # static binary -> bin/secretmediabot
make run             # go run ./cmd/bot
```

All unit suites run without Telegram credentials. The PostgreSQL integration suite is skipped unless `TEST_DATABASE_URL` is set — use a disposable database; it applies embedded migrations and truncates application tables:

```sh
TEST_DATABASE_URL='postgres://user:password@127.0.0.1:5432/database?sslmode=disable' \
  go test -race ./internal/repository -run '^TestPostgresRepositoryIntegration$' -count=1
```

### Migrations

Goose SQL migrations live in `migrations/` and are embedded in the Go binary. GORM AutoMigrate is intentionally not used. Bot startup runs embedded migrations inside the Compose network before processing updates.

For a host-reachable DSN:

```sh
set -a; . ./.env; set +a
make migrate-status
make migrate-up
make migrate-down       # explicit and destructive
```

### Local code quality

The same gates run in CI. A quick local pass:

```sh
gofmt -l .
go vet ./...
docker run --rm -v "$PWD:/repo:ro" rhysd/actionlint:latest -color /repo/.github/workflows/ci.yml
docker run --rm -v "$PWD:/repo:ro" hadolint/hadolint:latest hadolint /repo/Dockerfile
gitleaks detect --source . --redact --no-banner
```

### Operations and Runbooks

Step-by-step procedures for routine and incident response operations are detailed in [docs/runbooks.md](docs/runbooks.md):

- **Automated Backup**: `docker compose --profile backup run --rm backup` (writes timestamped `.sql.gz` dumps to `./backups`).
- **Key Rotation**: Non-disruptive rotation using `MEDIA_ENCRYPTION_PREVIOUS_KEYS`.
- **Restore & Recovery**: Cold database restore procedures and incident handling.

## CI/CD and container images

One GitHub Actions workflow ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) runs on every push and on pull requests targeting `main`:

1. **Quality gates** — Go formatting, module verification (`go mod verify`), `go vet`, `go test -race`, and `govulncheck`.
2. **Lint** — `golangci-lint` with strict error checking.
3. **Database integration suite** — dedicated PostgreSQL 18 service job running full repository integration tests (`internal/repository/postgres_integration_test.go`).
4. **Dockerfile lint** — Hadolint.
5. **Secret scan** — Gitleaks over full Git commit history.
6. **Filesystem security scan** — Trivy (vulnerabilities, secrets, misconfigurations).
7. **Build, scan, and publish image** — builds the container image locally, performs an automated Trivy image vulnerability scan, and only publishes multi-arch images (`linux/amd64`, `linux/arm64`) to GHCR if all security scans pass cleanly.

Tags: every push receives immutable `commit-<full-sha>` and `commit-<short-sha>` tags plus a branch tag; the `main` branch also receives the mutable `latest`; Git tags produce semantic-version tags. Each successful publish uploads a `compose-image.env` artifact containing the exact manifest digest. To deploy that exact image with Compose:

```sh
export BOT_IMAGE_REF='ghcr.io/idanbot/secretmediabot@sha256:<digest from the artifact>'
docker compose up -d --no-build
```

Local development keeps building from source:

```sh
docker compose up -d --build
```

`Dockerfile` uses a Go builder stage with BuildKit module/build caches and a minimal distroless runtime stage running as non-root UID/GID `65532:65532` with all Linux capabilities dropped and a container `HEALTHCHECK` against `/healthz` (with a 60s startup period to allow for startup database migrations).

## Privacy and threat model

- Ordinary user-facing access is limited to the sender and selected recipient. The envelope itself contains no secret.
- PostgreSQL stores ciphertext, nonces, key IDs, integrity hashes, Telegram identifiers, participant IDs, state, and timestamps. The AES key stays outside PostgreSQL.
- The bot necessarily handles plaintext while collecting or delivering it. Telegram also receives the original upload and the delivered copy.
- Deleting the PostgreSQL row cannot remove the sender's original Telegram message, screenshots, client caches, or data retained by Telegram. `deleteEphemeralMessage` is a best-effort cleanup call, not a deletion guarantee.
- `protect_content=true` discourages forwarding/saving in supporting clients; it cannot prevent screenshots or recording with another device.
- PostgreSQL backups, WAL, replicas, and infrastructure snapshots may outlive deletion from the live database. This project does not claim immediate cryptographic erasure.

The full threat model — protected assets, trust boundaries, defenses, and explicit non-goals — is in [docs/architecture.md](docs/architecture.md#privacy-and-threat-model).

## Current caveats

- No live Telegram end-to-end test has been performed. Bot API 10.2+ ephemeral methods and fields must be verified against the intended bot, group types, clients, and deployment region.
- "One-time" is one accepted Telegram delivery, not exactly-once human viewing. A prolonged database failure after Telegram accepts a send can still permit a later duplicate delivery.
- Envelope publication has the same cross-system boundary: a prolonged failure while recording an accepted Telegram send can produce a duplicate content-free envelope on retry.
- The deletion queue is durable and defaults to 30 seconds after Telegram accepts delivery, but Telegram may reject deletion, the message may already be gone, or the recipient may capture it first.
- Key rotation is supported via `MEDIA_ENCRYPTION_PREVIOUS_KEYS`; see [docs/runbooks.md](docs/runbooks.md) for operational procedures.

## Documentation

| Document | Contents |
| --- | --- |
| [docs/architecture.md](docs/architecture.md) | Component boundaries, data model, state machines, concurrency, retention, and threat model. |
| [docs/runbooks.md](docs/runbooks.md) | Operational runbooks for backups, restores, key rotation, and incident response. |
| [docs/live-validation.md](docs/live-validation.md) | Manual validation checklist against live Telegram Bot API. |
| [docs/progress.md](docs/progress.md) | Implementation progress log against the improvements backlog. |
| [docs/improvements.md](docs/improvements.md) | Prioritized top-25 improvement backlog from the full-project review. |
| [docs/telegram-media-whisper-v1.md](docs/telegram-media-whisper-v1.md) | Historical initial V1 build specification. |
