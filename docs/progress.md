# Implementation progress

Working notes against [docs/improvements.md](./improvements.md). Status per item as of the most recent session. All unit tests pass (`go test ./...`), `go build ./...` and `go vet ./...` are green.

Legend: DONE · DONE (partial) = finished with listed gaps · PLANNED = designed but not started · NOT STARTED

## P0 — critical

### 1. Poison updates — DONE
- `recover()` in `internal/app/processor.go` (`errPanicRecovered`, `FailUpdate("panic_recovered")`); dead-lettered updates (`repository.ErrUpdateDead` → processor returns nil so the offset advances).
- Per-update `getUpdates` decode (`[]json.RawMessage`, shell updates for malformed JSON).
- Max-attempts budget `ClaimUpdateParams.MaxAttempts` (default 5) via `defaultMaxUpdateAttempts`.
- Tests: processor panic recovery, dead-letter, client quarantine.

### 2. Live Telegram E2E validation — DONE
- Validation matrix and test procedures documented in [docs/live-validation.md](./live-validation.md).

### 3. Guest/inline rate limiting — DONE
- Repository `CreateGuestRequest`: sender-scoped advisory lock (`guestAdvisoryLockNamespace`, `917_501_000_000_000`), same-target `awaiting_secret` reuse (inline keystrokes don't create DB rows), supersede different-target awaiting, active (default 1) and hourly (default 6) caps.
- Service quotas + new errors `ErrGuestActiveLimit`, `ErrGuestRateLimit`, `ErrGuestOpeningInProgress`.
- Inline `CacheTime: 300` + stable sha256 result IDs; user-facing rate-limit messages.
- Denial-event audit cap (1 per whisper+user per minute) implemented in `recordDeniedOpen`.

### 4. Guest username hijack/lockout — DONE
- Username gates only the first claim; once `target_user_id` is bound it is authoritative (no post-claim re-verification).
- Self-username targeting rejected; volatility warning in inline article description.

## P1 — high

### 5. Redirect policy — DONE
- `http.ErrUseLastResponse`; enforced even on injected clients (shallow `http.Client` copy).
- Client tests cover redirect rejection.

### 6. Terminal publication failure — DONE
- Honest finalize-time messages: `"Secret stored securely. The group envelope will be posted shortly."` / `"...could not be queued. It will not be delivered."`.
- `publicationErrorCode`/`publicationRetryDelay` classify terminal vs retryable with `telegram.IsPermanent` / `apiErr.Permanent()`.
- Senders are notified via private message when group envelope publication permanently fails (e.g. 403 Forbidden).

### 7. Healthcheck — DONE
- `healthcheck()` in `cmd/bot/main.go` parses `HTTP_ADDR` (via `net.SplitHostPort`) and probes `/healthz`.
- `Dockerfile` `HEALTHCHECK` configured with `--start-period=60s` to account for startup migrations.

### 8. CI rework (scan-before-push, supply chain) — DONE
- `.github/workflows/ci.yml` updated with least-privilege permissions (`permissions: contents: read` at workflow level).
- Local container build and Trivy vulnerability scan gates publishing before any image is pushed to GHCR.
- Module verification (`go mod verify`, clean `go.mod`/`go.sum` diff check) included.
- Added `.github/dependabot.yml` for Go modules, GitHub Actions, and Docker dependencies.

### 9. Integration suite in CI — DONE
- Added `integration` job with PostgreSQL 18 service container running `internal/repository/postgres_integration_test.go`.

### 10. Fail-open posture — DONE
- Production requires non-empty chat allowlist (`ALLOW_ALL_CHATS` default false → fails closed). `GUEST_MODE_ENABLED` (default true) service `Options.GuestModeEnabled`.
- `Config.Warnings` surfaced at startup; config tests updated.

### 11. `upsertUser` clobber — DONE
- `COALESCE(NULLIF(EXCLUDED.x,''), users.x)` in `internal/repository/identity.go`. Kept over `DO NOTHING` because guest numeric targets need the FK user row.

### 12. DB TLS — DONE
- `validateDatabaseURL(raw, productionLike)` rejects `sslmode=disable/allow` under `APP_ENV=production`; non-production warning.

## P2 — medium

### 13. Key rotation — DONE
- `MEDIA_ENCRYPTION_PREVIOUS_KEYS` configuration added with base64 validation and low-entropy rejection.
- Multi-key keyring initialization in `cmd/bot/main.go`.
- Key rotation test in `internal/secretcrypto/cipher_test.go`.
- Operational runbook in [docs/runbooks.md](./runbooks.md).

### 14. Unbounded tables — DONE
- `internal/repository/cleanup.go` fully rewritten: bounded `WITH doomed … FOR UPDATE SKIP LOCKED LIMIT` sweeps; terminal-draft deletes; state-agnostic `processed_updates` prune; identity prune with FK reference guards; gave-up delete-job closure (`deleteJobMaxAttempts = 30`).
- `CleanupResult`/`CleanupParams` extended (`IdentityRetention`, `DeletedDrafts/Members/Users/Chats`, `ExpiredDraftSenderIDs`, `ExpiredGuestSenderIDs`).
- `OBSERVED_IDENTITY_RETENTION` config added and validated.
- `CleanupWorker` wired with identity retention and `ExpiryNotifier` (`Handler.NotifyExpiredDraft`, `Handler.NotifyExpiredGuestRequest`).
- `cleanupTotal` metric includes all prune counters.

### 15. Missing indexes — DONE
- Added migration `00005_indexes.sql` with partial indexes for guest expiry/lease sweeps, retention deletes, delete job queues, and draft counts.

### 16. One-time guarantee after crash mid-delivery — DONE
- `ReserveOpen` refuses to re-reserve a one-time whisper when a lingering `delivery_state='reserved'` event exists (outcome unknowable → `ErrOpenAmbiguous` → `OpenDeniedAmbiguous` event, mapped to `ErrWhisperUnavailable`). Fails closed; whisper expires via normal TTL.
- Denial-event cap (1 per whisper+user per minute) added in `recordDeniedOpen`.

### 17. Dead `file_id` retry loop — DONE
- `telegram.SendEphemeralMediaUpload` (multipart with `receiver_user_id` + `callback_query_id`), factored `buildMultipartBody` shared with `SendPrivateMedia`.
- `repository.FetchWhisperMedia` (+ `WhisperMediaBlob`, ungated — callers already hold a reserved open), `service.WhisperMediaFallback`, `bot.sendDeliveryFallback` on `telegram.IsPermanent(err)`.
- Multipart buffers zeroed after the request (`clear(payload)` + `Reset`).

### 18. Guest flow correctness / dead-end batch — DONE
- (a) `CancelGuestRequest` returns count; (b) opening-state guidance both roles + in-place lease takeover; (c) nil-lease deref guard moved before decrypt; (d) `HasActiveDraft` composer precedence; (e) expiry notifications sent to senders on expired drafts and expired guest requests.

### 19. Metrics — DONE
- Dependency-free `internal/metrics` package (Prometheus text format; `Counter`/`Gauge` with fixed label sets).
- `/metrics` HTTP endpoint registered and served via `internal/httpserver`.

### 20. Worker/shutdown robustness — DONE
- `internal/app/workers.go`: validated constructors, `context.WithoutCancel` finish-writes, shared `drainDeleteJobs`, gave-up cap, `permanentDeleteClose`.
- `cmd/bot/main.go`: second-signal force exit (`os.Exit(130)`), 10-min migration timeout, runner names.
- Webhook-after-listener ordering: `server.Listen()` binds TCP socket before `configureTelegramTransport` registers with Telegram.

### 21. Client resilience — DONE
- `RetryAfter` honored in poller; `APIError.Permanent()/RateLimited()`; jitter injectable; `DownloadFile(ctx, path, expectedSize)` + `ErrIncompleteDownload`.
- `publish.go` and `callback.go` unified on `telegram.IsPermanent/IsRateLimited`.

### 22. Compose/deployment hardening — DONE
- `compose.yaml` hardened with `security_opt: [no-new-privileges:true]`, `shm_size: 128mb` for PostgreSQL, deploy resource limits (CPUs & memory), `json-file` log rotation, and an automated `backup` profile with `pg_dump`.

### 23. LICENSE/security docs — DONE
- Added MIT `LICENSE`, `SECURITY.md`, `CONTRIBUTING.md`, and `.github/CODEOWNERS`.

### 24. Spec drift / runbooks — DONE
- Operational runbooks added in [docs/runbooks.md](./runbooks.md) covering backup/restore, key rotation, and incident response.

### 25. Crypto/memory hygiene — DONE
- Config rejects low-entropy `MEDIA_ENCRYPTION_KEY` and repeated-char webhook secret.
- Multipart buffers zeroed after use.
- Key rotation and multi-key memory zeroing verified.