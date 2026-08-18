# Live Telegram Validation Checklist

This checklist defines the manual validation suite to verify Secret Media Bot's core mechanics against live Telegram clients and the Telegram Bot API.

## Pre-Requisites

1. A test bot created via [@BotFather](https://t.me/botfather).
2. Bot configuration in BotFather:
   - **Inline Mode**: Enabled (`/setinline`).
   - **Group Privacy**: Disabled (`/setprivacy` → Disabled) or Bot promoted to administrator.
   - **Command list**: Configured (`/setcommands`).
3. Running bot instance connected to PostgreSQL with `APP_ENV=development` (or `test`).

---

## Validation Matrix

### 1. Group Whisper Flow

- [ ] **Reply Whisper**: Send `/whisper` as a reply to a member in a group.
  - Verification: Bot replies with inline button or instructions; private composer opens.
- [ ] **Username Whisper**: Send `/whisper @username` in a group.
  - Verification: Bot prompts sender in private chat.
- [ ] **Numeric ID Whisper**: Send `/whisper 123456789` in a group.
  - Verification: Bot resolves target user ID properly.
- [ ] **Text Secret**: Send text secret in private chat with bot.
  - Verification: Bot updates status; group receives envelope with "Open secret" button.
- [ ] **Media Secrets**: Test photo, voice note, video, audio, and document uploads.
  - Verification: Media is encrypted in DB; file ID or decrypted multipart upload is sent on open.
- [ ] **Authorized Open**: Recipient presses "Open secret".
  - Verification: Ephemeral message delivered; auto-delete scheduled; button status updated.
- [ ] **Unauthorized Open**: Another group member presses "Open secret".
  - Verification: Denied with alert popup; rate-limit prevents log/DB amplification.

### 2. Guest Mode & Inline Queries

- [ ] **Inline Locked Secret**: In any chat, type `@bot @target_username`.
  - Verification: Inline article appears with stable result ID; recipient must start bot or claim secret.
- [ ] **Guest Secret Composer**: Sender provides secret in private chat.
  - Verification: Target user receives private notification to claim and open.
- [ ] **Target ID Binding**: Once claimed by recipient numeric ID, target username change does not lock out legitimate recipient.

### 3. Expiry and Cleanup

- [ ] **Draft Expiry**: Leave a draft unfinalized past `DRAFT_TTL`.
  - Verification: Cleanup worker transitions state to `expired`; sender receives expiry notification.
- [ ] **Whisper Expiry**: Leave an envelope unopened past `DEFAULT_WHISPER_TTL`.
  - Verification: Cleanup transitions whisper to `expired`; opening attempt is refused.
- [ ] **Ephemeral Deletion**: Verify ephemeral message is deleted after configured `EPHEMERAL_DELETE_AFTER` window.

### 4. Failure Recovery & Edge Cases

- [ ] **Bot Kicked from Group**: When group envelope delivery fails permanently (e.g. 403 Forbidden).
  - Verification: Sender receives private notification that envelope could not be posted; whisper does not hang indefinitely.
- [ ] **Dead File ID Recovery**: Expired or rotated Telegram file ID triggers multipart upload fallback.
- [ ] **Key Rotation**: Rotate `MEDIA_ENCRYPTION_KEY_ID` with previous key in `MEDIA_ENCRYPTION_PREVIOUS_KEYS`.
  - Verification: Existing unopened secrets still decrypt and deliver successfully.
