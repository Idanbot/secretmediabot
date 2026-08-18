# Improvement backlog — top 25

Full-project review of V1 (commit `c453fa8`, 2026-08-18) covering `cmd/`, all `internal/` packages, migrations, CI/CD, container/compose, Makefile, and docs. Findings are consolidated and ranked by criticality: **P0** = remotely triggerable outage, secret-loss/integrity break, or blocker for any production use; **P1** = high-impact security, correctness, or pipeline gap; **P2** = important robustness, operational, or compliance work. Effort is a rough S/M/L estimate.

Items marked *documented* are already acknowledged in [README caveats](../README.md#current-caveats) or [architecture.md](architecture.md); they are listed here because they remain the most needed improvements, not new discoveries.

| # | Priority | Title | Area | Effort |
| --- | --- | --- | --- | --- |
| 1 | P0 | Poison updates can crash-loop or permanently wedge the bot | app/poller | M |
| 2 | P0 | No live Telegram end-to-end validation *documented* | testing | M |
| 3 | P0 | Guest/inline path has no rate limiting — unbounded DB writes by any user | service/bot | M |
| 4 | P1 | Guest username-targeted secrets hijackable; renamed targets locked out | guest flow | M |
| 5 | P1 | HTTP client follows redirects — plaintext secret exfiltration vector | telegram client | S |
| 6 | P1 | Terminal publication failure strands the whisper and lies to the sender | publish flow | M |
| 7 | P1 | Container healthcheck hardcodes the port and conflates liveness with readiness | cmd/Dockerfile | S |
| 8 | P1 | CI publishes the image before scanning it; supply chain is unpinned | CI/CD | M |
| 9 | P1 | PostgreSQL integration suite never runs in CI | CI/CD | S |
| 10 | P1 | Fail-open access posture: empty chat allowlist and guest/inline bypass | config/service | S |
| 11 | P1 | `upsertUser` stub clobbers stored profile data | repository | S |
| 12 | P1 | Database TLS not enforced; example DSN ships `sslmode=disable` | config | S |
| 13 | P2 | Key rotation is not operable (single-key config) *documented* | crypto | M |
| 14 | P2 | Several tables grow without bound | repository | M |
| 15 | P2 | Missing indexes for guest, cleanup, and rate-limit queries | migrations | S |
| 16 | P2 | One-time guarantee can be violated after a crash mid-delivery *documented* | opening | M |
| 17 | P2 | Dead `file_id` sends the recipient into an infinite retry loop | delivery | M |
| 18 | P2 | Guest flow correctness and dead-end batch | guest flow | M |
| 19 | P2 | No metrics or observability surface | app/httpserver | M |
| 20 | P2 | Worker and shutdown robustness gaps | app/cmd | S |
| 21 | P2 | Telegram client resilience: 429 compliance, error predicates, download integrity | telegram client | M |
| 22 | P2 | Compose/deployment hardening and no backup story | compose | M |
| 23 | P2 | No LICENSE despite a public GHCR image | repo | S |
| 24 | P2 | Spec drift unannotated; operational runbooks missing | docs | M |
| 25 | P2 | Crypto and memory hygiene details | crypto/service | M |

---

## P0 — critical

### 1. Poison updates can crash-loop or permanently wedge the bot

**Problem.** Three gaps combine into remotely triggerable total outage from a single crafted or merely unusual Telegram update:

- No `recover()` exists anywhere in the repo. A panic inside `HandleUpdate` propagates through `Poller.Run` into the runner goroutine and kills the process (`internal/app/processor.go:69`, `internal/app/poller.go:75-95`, `cmd/bot/main.go:211-217`). Because the polling offset is in-memory only and the `processed_updates` lease expires during restart, the same update is re-fetched and panics again — infinite crash loop.
- There is no attempt cap or dead-letter on updates. `ClaimUpdate` increments `attempt_count` forever (`internal/repository/updates.go:64-79`), and on any `Process` error the poller never advances the offset (`internal/app/poller.go:82-95`). One deterministically failing update stalls the entire update stream. Concrete trigger: a one-shot guest query that expired makes `CreateGuestRequest` hit the `guest_query_id` uniqueness conflict on every redelivery, failing forever.
- `getUpdates` decodes the whole batch in one `json.Unmarshal`; a single malformed update returns a `ProtocolError`, the poller retries at the same offset, and Telegram returns the same poisoned batch every time (`internal/telegram/client.go:470-472`).

**Improvement.** Wrap handler dispatch in `recover()` that records `FailUpdate` with a `panic_recovered` marker; decode updates individually from `[]json.RawMessage` and quarantine undecodable ones; add a max-attempt threshold after which an update is marked terminally failed and skipped (offset advances / webhook returns 200), with a redacted dead-letter log line and counter.

### 2. No live Telegram end-to-end validation

**Problem.** The product's core mechanics — Bot API 10.2+ ephemeral-message fields (`receiver_user_id`, `ephemeral_message_id`), `protect_content` behavior, ephemeral deletion, offline recipients, simultaneous button presses — have never been exercised against a real bot and real clients (README caveats, `docs/architecture.md:325`). Everything else on this list is secondary if the delivery primitive itself behaves differently than assumed.

**Improvement.** Run a scripted live validation pass: supergroup + private flows, text and each media type, recipient offline/online, deletion timing, guest and inline modes, both polling and webhook. Record results in a validation checklist checked into `docs/`, and turn any behavioral surprise into either a code fix or an explicit documented limitation.

### 3. Guest/inline path has no rate limiting — unbounded DB writes by any user

**Problem.** The whisper-draft path enforces active-draft and per-hour quotas (`internal/service/drafts.go:76-89`), but `CreateGuestRequest` enforces nothing (`internal/service/guest.go:84-128`): every guest mention and every inline query creates a fresh `guest_secret_requests` row, a 32-byte token, and identity upserts. Inline answers set `CacheTime: 0` (`internal/bot/guest.go:82,123`), defeating Telegram's query cache, so **every keystroke** of `@bot @target` in any chat on Telegram produces a DB write with zero friction. Unused rows sit for 24 h + 30-day retention. Separately, every denied "Open secret" button press by any group member inserts a `whisper_open_events` row under a `FOR UPDATE` lock on the whisper row (`internal/repository/opening.go:301-318`) — hammering the button is unbounded write amplification plus lock contention.

**Improvement.** Apply the same quota pattern to guest creation (per-sender outstanding + hourly counters, transactionally enforced); set a nonzero inline `CacheTime` with stable result IDs; dedupe/replace a sender's pending `awaiting_secret` request; cap or coalesce per-(whisper, user) denial events and stop locking the whisper row for pure denials.

## P1 — high

### 4. Guest username-targeted secrets hijackable; renamed targets locked out

**Problem.** Two opposite bugs in username-bound guest requests:

- Guest requests authorize the first claimant whose *current* username matches, for the whole 24 h TTL (`internal/repository/guest.go:346-351`). If the intended target releases or changes their username, any group member can register it, claim the secret, and permanently bind `target_user_id` to themselves.
- Once claimed, `target_user_id` is persisted but the username is re-verified on every subsequent step and at open time (`internal/repository/guest.go:343-348, 493-498`). If the legitimate target renames after claiming, they are denied with "This locked secret is for another Telegram user" (`internal/bot/messages.go:525-526`) — permanently, since numeric IDs are authoritative.

**Improvement.** Skip the username check once `TargetUserID` is set (numeric ID is authoritative by design). For the pre-claim window: prefer numeric targets in guest mode, warn senders about username volatility, and/or shorten the username-claim window.

### 5. HTTP client follows redirects — plaintext secret exfiltration vector

**Problem.** Neither the default nor the production client sets `CheckRedirect` (`internal/telegram/client.go:156-159`, `cmd/bot/main.go:99-106`). Go forwards 307/308 with method **and body** preserved: a single redirect response exfiltrates plaintext secret text (`sendMessage`) or decrypted media (multipart upload, `internal/telegram/media.go:299`) to an attacker-chosen host. The Bot API never legitimately redirects; plain-HTTP base URLs are permitted outside production.

**Improvement.** Set `CheckRedirect` to reject all redirects (or same-host only) on the API client and the download client. One-line fix, high defense value.

### 6. Terminal publication failure strands the whisper and lies to the sender

**Problem.** When Telegram permanently rejects the envelope (bot kicked from the group), the whisper ends in `publish_state='failed'` while staying `status='active'` (`internal/repository/publish.go:150-185`). The `failed → pending` edge is legal in the domain model but no code ever performs it, so the whisper can never be opened, never republished, and lives until 30-day retention — meanwhile the sender was told "Secret stored securely. The group envelope is queued for delivery." (`internal/bot/messages.go:396-406`).

**Improvement.** On terminal publication failure, notify the sender in private chat, and either implement a bounded `failed → pending` retry path (operator-triggered or automatic with a budget) or move the whisper to a visibly terminal state.

### 7. Container healthcheck hardcodes the port and conflates liveness with readiness

**Problem.** The `healthcheck` subcommand probes `http://127.0.0.1:8080/readyz` regardless of `HTTP_ADDR` (`cmd/bot/main.go:47-58`, `Dockerfile:40-41`). Any non-default port/bind makes a healthy container restart-loop under `restart: unless-stopped`. Worse, `/readyz` pings PostgreSQL (`internal/httpserver/server.go:57-70`), so a transient DB outage longer than ~105 s (start-period + 3 retries) kills and restarts the bot mid-flight — exactly the situation the lease/backoff design is built to survive. `start-period=15s` is also shorter than the 2-minute migration budget.

**Improvement.** Derive the probe target from `HTTP_ADDR`; probe `/healthz` for the container HEALTHCHECK (pure liveness); raise `start-period` above the migration budget; keep worker/DB detail out of the restart decision (or move it to metrics, item 19).

### 8. CI publishes the image before scanning it; supply chain is unpinned

**Problem.** `build-push-action` pushes to public GHCR immediately; the Trivy image scan with `exit-code: 1` runs only afterward (`.github/workflows/ci.yml:167-211`). A CRITICAL finding fails the job only after the image is already downloadable. Additionally: all third-party actions are tag-pinned, not SHA-pinned; `go run golang.org/x/vuln/cmd/govulncheck@latest` executes whatever is latest at run time; and `permissions: packages: write, id-token: write` apply to every job including lint/test (`ci.yml:10-14`). No Dependabot/Renovate, no `go mod verify`/tidy-diff gate.

**Improvement.** Build+load locally, scan, push only on green (or push to a quarantine tag, then promote). Pin actions by full commit SHA and govulncheck to a fixed version. Set workflow-level `permissions: contents: read`, grant `packages: write` only in the image job. Add Dependabot and a module-hygiene gate.

### 9. PostgreSQL integration suite never runs in CI

**Problem.** The 888-line integration suite covering the riskiest code — one-time-open atomicity, concurrent claims, leases, rollback, retention cascades — is skipped unless `TEST_DATABASE_URL` is set, and CI never sets it (`.github/workflows/ci.yml:51-52`, `internal/repository/postgres_integration_test.go:48-51`).

**Improvement.** Add an `integration` job with a `services:` PostgreSQL container exporting `TEST_DATABASE_URL`, and make it a required gate for the image job alongside `quality`.

### 10. Fail-open access posture: empty chat allowlist and guest/inline bypass

**Problem.** `ALLOWED_CHAT_IDS` empty means *all* groups are allowed (`internal/config/config.go:391-401`) — an operator who forgets the variable silently gets a bot that harvests membership observations in any group that adds it. And even with a configured allowlist, `CreateGuestRequest` never checks it and inline queries have no chat at all (`internal/service/drafts.go:58-60` vs `internal/service/guest.go:84-128`), so the footprint control is silently bypassed by the newer flows.

**Improvement.** In production-like environments, warn loudly or fail closed on an empty allowlist (or require explicit `ALLOW_ALL_CHATS=true`); add a documented guest/inline enable switch so operators can actually scope the bot's footprint.

### 11. `upsertUser` stub clobbers stored profile data

**Problem.** `upsertUser`'s `ON CONFLICT` unconditionally overwrites `username`, `first_name`, `last_name`, `is_bot`, `language_code` (`internal/repository/identity.go:147-159`). `CreateGuestRequest` calls it with a stub containing only a Telegram ID (`internal/repository/guest.go:273-277`) — creating a guest request targeting an existing, fully-profiled user wipes their stored username and names to empty strings, degrading `FindObservedUserByUsername` and observed-membership data.

**Improvement.** Either make the conflict clause fall back to existing values when the incoming value is empty (`NULLIF(EXCLUDED.username, '')` pattern), or use `INSERT ... ON CONFLICT DO NOTHING` for the stub path.

### 12. Database TLS not enforced; example DSN ships `sslmode=disable`

**Problem.** `validateDatabaseURL` checks only scheme/host (`internal/config/config.go:414-420`), and `.env.example:43` documents `sslmode=disable`. For a threat model whose centerpiece is protecting stored secrets, ciphertext, token hashes, and audit rows travel the DB channel in plaintext without any warning, in every environment including production.

**Improvement.** Reject `sslmode=disable`/`allow` when `APP_ENV` is production-like; change the example to `sslmode=require` with a comment; document the verify-full option.

## P2 — medium

### 13. Key rotation is not operable

The keyring supports decrypt-by-key-ID for exactly this purpose (`internal/secretcrypto/cipher.go:69-106`), but configuration accepts only one key (`MEDIA_ENCRYPTION_KEY`/`..._KEY_ID`), so performing a rotation today orphans every existing row — all decrypts fail with `ErrUnknownKey`, breaking publication, opens, guest delivery, and owner review simultaneously. Add `MEDIA_ENCRYPTION_KEYS=id:key,...` for previous keys alongside the active one, document the rotation procedure (the keyring already supports it), and add the two-key keyring test that is currently missing. The README caveats already list this as unbuilt.

### 14. Several tables grow without bound

Four distinct retention gaps: (a) `whisper_drafts` rows are never deleted — only transitioned to `expired` (`internal/repository/cleanup.go:31-37`) — so completed/cancelled/expired drafts and their compose-token hashes accumulate forever; (b) `processed_updates` rows in `failed` state (and abandoned `processing` rows) match no cleanup path (`internal/repository/cleanup.go:149-165`); (c) ephemeral/guest delete jobs have no terminal-failure cap — a message that can never be deleted retries forever and is never purged (`internal/repository/updates.go:139-223`); (d) `users`/`chats`/`observed_chat_members` grow monotonically with no prune path, and composite FKs make membership pruning impossible while whispers reference it. Add retention deadlines or state-agnostic age-based cleanup for each, plus a `gave_up` terminal state for delete jobs.

### 15. Missing indexes for guest, cleanup, and rate-limit queries

The whisper tables have careful partial indexes, but the guest additions did not: guest expiry/ingest-lease/opening-lease sweeps and retention deletes (`internal/repository/cleanup.go:104-127, 201-212`), the guest delete-job due-queue (`internal/repository/guest.go:641-656`), and the per-sender hourly whisper count paid on every `CreateDraft` (`internal/repository/drafts.go:83-91`) all degrade to sequential scans as tables grow. Add the corresponding partial indexes in a new migration.

### 16. One-time guarantee can be violated after a crash mid-delivery

If the process dies after Telegram accepts the ephemeral send but before `CompleteOpen` commits, the opening lease expires, cleanup resets the whisper to `active` (`internal/repository/cleanup.go:73-85`), and a second press delivers the "one-time" secret again. Documented, and deliberately tight (lease sizing covers the handler's worst case), but the reset path makes no use of the recorded `whisper_open_events` delivery state. Improvement: refuse re-reservation when a prior open event for the same whisper already records a delivered state, trading a rarer stuck-active outcome for not breaking the one-time promise.

### 17. Dead `file_id` sends the recipient into an infinite retry loop

Ephemeral media delivery only resends the stored Telegram `file_id` (`internal/telegram/media.go:141-191`). If Telegram permanently rejects it (400 "file id invalid" after server-side rotation), the failure is treated as transient: `FailOpen` releases the reservation and the user is told to press again (`internal/bot/callback.go:32-45`) — an infinite, never-succeeding loop. The encrypted bytes are already in PostgreSQL (that is the whole point of downloading them), but no re-upload path exists and `SendEphemeralMedia` cannot take bytes. Improvement: classify permanent 4xx (excluding 429) in the open path, surface "no longer available", and add an ephemeral multipart re-upload variant as the recovery path (architecture.md:330 already lists this).

### 18. Guest flow correctness and dead-end batch

Accumulated guest-flow bugs, each small, together a rough edge: (a) `CancelGuestRequest` cancels *all* of a sender's active requests but returns `ErrNotFound` unless exactly one row matched (`internal/repository/guest.go:624-625`) — with 2+ requests (trivial given item 3) the user is falsely told "No active draft was found" after the cancels already happened; (b) an `opening`-state request (crash between claim and completion) yields wrong guidance in both roles — target told "not ready yet", sender told "no longer available" — until the 5-minute cleanup pass resets the lease (`internal/bot/messages.go:194-211`, and no inline lease takeover unlike the whisper path at `internal/repository/opening.go:73-90`); (c) `ReserveGuestOpen` dereferences a possibly-nil lease *before* its own nil guard (`internal/service/guest.go:285,293-297`) — a panic in the worker; (d) a pending guest request silently hijacks the private composer, starving an active `/whisper` draft until it expires without notice (`internal/bot/messages.go:151-157`); (e) drafts and guest requests expire silently — sender gets no expiry notification, target stares at "sender has not added the secret yet" for hours.

### 19. No metrics or observability surface

Zero Prometheus/OTel/tracing references in the codebase; the server exposes only `/healthz`/`/readyz`. Queue depths (pending publications, delete jobs), lease contention, dead-letter counts, retry ages, publication-terminal-failure counts, and backoff saturation are all invisible — the original spec even defines the metric set (`docs/telegram-media-whisper-v1.md:2123-2151`). All diagnosis depends on deliberately detail-free log lines. Improvement: loopback-bound `/metrics` with the spec's low-cardinality counters/histograms; consider a per-update correlation ID in the slog context and periodic queue-depth stats lines at minimum.

### 20. Worker and shutdown robustness gaps

A batch of small operational items: cleanup runs eight unbounded `UPDATE`s plus five batched deletes in one transaction (`internal/repository/cleanup.go:30-130`) — long lock hold times and WAL spikes after downtime; delete-worker finish-writes use the cancellable worker context instead of the codebase's established `WithoutCancel` pattern (`internal/app/workers.go:163-167, 229-235`), so a shutdown right after a successful Telegram delete loses the record write; webhook is registered before the HTTP listener accepts (`cmd/bot/main.go:178` vs `:219`); SIGTERM can exit non-zero after handlers are abandoned mid-flight (`cmd/bot/main.go:251-257` — 15 s shutdown budget vs ~2 m15 s worst-case handler), and a second signal does not force-exit; migration timeout is a hardcoded 2 minutes; tick-based worker constructors don't validate intervals (panic on 0).

### 21. Telegram client resilience: 429 compliance, error predicates, download integrity

Three client-level items: (a) the poller ignores `APIError.RetryAfter()` and caps backoff at 30 s (`internal/app/poller.go:66-71`) — under a flood limit Telegram may demand longer, and hammering at 30 s intervals can extend the limit; (b) the "4xx except 429 = permanent" heuristic is re-implemented with drift in three packages (`internal/app/workers.go:238-240`, `internal/bot/callback.go:104`, `internal/bot/publish.go:83-92`) — the client should own canonical `Permanent()`/`RateLimited()` predicates; (c) `DownloadFile` never compares received bytes to `getFile`'s declared `file_size` (`internal/telegram/client.go:322-355`) — a cleanly-closed truncated body is silently accepted, encrypted, and later delivered truncated. AES-GCM protects stored ciphertext, not input completeness.

### 22. Compose/deployment hardening and no backup story

The build spec required `security_opt: [no-new-privileges:true]` on both services (`docs/telegram-media-whisper-v1.md:570-598`); the shipped `compose.yaml` omits it. Also missing: memory/CPU limits on either service (the bot buffers 20 MiB plaintext per download), log rotation (`json-file` unbounded, with `LOG_LEVEL=debug` as the example default), file-based `secrets:` instead of plain `env_file` (bot token, AES key, and DB password are all visible in `docker inspect`), a `shm_size` for PostgreSQL, and any backup/restore mechanism for the volume that holds the only encrypted copies. Add an optional `backup` compose profile (pg_dump cron) plus the runbook from item 24.

### 23. No LICENSE despite a public GHCR image

The repo publishes a publicly consumable image and invites cloning, but contains no `LICENSE` — legally "all rights reserved", so nobody may actually use or redistribute the published image. Also missing for a secret-handling project: `SECURITY.md` (vulnerability disclosure policy), `CONTRIBUTING.md`, `CODEOWNERS`, and any changelog. LICENSE is the blocker; the rest are cheap follow-ups.

### 24. Spec drift unannotated; operational runbooks missing

`docs/telegram-media-whisper-v1.md` is presented as "the requirements the code implements" (`docs/README.md:9`) but materially contradicts the implementation with no supersession notes: the spec never persists media bytes while the implementation downloads and encrypts them; `DEFAULT_ONE_TIME=false` vs `true`; `MAX_ACTIVE_DRAFTS_PER_USER=3` vs `1`; different Postgres image/version and user; different env var names and Makefile targets. A new reviewer cannot tell which document is authoritative. Add a superseded banner plus inline drift notes. Second half: the operational runbooks the architecture doc itself admits are missing (architecture.md:323-330) — backup/restore, `MEDIA_ENCRYPTION_KEY` rotation (the keyring supports it, see item 13), bot-token/DB-compromise incident response, and migration rollback.

### 25. Crypto and memory hygiene details

Small but real hygiene gaps for a secrets product: the multipart upload duplicates decrypted plaintext into a `bytes.Buffer` that is never zeroed (`internal/telegram/media.go:250-296`), unlike the disciplined zeroing elsewhere — stream the body or back it with a zeroable buffer; live callback/open tokens travel in plain `string` fields on `service.Publication`/`CreatedWhisper` (`internal/service/publish.go:19`, `internal/service/ingest.go:21`) with no redacting wrapper — one accidental `slog` of those structs leaks a usable open token; the AEAD namespace literal is `secretsantabot`, a rename leftover frozen into all stored ciphertext's associated data (`internal/secretcrypto/cipher.go:47`) — must never change, so document it; config accepts all-zero/low-entropy encryption keys and webhook secrets (`internal/config/config.go:371-373, 295-302`); and there is no nonce-usage counter against the 2^32 random-nonce GCM ceiling. Also worth folding in: rejected-4xx-classification for `permanentDeleteError` currently closes jobs on any 400 (`internal/app/workers.go:238-241`).

---

## Deliberately not in the top 25

- Duplicate ephemeral delivery / duplicate envelope publication from cross-system ambiguity — inherent to at-least-once across Telegram + PostgreSQL, documented, and already minimized by lease sizing; further work belongs with item 19 (observability to detect it).
- Duplicated delete-worker code, fat handler interfaces, dead `edited_message`/`GetChatMember`/`inline_message_id` code, GORM-adjacent naming — cleanup, scheduled after the items above.
- Broad unit-test coverage gaps (processor failure paths, worker branch matrix, guest service layer) — best addressed incrementally alongside each fix above rather than as a standalone effort.
