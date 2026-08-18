# Operational Runbooks

## 1. Backup and Restore

### Automated Backup via Docker Compose
Run the backup profile to generate a timestamped compressed database dump:
```bash
docker compose --profile backup run --rm backup
```
Backups are saved to `./backups/secretmediabot-<timestamp>.sql.gz`.

### Manual Backup
```bash
PGPASSWORD="${POSTGRES_PASSWORD}" pg_dump \
  -h localhost -p 5432 -U "${POSTGRES_USER}" -d "${POSTGRES_DB}" | gzip > backup.sql.gz
```

### Restore Procedure
1. Stop the bot service to prevent concurrent writes:
   ```bash
   docker compose stop bot
   ```
2. Restore database from dump:
   ```bash
   gunzip -c backup.sql.gz | PGPASSWORD="${POSTGRES_PASSWORD}" psql \
     -h localhost -p 5432 -U "${POSTGRES_USER}" -d "${POSTGRES_DB}"
   ```
3. Start the bot service:
   ```bash
   docker compose start bot
   ```

---

## 2. Key Rotation Procedure

To rotate the AES-256-GCM media encryption key without invalidating existing stored secrets:

1. Generate a new 32-byte cryptographically secure key:
   ```bash
   openssl rand -base64 32
   ```
2. Update configuration:
   - Set `MEDIA_ENCRYPTION_KEY_ID=v2` (new active key ID).
   - Set `MEDIA_ENCRYPTION_KEY=<new-base64-key>`.
   - Add previous key to `MEDIA_ENCRYPTION_PREVIOUS_KEYS`:
     ```env
     MEDIA_ENCRYPTION_PREVIOUS_KEYS=v1:<old-base64-key>
     ```
3. Restart the bot service:
   ```bash
   docker compose restart bot
   ```
4. Verification:
   - Newly created secrets will be encrypted using `v2`.
   - Existing secrets created with `v1` will continue to decrypt seamlessly using the previous key mapping.
5. After the `MEDIA_RETENTION` window (e.g. 30 days) has passed and all `v1` secrets have expired and cleaned up, `v1` can be safely removed from `MEDIA_ENCRYPTION_PREVIOUS_KEYS`.

---

## 3. Incident Response

### Compromised Bot Token
1. Revoke the token immediately in [@BotFather](https://t.me/botfather) using `/revoke`.
2. Update `TELEGRAM_BOT_TOKEN` with the new token.
3. Restart the bot service.

### Database or Secret Compromise
1. Rotate `MEDIA_ENCRYPTION_KEY` following the Key Rotation Procedure above.
2. Invalidate all active drafts and pending requests via database cleanup or operator intervention.
3. Check `owner_audit_events` and database logs for unauthorized access.

---

## 4. Observability & Monitoring

Secret Media Bot includes a pre-configured Prometheus and Grafana stack in `compose.yaml`:

### Starting the Monitoring Stack
```bash
docker compose --profile monitoring up -d
```

### Accessing Dashboards
- **Grafana**: [http://localhost:3000](http://localhost:3000) (default credentials: `admin` / `admin`).
  - Pre-provisioned dashboard: **Secret Media Bot Overview** (tracks update rates, API latency, delete job lag, and errors).
- **Prometheus**: [http://localhost:9090](http://localhost:9090).
- **Bot Raw Metrics**: `curl http://localhost:8080/metrics`

### Metric Reference
| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `updates_processed_total` | Counter | `kind` | Acknowledged Telegram updates by type (`message`, `callback_query`, etc.). |
| `updates_failed_total` | Counter | `error_class` | Processing failures by error classification. |
| `updates_dead_letter_total` | Counter | None | Updates skipped after exceeding retry budget. |
| `update_handler_panics_total` | Counter | None | Recovered panics in update handlers. |
| `telegram_api_requests_total` | Counter | `method`, `outcome` | Outgoing Telegram API calls and outcomes. |
| `telegram_api_request_duration_microseconds_total` | Counter | `method` | Cumulative duration of Telegram API calls. |
| `delete_jobs_total` | Counter | `queue`, `outcome` | Ephemeral message deletion job results. |
| `telegram_poll_failures_total` | Counter | `error_class` | Failed `getUpdates` polling requests. |
