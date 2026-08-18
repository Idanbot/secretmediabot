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
