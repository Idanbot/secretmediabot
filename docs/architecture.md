# Architecture

## Scope and maturity

This document describes the implemented V1 architecture. The executable, transports, handlers, services, repositories, workers, migrations, and Compose stack are wired and covered by unit tests; concurrency and persistence invariants also have an opt-in PostgreSQL integration suite. This is not a claim that the bot has been deployed, production-hardened, or tested end to end against live Telegram.

V1 accepts exactly one of:

- a text secret; or
- one photo, voice note, video, audio file, or document up to 20 MiB, with an optional caption.

The default recipient window is 24 hours, access is one-time, and live database content is retained until hard deletion 30 days after creation. The public group envelope never contains secret text, a caption, a thumbnail, or a filename.

## System context

```text
Sender Telegram client
          |
          | private text/media
          v
    Telegram Bot API <---- ephemeral delivery ---- Bot process
          ^                                         |
          | group update / callback                 | encrypted CRUD + leases
          +-----------------------------------------+----> PostgreSQL
                                                           |
                                                ciphertext, metadata, audit

AES key ----------------------------------------------> Bot process only
(environment/secret manager; never PostgreSQL)
```

The bot is a trusted plaintext endpoint. Encryption protects persisted payloads from a database-only disclosure.

## Component boundaries

### Telegram adapter

`internal/telegram` is a small `net/http` client instead of a broad third-party SDK. It currently implements the Bot API operations needed by the design, including long polling, webhook management, user/member lookup, file metadata and download, ordinary messages, ephemeral text/media sends, callback answers, and ephemeral deletion.

The client:

- bounds Telegram API responses and downloads;
- enforces the configured 20 MiB download/upload limit;
- does not retry time-sensitive ephemeral sends;
- redacts the bot token from typed errors;
- supports `receiver_user_id`, `callback_query_id`, `protect_content`, and `ephemeral_message_id` fields directly.

### HTTP and update transports

`internal/app.Poller` serially consumes `message` and `callback_query` updates, advances the offset only after processing, and backs off on failure. The webhook adapter exposes the same processor interface, requires an `application/json` content type, authenticates Telegram's secret header in constant time, and places a 1 MiB limit on request bodies.

Only one transport should be active:

- `polling` for development or an outbound-only deployment;
- `webhook` behind a stable public HTTPS endpoint.

Webhook processing is synchronous. Configuration therefore requires `HTTP_WRITE_TIMEOUT >= MEDIA_DOWNLOAD_TIMEOUT + 3*TELEGRAM_REQUEST_TIMEOUT`; with the example values, webhook mode should set the write timeout to at least 2m45s (the documented example uses 3m). `TELEGRAM_WEBHOOK_MAX_CONNECTIONS` is validated in Telegram's 1–100 range and defaults to 4, bounding concurrent update handlers and their database/media load.

Both transports feed the same implemented update processor. It hashes each update, claims a durable PostgreSQL processing lease, dispatches it once to the bot handler, and records success or a retryable failure. Polling mode automatically deletes an existing webhook before starting; webhook mode automatically calls `setWebhook` during startup and does not run the poller.

### Domain

`internal/domain` keeps persistence-independent models and legal transitions:

- users, chats, and observed chat membership;
- draft ownership and ingestion leases;
- text/media content references containing UUIDs and metadata, never payload bytes;
- separate recipient-lifecycle and envelope-publication states;
- open-attempt/delivery events and audit action types;
- availability and retention timestamps.

### Cryptography

`internal/secretcrypto` uses AES-256-GCM with a fresh 12-byte random nonce for every encrypted record. Associated data is derived from:

```text
secretsantabot:v1:<media|text|caption|callback>:<immutable-record-uuid>[:<whisper-uuid>]

The `secretsantabot` namespace is retained in associated data for compatibility with ciphertext already stored by earlier local deployments; it is not the repository or module name.
```

This binds ciphertext to its row, semantic purpose, and—where supplied—parent whisper. PostgreSQL stores the key ID, nonce, ciphertext, and SHA-256 of the ciphertext. The active 32-byte key is supplied through `MEDIA_ENCRYPTION_KEY`; the key itself is not stored in PostgreSQL. The keyring code can decrypt records by key ID, allowing a future rotation process to retain old keys while selecting one active key for new writes.

The configuration currently supplies one active key. Operational key rotation and external secret-manager integration remain to be built. Losing the key makes retained payloads unrecoverable; obtaining both the key and database defeats at-rest encryption.

### Persistence

Goose SQL migrations are authoritative. They define constraints, foreign keys, partial indexes, retry queues, leases, and deletion behavior that GORM AutoMigrate cannot safely express. `internal/repository` opens GORM over PostgreSQL, disables GORM's SQL logger to avoid secret-bearing parameters, configures the pool, runs the embedded Goose migrations, persists observed identities/memberships, performs same-chat recipient lookup, and keeps ordinary whisper projections free of ciphertext and Telegram file IDs.

The implementation split is:

- GORM for routine, non-contentious CRUD and mapping;
- explicit GORM transactions plus conditional/raw SQL for state-machine changes;
- Goose only for schema evolution; never GORM AutoMigrate.

The repository implements identity lookup, atomic draft quotas and ingestion leases, transactional encrypted finalization, publication leases/retries, one-time open reservation/completion/failure, audited hard deletion and retention updates, processed-update leases, durable ephemeral-delete jobs, and bounded cleanup. PostgreSQL integration tests exercise critical constraints, concurrent claims, rollback, idempotent open completion, durable jobs, audit action selection, and retention cascades.

## Data model

### Identity and recipient lookup

`users.telegram_user_id` is the identity. Usernames are optional, mutable, normalized to lowercase without `@`, and indexed only as lookup hints.

`observed_chat_members` scopes target lookup to a source chat. Both sender and recipient references on a draft/whisper have composite foreign keys back to an observed membership in that chat. Implemented `/whisper @username` and `/whisper <positive-id>` resolution therefore cannot turn the bot into an arbitrary or global Telegram directory.

If a username maps to multiple observed users, the service fails closed and asks for a reply or numeric ID. Telegram usernames are mutable: a unique but stale cached mapping cannot be detected reliably, so replying to the intended user or using a verified numeric ID is safer. Authorization never compares usernames or display names.

### Drafts

`whisper_drafts` records the sender, resolved recipient, group/thread context, a 10-minute expiry, and a SHA-256 hash of an opaque compose token. It has no secret payload.

V1 permits exactly one active draft per sender. Configuration validates `MAX_ACTIVE_DRAFTS_PER_USER=1`, creation takes a sender-scoped PostgreSQL advisory transaction lock, and a partial unique index enforces the invariant for `awaiting_media`/`ingesting_media` states. This makes an ordinary private text/media update unambiguous.

The ingestion lease prevents duplicate handlers from downloading and encrypting the same Telegram update concurrently:

```text
awaiting_media ----claim----> ingesting_media ----commit----> completed
      |                             |     |
      +----cancel/expiry------------+     +----lease release----> awaiting_media
                                    +----------cancel/expiry----> terminal
```

The historical state name `awaiting_media` also covers a text secret in V1.

### Whispers and encrypted payloads

`whispers` stores participants, group context, opaque-token hash, payload kind, Telegram source identifiers, recipient lifecycle, publication lifecycle, and timestamps. It does not contain plaintext or ciphertext payload bytes.

Payload cardinality is enforced by the service transaction:

- `payload_kind=text`: exactly one `encrypted_text_payloads` row with purpose `text`, no media row, no caption row;
- `payload_kind=media`: exactly one `media_blobs` row and zero or one `encrypted_text_payloads` row with purpose `caption`.

`media_blobs` and `encrypted_text_payloads` cascade when their parent whisper is deleted. Media plaintext size is constrained to the fixed V1 maximum of 20 MiB in configuration, the service, and PostgreSQL. No original filename is persisted. The service creates the whisper and its required child rows in one transaction because ordinary SQL `CHECK` constraints cannot assert the existence of rows in another table.

Telegram `file_id` and `file_unique_id` remain source/delivery metadata. Keeping an encrypted PostgreSQL copy does not remove Telegram's own copy.

`encrypted_callback_tokens` solves publication retry without storing a forgeable callback token in plaintext. It temporarily stores the raw callback token encrypted with row/whisper-bound associated data, while `whispers.open_token_hash` remains the lookup value. A publisher decrypts the temporary row to reproduce `callback_data`; the transaction that durably marks the envelope published deletes that row.

### Events and audit

`whisper_open_events` records the actor, callback ID, allow/deny result, denial reason, delivery state, Telegram ephemeral message ID, and completion/error metadata. It supports authorization review and delivery diagnosis; it is not a read receipt.

`owner_audit_events` has fields for actor ID, action, success, whisper ID when available, and JSON details. Audit details contain identifiers and diagnostic codes, not secret text, captions, file IDs, filenames, ciphertext, decrypted bytes, bot tokens, or encryption keys. The table intentionally has no foreign key to `whispers`, so audit evidence survives content retention deletion.

## State machines and concurrency

### Recipient lifecycle

```text
active ----atomic reserve----> opening ----delivery accepted----> opened
   ^                              |
   +---------release/lease expiry-+

active/opening ----TTL----> expired
```

For the default `one_time=true`, only one transaction may move an unexpired, published whisper from `active` to `opening`. A short opening lease allows a crash or Telegram delivery failure to release the reservation back to `active`. A successful delivery moves it to `opened`, preventing another open.

After Telegram accepts the ephemeral send, the handler retries database completion. Completion is idempotent for the same whisper, event, callback, and ephemeral message ID: an already-delivered event succeeds only when its matching durable deletion job exists. A mismatched retry fails closed. This narrows—but cannot eliminate—the cross-system ambiguity if Telegram accepts delivery while PostgreSQL remains unavailable beyond the completion retry and opening lease.

Reusable whispers are represented in the domain for future policy, but V1 defaults to one-time and should not expose a reusable command unless explicitly added later.

The domain/schema also reserve a `revoked` state, but the current V1 command and repository surface does not expose a revoke transition.

### Envelope publication

Publication is independent from recipient lifecycle:

```text
pending -> publishing -> published
              |
              +-> retry_wait -> publishing
              +-> failed (terminal until operator intervention)
```

Separating these states avoids marking a secret expired or opened merely because sending the public envelope failed. Publishing uses an attempt counter, next-attempt timestamp, and lease so a worker can reclaim abandoned work. The recipient cannot open until publication is `published`.

The request path attempts immediate publication after finalization. Failures remain queued; the publication worker polls at `PUBLISH_INTERVAL` (2 seconds by default), drains a bounded batch, and applies bounded operational backoff. The temporary encrypted callback-token row lets a retry reproduce the button without storing callback plaintext. As with recipient delivery, Telegram acceptance and PostgreSQL completion are separate systems, so a prolonged completion failure can produce a duplicate content-free envelope.

### Required SQL shape

State changes do not use an unprotected read-then-write sequence. Repositories use one transaction and conditional statements such as:

```sql
UPDATE whispers
SET status = 'opening',
    opening_reserved_at = NOW(),
    opening_lease_until = $lease_until,
    opening_callback_query_id = $callback_id,
    updated_at = NOW()
WHERE id = $whisper_id
  AND recipient_id = $actor_id
  AND one_time = TRUE
  AND status = 'active'
  AND publish_state = 'published'
  AND expires_at > NOW()
RETURNING id;
```

The private-ingestion transaction similarly claims the draft, encrypts and inserts the whisper plus child payload rows, marks the draft complete, and commits before publication. Processed Telegram updates use their own lease table for idempotency.

These SQL shapes are implemented repository invariants. Raw/conditional SQL is limited to atomic queue and state-machine claims; typed projections deliberately avoid loading large media ciphertext during publication or recipient reservation.

## Private composer binding

`/whisper` first tries to send a protected composer prompt to the sender's private chat. If Telegram does not allow that direct prompt, the group response carries a `t.me/<bot>?start=compose_<opaque-token>` link. `/start compose_<token>` verifies ownership and resumes that draft. Because V1 enforces one active draft per sender, a subsequent non-command private text/media update can atomically claim the single latest active or reclaimable ingestion lease without guessing among recipients. `/cancel` cancels that active draft.

## Secret creation and delivery flow

The user-facing command surface is intentionally small:

- `/whisper` as a group reply, `/whisper @username`, or `/whisper <positive-id>` creates the one active draft;
- `/start` in private chat shows guidance, while `/start compose_<token>` resumes an owned draft from the generated deep link;
- `/cancel` cancels the sender's active private draft;
- `/privacy` in group or private chat explains the trust and retention model;
- `/help` provides group/private usage guidance.

1. Observe/upsert the sender, target, chat, and membership metadata from Telegram.
2. Parse a positive numeric ID or normalized `@username`, then resolve it only within `observed_chat_members` for the source group.
3. Create a draft with a 10-minute TTL and private compose token.
4. Accept text or exactly one supported media value. Reject albums, multiple attachments, unsupported types, and media over 20 MiB.
5. For media, obtain the Telegram file, download with both declared-size and actual-byte limits, and clear plaintext buffers on a best-effort basis after encryption. Encrypt an optional caption separately.
6. In one database transaction, insert the whisper and required encrypted child records, complete the draft, and enqueue envelope publication.
7. Publish an envelope containing only sender/recipient presentation data and a button with `w:<opaque-token>`. Store SHA-256 for open lookup and only a temporary encrypted copy of the raw token for crash-safe publication retries; delete that encrypted copy when publication is durably marked successful.
8. On callback, parse exactly 32 random bytes encoded as canonical unpadded base64url, look up its hash, and compare `callback.from.id` with the stored recipient ID.
9. Atomically reserve the one-time open, decrypt only the required payload, and send it ephemerally in the original chat within Telegram's callback eligibility window with `protect_content=true`.
10. Idempotently record the delivery result, clear plaintext buffers where practical, and insert a durable deletion job in the same transaction. By default the deletion worker begins attempts 30 seconds after Telegram accepts delivery and polls every 2 seconds, retrying transient failures with bounded backoff. Telegram does not provide a read receipt.

## One-time access is not a read receipt

Telegram reports whether the Bot API accepted a send, not whether the human saw it. Therefore:

- `opening` means the backend reserved an attempt;
- `opened` means Telegram accepted delivery and the one-time capability was consumed;
- an open event can say `delivered`, but never truthfully say `read`;
- recipient offline state, client restart, network behavior, or Telegram's ephemeral semantics may prevent actual viewing.

The service should tell users that delivery is best effort. It must not claim that the secret self-destructed immediately after being read.

## Availability, retention, and deletion

Three concepts are intentionally separate:

| Concept | Default | Meaning |
| --- | --- | --- |
| Draft TTL | 10 minutes | Time for the sender to submit content. |
| Recipient availability | 24 hours | Time in which an unconsumed published whisper may be opened. |
| Live content retention | 30 days from creation | Time before hard deletion of the whisper and encrypted child rows. |

Opening a one-time whisper removes recipient access but does not immediately remove retained content. V1 requires content and metadata deadlines to match. At `retention_delete_at`, the cleanup worker deletes the whisper row in bounded batches; PostgreSQL cascades to media/text/caption ciphertext and any unpublished callback-token row. Audit rows survive because they deliberately have no whisper foreign key.

The cleanup worker runs once at startup and then every `CLEANUP_INTERVAL` (5 minutes by default). It also expires drafts/whispers, releases abandoned ingestion/open/publication leases, and prunes old processed-update and completed ephemeral-job rows according to configured bounds.

An early hard deletion uses the same delete path and records an audit event. Retention changes must update parent and child deadlines transactionally so they cannot disagree.

“Hard delete” means the live `whispers` row and its encrypted text/media/caption/callback children are absent. Related identity, completed-draft, processed-update, and audit metadata may remain under their own lifecycle rules. Deletion does not promise removal from PostgreSQL WAL, backups, replicas, host snapshots, Telegram, user devices, or logs outside this application's control.

## Ephemeral deletion caveats

Successful open completion stores a deletion job with the source chat, recipient, returned `ephemeral_message_id`, due time, attempts, and lease. The worker polls at `EPHEMERAL_DELETE_INTERVAL` (2 seconds by default), calls `deleteEphemeralMessage` after `EPHEMERAL_DELETE_AFTER` (30 seconds by default), treats permanent Telegram 4xx responses as complete, and retries transient failures with backoff up to five minutes. The queue and lease survive process restarts, but:

- Telegram may remove the message itself before the call;
- the process may crash after Telegram accepts a send but before the completion/deletion-job transaction commits;
- the recipient may be offline or restart a client;
- Telegram can reject or lose the delete request;
- the recipient can capture the content before deletion;
- deletion affects only that ephemeral delivery, not the sender's original message.

Failure state remains on the durable job without reopening a consumed one-time secret or claiming it remains unread. Successfully deleted jobs are later removed by cleanup.

## Privacy and threat model

### Protected assets

- secret text, media, and captions;
- the AES key and Telegram bot token;
- callback/compose token plaintext;
- participant and group association metadata;
- audit history.

### Trust boundaries

| Actor or compromise | What it can learn/do |
| --- | --- |
| Unrelated Telegram group member | Sees the public envelope and presentation metadata, but should not receive payload content. |
| Intended recipient | Can request one delivery during the availability window; can copy or capture what their client displays. |
| Sender | Already knows the submitted secret and retains their original Telegram private-chat message unless they delete it. |
| Telegram | Receives uploads, identifiers, captions/text used for delivery, and ephemeral copies. |
| Database-only attacker | Sees metadata, ciphertext, nonces, hashes, and IDs; should not decrypt payloads without the external AES key. |
| Runtime/secret-store attacker | Can use the AES key and bot token and may access plaintext; this is outside the protection of at-rest encryption. |

### Defenses

- stable numeric-ID authorization and same-chat lookup scope;
- opaque 256-bit callback tokens with only SHA-256 stored;
- conditional SQL, leases, and idempotent update claims;
- AES-256-GCM with random nonces and row/purpose-bound associated data;
- no plaintext payload columns or persisted filenames;
- bounded network bodies and media sizes;
- webhook secret validation and loopback-only HTTP binding by default;
- redacted Telegram errors, disabled SQL parameter logging, and a non-root read-only container;
- durable audit evidence.

### Explicit non-goals and residual risks

- no end-to-end encryption;
- no guarantee against screenshots, forwarding exploits, cameras, compromised clients, or malicious recipients;
- no reliable read receipt or guaranteed ephemeral delivery/deletion;
- no guarantee that Telegram deletes its stored file when this service deletes its database copy;
- no immediate erasure guarantee across backups/WAL;
- no public web content endpoint, dashboard, Mini App, object storage, Redis, or multi-region design in V1;
- audit records cannot prevent an infrastructure administrator from bypassing the bot path.

## Deployment posture

The default Compose stack contains only the bot and PostgreSQL. PostgreSQL is internal-only. The bot port maps to `127.0.0.1:8080` unless explicitly changed, which is suitable for local polling or a host reverse proxy/tunnel.

`cmd/bot` validates configuration, opens PostgreSQL, applies embedded migrations, initializes the keyring, verifies `getMe`, configures polling or webhook mode, then starts the HTTP server and cleanup, publication, and ephemeral-deletion workers. Polling adds a fifth runner. A signal or unexpected runner exit cancels the shared context; HTTP receives a bounded graceful shutdown and the executable waits for all workers.

Before calling the service production-ready, at minimum validate or add:

- live Telegram group/supergroup tests for Bot API 10.2+ ephemeral fields, offline recipients, topics, media types, simultaneous button presses, and deletion behavior;
- deployment monitoring, alerting, log review, database capacity limits, and migration/rollback procedures;
- backup, restore, WAL/replica retention, key rotation, external secret management, and incident response;
- a durable security-audit path for rejected or failed privileged operations if that is required by policy;
- operational handling for cross-system ambiguous successes that can duplicate an ephemeral delivery or empty envelope;
- a recipient fallback that decrypts/reuploads the retained media blob if Telegram's stored `file_id` can no longer be reused.
