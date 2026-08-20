package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadFromLookupUsesSecureDefaultsAndNormalizesLists(t *testing.T) {
	env := validEnvironment()
	env["TELEGRAM_BOT_USERNAME"] = "@SecretSantaBot"
	env["OWNER_TELEGRAM_IDS"] = "1001, 1002,1001"
	env["ALLOWED_CHAT_IDS"] = "-100123,42,-100123"

	cfg, err := LoadFromLookup(mapLookup(env))
	if err != nil {
		t.Fatalf("LoadFromLookup() error = %v", err)
	}

	if cfg.Telegram.UpdateMode != UpdateModePolling {
		t.Fatalf("UpdateMode = %q, want %q", cfg.Telegram.UpdateMode, UpdateModePolling)
	}
	if cfg.Telegram.WebhookMaxConnections != 4 {
		t.Fatalf("WebhookMaxConnections = %d, want 4", cfg.Telegram.WebhookMaxConnections)
	}
	if cfg.Telegram.BotUsername != "SecretSantaBot" {
		t.Fatalf("BotUsername = %q, want username without @", cfg.Telegram.BotUsername)
	}
	if cfg.Whisper.DraftTTL != 10*time.Minute {
		t.Fatalf("DraftTTL = %s, want 10m", cfg.Whisper.DraftTTL)
	}
	if cfg.Whisper.DefaultTTL != 24*time.Hour {
		t.Fatalf("DefaultTTL = %s, want 24h", cfg.Whisper.DefaultTTL)
	}
	if !cfg.Whisper.DefaultOneTime {
		t.Fatal("DefaultOneTime = false, want true")
	}
	if cfg.Whisper.PublishInterval != 2*time.Second {
		t.Fatalf("PublishInterval = %s, want 2s", cfg.Whisper.PublishInterval)
	}
	if cfg.Whisper.EphemeralDeleteAfter != 0 || cfg.Whisper.EphemeralDeleteInterval != 2*time.Second {
		t.Fatalf("ephemeral delete defaults = %s/%s, want 0s/2s", cfg.Whisper.EphemeralDeleteAfter, cfg.Whisper.EphemeralDeleteInterval)
	}
	if cfg.Media.Retention != 30*24*time.Hour {
		t.Fatalf("Media.Retention = %s, want 720h", cfg.Media.Retention)
	}
	if cfg.Media.StorageMode != "postgres" {
		t.Fatalf("Media.StorageMode = %q, want postgres", cfg.Media.StorageMode)
	}
	if cfg.Cleanup.ObservedIdentityRetention != 90*24*time.Hour {
		t.Fatalf("Cleanup.ObservedIdentityRetention = %s, want 2160h", cfg.Cleanup.ObservedIdentityRetention)
	}
	if len(cfg.Media.EncryptionKey) != 32 {
		t.Fatalf("EncryptionKey length = %d, want 32", len(cfg.Media.EncryptionKey))
	}
	if got := cfg.Telegram.OwnerIDs; len(got) != 2 || got[0] != 1001 || got[1] != 1002 {
		t.Fatalf("OwnerIDs = %v, want [1001 1002]", got)
	}
	if got := cfg.Whisper.AllowedChatIDs; len(got) != 2 || got[0] != -100123 || got[1] != 42 {
		t.Fatalf("AllowedChatIDs = %v, want [-100123 42]", got)
	}
	if !cfg.IsOwner(1002) || cfg.IsOwner(9999) {
		t.Fatal("IsOwner returned an unexpected result")
	}
	if !cfg.IsChatAllowed(-100123) || cfg.IsChatAllowed(-999) {
		t.Fatal("IsChatAllowed returned an unexpected result for a configured allowlist")
	}
	if got := cfg.Media.EncryptionKey.String(); got != "[REDACTED]" {
		t.Fatalf("EncryptionKey.String() = %q, want redaction", got)
	}
}

func TestEmptyChatAllowlistAllowsEveryChat(t *testing.T) {
	cfg, err := LoadFromLookup(mapLookup(validEnvironment()))
	if err != nil {
		t.Fatalf("LoadFromLookup() error = %v", err)
	}
	if !cfg.IsChatAllowed(-100123) || !cfg.IsChatAllowed(42) {
		t.Fatal("empty allowlist should allow every nonzero chat")
	}
}

func TestLoadFromLookupWebhook(t *testing.T) {
	env := validEnvironment()
	env["TELEGRAM_UPDATE_MODE"] = "WEBHOOK"
	env["TELEGRAM_WEBHOOK_PUBLIC_URL"] = "https://bot.example.com/telegram/webhook"
	env["TELEGRAM_WEBHOOK_SECRET"] = "abcdef0123456789abcdef0123456789"
	env["TELEGRAM_WEBHOOK_MAX_CONNECTIONS"] = "7"
	env["HTTP_WRITE_TIMEOUT"] = "3m"

	cfg, err := LoadFromLookup(mapLookup(env))
	if err != nil {
		t.Fatalf("LoadFromLookup() error = %v", err)
	}
	if cfg.Telegram.UpdateMode != UpdateModeWebhook {
		t.Fatalf("UpdateMode = %q, want webhook", cfg.Telegram.UpdateMode)
	}
	if cfg.Telegram.WebhookMaxConnections != 7 {
		t.Fatalf("WebhookMaxConnections = %d, want 7", cfg.Telegram.WebhookMaxConnections)
	}
}

func variedTestKey() []byte {
	key := make([]byte, 32)
	for index := range key {
		key[index] = byte(index*7 + 3)
	}
	return key
}

func TestLoadFromLookupRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name        string
		change      func(map[string]string)
		wantErrPart string
	}{
		{
			name: "unknown update mode",
			change: func(env map[string]string) {
				env["TELEGRAM_UPDATE_MODE"] = "socket"
			},
			wantErrPart: "TELEGRAM_UPDATE_MODE",
		},
		{
			name: "environment typo does not allow insecure Telegram API",
			change: func(env map[string]string) {
				env["APP_ENV"] = "prodution"
				env["TELEGRAM_API_BASE_URL"] = "http://api.example.test"
			},
			wantErrPart: "TELEGRAM_API_BASE_URL",
		},
		{
			name: "webhook requires HTTPS URL",
			change: func(env map[string]string) {
				env["TELEGRAM_UPDATE_MODE"] = "webhook"
				env["TELEGRAM_WEBHOOK_PUBLIC_URL"] = "http://bot.example.com/hook"
				env["TELEGRAM_WEBHOOK_SECRET"] = "abcdef0123456789abcdef0123456789"
			},
			wantErrPart: "TELEGRAM_WEBHOOK_PUBLIC_URL",
		},
		{
			name: "webhook URL must use the served path",
			change: func(env map[string]string) {
				env["TELEGRAM_UPDATE_MODE"] = "webhook"
				env["TELEGRAM_WEBHOOK_PUBLIC_URL"] = "https://bot.example.com/hook"
				env["TELEGRAM_WEBHOOK_SECRET"] = "abcdef0123456789abcdef0123456789"
				env["HTTP_WRITE_TIMEOUT"] = "3m"
			},
			wantErrPart: "TELEGRAM_WEBHOOK_PUBLIC_URL",
		},
		{
			name: "webhook write timeout covers media processing",
			change: func(env map[string]string) {
				env["TELEGRAM_UPDATE_MODE"] = "webhook"
				env["TELEGRAM_WEBHOOK_PUBLIC_URL"] = "https://bot.example.com/telegram/webhook"
				env["TELEGRAM_WEBHOOK_SECRET"] = "abcdef0123456789abcdef0123456789"
			},
			wantErrPart: "HTTP_WRITE_TIMEOUT",
		},
		{
			name: "webhook connections cannot be zero",
			change: func(env map[string]string) {
				env["TELEGRAM_WEBHOOK_MAX_CONNECTIONS"] = "0"
			},
			wantErrPart: "TELEGRAM_WEBHOOK_MAX_CONNECTIONS",
		},
		{
			name: "webhook connections cannot exceed Telegram limit",
			change: func(env map[string]string) {
				env["TELEGRAM_WEBHOOK_MAX_CONNECTIONS"] = "101"
			},
			wantErrPart: "TELEGRAM_WEBHOOK_MAX_CONNECTIONS",
		},
		{
			name: "missing owner",
			change: func(env map[string]string) {
				env["OWNER_TELEGRAM_IDS"] = ""
			},
			wantErrPart: "OWNER_TELEGRAM_IDS",
		},
		{
			name: "invalid chat ID",
			change: func(env map[string]string) {
				env["ALLOWED_CHAT_IDS"] = "not-an-id"
			},
			wantErrPart: "ALLOWED_CHAT_IDS",
		},
		{
			name: "default TTL exceeds max",
			change: func(env map[string]string) {
				env["DEFAULT_WHISPER_TTL"] = "192h"
			},
			wantErrPart: "DEFAULT_WHISPER_TTL",
		},
		{
			name: "multiple drafts unsupported in V1",
			change: func(env map[string]string) {
				env["MAX_ACTIVE_DRAFTS_PER_USER"] = "2"
			},
			wantErrPart: "MAX_ACTIVE_DRAFTS_PER_USER",
		},
		{
			name: "invalid database pool bounds",
			change: func(env map[string]string) {
				env["DB_MIN_CONNS"] = "11"
			},
			wantErrPart: "DB pool",
		},
		{
			name: "media exceeds Telegram download cap",
			change: func(env map[string]string) {
				env["MAX_MEDIA_BYTES"] = "20971521"
			},
			wantErrPart: "MAX_MEDIA_BYTES",
		},
		{
			name: "short encryption key",
			change: func(env map[string]string) {
				env["MEDIA_ENCRYPTION_KEY"] = base64.StdEncoding.EncodeToString(make([]byte, 16))
			},
			wantErrPart: "MEDIA_ENCRYPTION_KEY",
		},
		{
			name: "disabled publication worker",
			change: func(env map[string]string) {
				env["PUBLISH_INTERVAL"] = "0s"
			},
			wantErrPart: "config publication",
		},
		{
			name: "publication lease must outlive API request",
			change: func(env map[string]string) {
				env["PUBLISH_LEASE_TIMEOUT"] = "19s"
			},
			wantErrPart: "PUBLISH_LEASE_TIMEOUT",
		},
		{
			name: "negative ephemeral deletion",
			change: func(env map[string]string) {
				env["EPHEMERAL_DELETE_AFTER"] = "-5s"
			},
			wantErrPart: "ephemeral deletion",
		},
		{
			name: "invalid duration",
			change: func(env map[string]string) {
				env["CLEANUP_INTERVAL"] = "often"
			},
			wantErrPart: "CLEANUP_INTERVAL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnvironment()
			tt.change(env)
			_, err := LoadFromLookup(mapLookup(env))
			if err == nil || !strings.Contains(err.Error(), tt.wantErrPart) {
				t.Fatalf("LoadFromLookup() error = %v, want error containing %q", err, tt.wantErrPart)
			}
		})
	}
}

func validEnvironment() map[string]string {
	return map[string]string{
		"TELEGRAM_BOT_TOKEN":      "123456:real-looking-test-token",
		"TELEGRAM_BOT_USERNAME":   "secret_santa_bot",
		"OWNER_TELEGRAM_IDS":      "1001",
		"DATABASE_URL":            "postgres://secretmediabot:secret@postgres:5432/secretmediabot?sslmode=disable",
		"MEDIA_ENCRYPTION_KEY":    base64.StdEncoding.EncodeToString(variedTestKey()),
		"MEDIA_ENCRYPTION_KEY_ID": "test-v1",
	}
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestProductionRequiresExplicitChatScope(t *testing.T) {
	t.Parallel()

	env := validEnvironment()
	env["APP_ENV"] = "production"
	env["DATABASE_URL"] = "postgres://user:pass@db.example.com:5432/bot?sslmode=require"
	if _, err := LoadFromLookup(mapLookup(env)); err == nil ||
		!strings.Contains(err.Error(), "ALLOWED_CHAT_IDS") {
		t.Fatalf("production with empty allowlist should fail, got %v", err)
	}
	env["ALLOW_ALL_CHATS"] = "true"
	cfg, err := LoadFromLookup(mapLookup(env))
	if err != nil {
		t.Fatalf("explicit opt-in should pass: %v", err)
	}
	if !cfg.Whisper.AllowAllChats {
		t.Error("AllowAllChats not loaded")
	}
}

func TestUnknownEnvironmentFailsClosedToProductionPosture(t *testing.T) {
	t.Parallel()

	env := validEnvironment()
	env["APP_ENV"] = "produciton"
	env["ALLOW_ALL_CHATS"] = "true"
	env["DATABASE_URL"] = "postgres://user:pass@db.example.com:5432/bot?sslmode=require"
	cfg, err := LoadFromLookup(mapLookup(env))
	if err != nil {
		t.Fatalf("unknown environment should load with explicit secure settings: %v", err)
	}
	if !cfg.ProductionLike() {
		t.Fatal("unknown environment relaxed production posture")
	}
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "strict production posture is applied") {
		t.Fatalf("warnings = %v, want strict posture warning", cfg.Warnings)
	}
}

func TestProductionRejectsPlaintextDatabaseTransport(t *testing.T) {
	t.Parallel()

	env := validEnvironment()
	env["APP_ENV"] = "production"
	env["ALLOW_ALL_CHATS"] = "true"
	env["DATABASE_URL"] = "postgres://user:pass@db.example.com:5432/bot?sslmode=disable"
	if _, err := LoadFromLookup(mapLookup(env)); err == nil ||
		!strings.Contains(err.Error(), "sslmode") {
		t.Fatalf("production sslmode=disable should fail, got %v", err)
	}
	env["DATABASE_URL"] = "postgres://user:pass@db.example.com:5432/bot?sslmode=require"
	if _, err := LoadFromLookup(mapLookup(env)); err != nil {
		t.Fatalf("sslmode=require should pass: %v", err)
	}
}

func TestWeakSecretsAreRejected(t *testing.T) {
	t.Parallel()

	env := validEnvironment()
	env["TELEGRAM_UPDATE_MODE"] = "webhook"
	env["TELEGRAM_WEBHOOK_PUBLIC_URL"] = "https://bot.example.com/telegram/webhook"
	env["TELEGRAM_WEBHOOK_SECRET"] = strings.Repeat("x", 40)
	if _, err := LoadFromLookup(mapLookup(env)); err == nil ||
		!strings.Contains(err.Error(), "repeated") {
		t.Fatalf("repeated webhook secret should fail, got %v", err)
	}

	env = validEnvironment()
	env["MEDIA_ENCRYPTION_KEY"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xA5}, 32))
	if _, err := LoadFromLookup(mapLookup(env)); err == nil ||
		!strings.Contains(err.Error(), "non-random") {
		t.Fatalf("repeated-byte key should fail, got %v", err)
	}
}

func TestFileBackedSecretLookup(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("110201543:atokenbehindafile\n"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	env := validEnvironment()
	delete(env, "TELEGRAM_BOT_TOKEN")
	env["TELEGRAM_BOT_TOKEN_FILE"] = path
	cfg, err := LoadFromLookup(func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	})
	// The injectable lookup path cannot see _FILE vars; only Load can. This
	// documents that file support lives in Load, not LoadFromLookup.
	if err == nil && cfg.Telegram.BotToken == "" {
		t.Fatal("expected lookup-based load to lack the token")
	}
}

func TestWarningsSurfaceOperationalRisks(t *testing.T) {
	t.Parallel()

	env := validEnvironment()
	env["DATABASE_URL"] = "postgres://user:pass@127.0.0.1:5432/bot?sslmode=disable"
	env["ALLOW_ALL_CHATS"] = "true"
	cfg, err := LoadFromLookup(mapLookup(env))
	if err != nil {
		t.Fatalf("LoadFromLookup() error = %v", err)
	}
	joined := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(joined, "sslmode=disable") {
		t.Errorf("warnings missing sslmode note: %v", cfg.Warnings)
	}
	if !strings.Contains(joined, "ALLOW_ALL_CHATS") {
		t.Errorf("warnings missing allow-all note: %v", cfg.Warnings)
	}
}

func TestMediaEncryptionPreviousKeys(t *testing.T) {
	t.Parallel()

	prevKey := make([]byte, 32)
	for i := range prevKey {
		prevKey[i] = byte(i*13 + 7)
	}
	prevKeyB64 := base64.StdEncoding.EncodeToString(prevKey)

	env := validEnvironment()
	env["MEDIA_ENCRYPTION_KEY_ID"] = "v2"
	env["MEDIA_ENCRYPTION_PREVIOUS_KEYS"] = "v1:" + prevKeyB64

	cfg, err := LoadFromLookup(mapLookup(env))
	if err != nil {
		t.Fatalf("LoadFromLookup() with previous keys error = %v", err)
	}
	if len(cfg.Media.PreviousKeys) != 1 {
		t.Fatalf("PreviousKeys count = %d, want 1", len(cfg.Media.PreviousKeys))
	}
	if got := cfg.Media.PreviousKeys["v1"]; len(got) != 32 {
		t.Fatalf("Previous key v1 length = %d, want 32", len(got))
	}

	// Reject previous key using active key ID
	invalidEnv := validEnvironment()
	invalidEnv["MEDIA_ENCRYPTION_KEY_ID"] = "v1"
	invalidEnv["MEDIA_ENCRYPTION_PREVIOUS_KEYS"] = "v1:" + prevKeyB64
	if _, err := LoadFromLookup(mapLookup(invalidEnv)); err == nil || !strings.Contains(err.Error(), "active key ID") {
		t.Fatalf("expected error for duplicate active key ID in previous keys, got %v", err)
	}

	// Reject invalid format
	badFormatEnv := validEnvironment()
	badFormatEnv["MEDIA_ENCRYPTION_PREVIOUS_KEYS"] = "v1"
	if _, err := LoadFromLookup(mapLookup(badFormatEnv)); err == nil {
		t.Fatal("expected error for invalid previous key format")
	}
}
