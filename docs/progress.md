# Implementation progress

Working notes against [docs/improvements.md](./improvements.md). Status per item as of the most recent session. All unit tests pass (`go test ./...`), `go build ./...` and `go vet ./...` are green.

Legend: DONE · DONE (partial) = finished with listed gaps · PLANNED = designed but not started · NOT STARTED

## P0 — critical

### 1. Poison updates — DONE
- `recover()` in `internal/app/processor.go` (`errPanicRecovered`, `FailUpdate("panic_recovered")`); dead-lettered updates (`repository.ErrUpdateDead` → processor returns nil so the offset advances).
- Per-update `getUpdates` decode (`[]json.RawMessage`, shell updates for malformed JSON).
- Max-attempts budget `ClaimUpdateParams.MaxAttempts` (default 5) via `defaultMaxUpdateAttempts`.
- Tests: processor panic recovery, dead-letter, client quarantine.

### 2. Live Telegram E2E validation — PLANNED
Cannot be executed in this environment. Plan: ship `docs/live-validation.md` checklist. Not yet written.

### 3. Guest/inline rate limiting — DONE (partial)
- Repository `CreateGuestRequest`: sender-scoped advisory lock (`guestAdvisoryLockNamespace`, `917_501_000_000_000`), same-target `awaiting_secret` reuse (inline keystrokes don't create DB rows), supersede different-target awaiting, active (default 1) and hourly (default 6) caps.
- Service quotas + new errors `ErrGuestActiveLimit`, `ErrGuestRateLimit`, `ErrGuestOpeningInProgress`.
- Inline `CacheTime: 300` + stable sha256 result IDs; user-facing rate-limit messages.
- HALF DONE: denial-event audit cap — the open-event denial cap (1 per whisper+user per minute) is now implemented in `recordDeniedOpen` (was listed under 3 in the backlog, see 16).

### 4. Guest username hijack/lockout — DONE
- Username gates only the first claim; once `target_user_id` is bound it is authoritative (no post-claim re-verification).
- Self-username targeting rejected; volatility warning in inline article description.

## P1 — high

### 5. Redirect policy — DONE
- `http.ErrUseLastResponse`; enforced even on injected clients (shallow `http.Client` copy).
- Client tests cover redirect rejection.

### 6. Terminal publication failure — DONE (partial)
- Honest finalize-time messages: `"Secret stored securely. The group envelope will be posted shortly."` / `"...could not be queued. It will not be delivered."`.
- `publicationErrorCode`/`publicationRetryDelay` classify terminal vs retryable.
- HALF DONE: notify the sender in private chat on a terminal publication failure is still NOT implemented (publication worker path). Next step.

### 7. Healthcheck — DONE (partial)
- `healthcheck()` in `cmd/bot/main.go` parses `HTTP_ADDR` (via `net.SplitHostPort`) and probes `/healthz`.
- HALF DONE: `Dockerfile` has no `HEALTHCHECK` with migration-aware start-period yet.

### 8. CI rework (scan-before-push, supply chain) — NOT STARTED
- `.github/workflows/ci.yml` untouched. Planned: scan-before-push (trivy/govulncheck gates image push), SHA-pinned actions, least-privilege permissions, `dependabot.yml`. SHA-pinning needs network/version lookup.

### 9. Integration suite in CI — NOT STARTED
- Planned: CI job with postgres service + `TEST_DATABASE_URL` gating `internal/repository/postgres_integration_test.go`; `go mod verify` gate.

### 10. Fail-open posture — DONE
- Production requires non-empty chat allowlist (`ALLOW_ALL_CHATS` default false → fails closed). `GUEST_MODE_ENABLED` (default true) service `Options.GuestModeEnabled`.
- `Config.Warnings` surfaced at startup; config tests updated.

### 11. `upsertUser` clobber — DONE
- `COALESCE(NULLIF(EXCLUDED.x,''), users.x)` in `internal/repository/identity.go`. Kept over `DO NOTHING` because guest numeric targets need the FK user row.

### 12. DB TLS — DONE
- `validateDatabaseURL(raw, productionLike)` rejects `sslmode=disable/allow` under `APP_ENV=production`; non-production warning. `.env.example` still needs an `sslmode` comment.

## P2 — medium

### 13. Key rotation — NOT STARTED
- Planned: `MEDIA_ENCRYPTION_PREVIOUS_KEYS` (or similar) config, keyring multi-key decrypt, key-rotation runbook. `internal/secretcrypto` and config not yet touched.

### 14. Unbounded tables — DONE (partial)
- `internal/repository/cleanup.go` fully rewritten: bounded `WITH doomed … FOR UPDATE SKIP LOCKED LIMIT` sweeps; terminal-draft deletes; state-agnostic `processed_updates` prune; identity prune with FK reference guards; gave-up delete-job closure (`deleteJobMaxAttempts = 30`).
- `CleanupResult`/`CleanupParams` extended (`IdentityRetention`, `DeletedDrafts/Members/Users/Chats`, `ExpiredDraftSenderIDs`, `ExpiredGuestSenderIDs`).
- HALF DONE: `OBSERVED_IDENTITY_RETENTION` config not added; `NewCleanupWorker` in main.go not yet passed identity retention; `cleanupTotal` in `internal/app/workers.go` misses the new counters; expiry notifier not wired.

### 15. Missing indexes — NOT STARTED
- Migration 00005 planned: guest sweeps, guest delete-job due queue, drafts hourly-count, delete-job purges.

### 16. One-time guarantee after crash mid-delivery — DONE
- `ReserveOpen` refuses to re-reserve a one-time whisper when a lingering `delivery_state='reserved'` event exists (outcome unknowable → `ErrOpenAmbiguous` → `OpenDeniedAmbiguous` event, mapped to `ErrWhisperUnavailable`). Fails closed; whisper expires via normal TTL.
- Denial-event cap (1 per whisper+user per minute) added in `recordDeniedOpen` (closes the 3b gap).

### 17. Dead `file_id` retry loop — DONE
- `telegram.SendEphemeralMediaUpload` (multipart with `receiver_user_id` + `callback_query_id`), factored `buildMultipartBody` shared with `SendPrivateMedia`.
- `repository.FetchWhisperMedia` (+ `WhisperMediaBlob`, ungated — callers already hold a reserved open), `service.WhisperMediaFallback`, `bot.sendDeliveryFallback` on `telegram.IsPermanent(err)`.
- Multipart buffers zeroed after the request (`clear(payload)` + `Reset`), closing the media.go half of item 25.

### 18. Guest flow correctness / dead-end batch — DONE (partial)
- (a) `CancelGuestRequest` returns count; (b) opening-state guidance both roles + in-place lease takeover; (c) nil-lease deref guard moved before decrypt; (d) `HasActiveDraft` composer precedence; (e) HALF DONE: expiry notifications for abandoned guest requests not wired (needs the cleanup-worker notifier from 14).

### 19. Metrics — DONE (partial)
- Dependency-free `internal/metrics` package (Prometheus text format; `Counter`/`Gauge` with fixed label sets), instrumentation in `internal/telegram/client.go` and `internal/app` (processor, poller, workers).
- HALF DONE: `/metrics` endpoint not wired into `internal/httpserver`.

### 20. Worker/shutdown robustness — DONE (partial)
- `internal/app/workers.go` rewritten: validated constructors (`NewEphemeralDeleteWorker`/`NewGuestPrivateDeleteWorker` → `(*X, error)`), `context.WithoutCancel` finish-writes, shared `drainDeleteJobs`, gave-up cap, `permanentDeleteClose` (narrow 403/404/"message to delete not found").
- main.go: second-signal force exit (`os.Exit(130)`), 10-min migration timeout, runner names.
- HALF DONE: webhook is still configured before the HTTP listener accepts (`configureTelegramTransport` ordering not yet moved).

### 21. Client resilience — DONE (partial)
- `RetryAfter` honored in poller; `APIError.Permanent()/RateLimited()`; jitter injectable; `DownloadFile(ctx, path, expectedSize)` + `ErrIncompleteDownload`.
- HALF DONE: `internal/bot/publish.go` (~83–92) still re-implements the 4xx-permanent heuristic with drift — switch to `telegram.IsPermanent/IsRateLimited`. (`internal/bot/callback.go:104` already converted this session.)

### 22. Compose/deployment hardening — NOT STARTED
- `compose.yaml`/`Dockerfile` untouched. Planned: security_opt, resource limits, log rotation, shm_size, secrets via `*_FILE`, backup profile.

### 23. LICENSE/security docs — NOT STARTED
- No LICENSE. MIT assumed but UNCONFIRMED by the user. Planned: LICENSE, SECURITY.md, CONTRIBUTING.md, CODEOWNERS.

### 24. Spec drift / runbooks — NOT STARTED
- Planned: drift banner in `docs/telegram-media-whisper-v1.md` + `docs/README.md`; operational runbooks (backup/restore, key rotation, incident response).

### 25. Crypto/memory hygiene — DONE (partial)
- Config rejects low-entropy `MEDIA_ENCRYPTION_KEY` and repeated-char webhook secret.
- Multipart buffers zeroed (this session).
- HALF DONE: redacting token strings in `service.Publication.CallbackData`/`CreatedWhisper`; AEAD namespace doc; nonce-counter note.

## User requirements (not in the 25)

- Inline minimal-click `@bot @target_username` flow matches the existing guest design — kept consistent (stable result IDs, cache 300).
- Do NOT advertise the owner's ability to view/access all media in docs — NOT STARTED (README/docs scrub pending).
- `/commands` in README and in the bot menu — `setMyCommands` is already wired at startup (scope: private vs group not yet verified). README commands table NOT STARTED.

## In-flight / next steps

1. Item 6: sender notification on terminal publication failure (`internal/bot/publish.go`, `internal/app/publication_worker.go`); also convert the remaining 4xx-permanent heuristic in `publish.go`.
2. Item 14 wiring: `OBSERVED_IDENTITY_RETENTION` config, `NewCleanupWorker` (identity retention + expiry notifier), update `cleanupTotal`, update main.go call site.
3. Item 19: `/metrics` HTTP handler in `internal/httpserver`.
4. Item 13: key-rotation config + keyring multi-key test.
5. Item 15: migration 00005 (indexes).
6. Item 20: webhook-after-listener ordering.
7. Items 7/8/9/22/23: Dockerfile HEALTHCHECK, CI rework + integration job, compose hardening, LICENSE/security docs.
8. Item 25 residue + docs (2/24) + user-facing owner-mention scrub + README commands + bot-menu scope.