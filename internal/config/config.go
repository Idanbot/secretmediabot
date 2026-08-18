// Package config loads and validates the bot's environment-based configuration.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// UpdateMode controls how Telegram updates reach the bot.
type UpdateMode string

const (
	UpdateModePolling UpdateMode = "polling"
	UpdateModeWebhook UpdateMode = "webhook"

	mediaStoragePostgres      = "postgres"
	telegramWebhookPath       = "/telegram/webhook"
	publicationLeaseFinishGap = 5 * time.Second
)

// SecretBytes redacts key material when a configuration value is formatted.
type SecretBytes []byte

func (SecretBytes) String() string   { return "[REDACTED]" }
func (SecretBytes) GoString() string { return "[REDACTED]" }

type Config struct {
	AppEnv   string
	LogLevel string
	// Warnings carries non-fatal operational warnings the caller should log.
	Warnings []string

	HTTP     HTTPConfig
	Telegram TelegramConfig
	Database DatabaseConfig
	Whisper  WhisperConfig
	Media    MediaConfig
	Cleanup  CleanupConfig
}

type HTTPConfig struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

type TelegramConfig struct {
	BotToken              string
	BotUsername           string
	APIBaseURL            string
	UpdateMode            UpdateMode
	WebhookPublicURL      string
	WebhookSecret         string
	WebhookMaxConnections int
	PollTimeout           time.Duration
	RequestTimeout        time.Duration
	OwnerIDs              []int64
	// GuestModeEnabled controls guest mentions and inline locked envelopes.
	GuestModeEnabled bool
}

type DatabaseConfig struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ConnectTimeout  time.Duration
}

type WhisperConfig struct {
	DraftTTL                       time.Duration
	DefaultTTL                     time.Duration
	MaxTTL                         time.Duration
	DefaultOneTime                 bool
	ProtectContent                 bool
	MaxActiveDraftsPerUser         int
	MaxWhispersPerUserPerHour      int
	MaxActiveGuestRequestsPerUser  int
	MaxGuestRequestsPerUserPerHour int
	AllowedChatIDs                 []int64
	// AllowAllChats explicitly opts in to operating in every group that adds
	// the bot when ALLOWED_CHAT_IDS is empty. Required in production.
	AllowAllChats           bool
	PublishLeaseTimeout     time.Duration
	PublishInterval         time.Duration
	EphemeralDeleteAfter    time.Duration
	EphemeralDeleteInterval time.Duration
}

type MediaConfig struct {
	StorageMode     string
	MaxBytes        int64
	Retention       time.Duration
	DownloadTimeout time.Duration
	EncryptionKeyID string
	EncryptionKey   SecretBytes
	PreviousKeys    map[string]SecretBytes
}

type CleanupConfig struct {
	Interval                  time.Duration
	BatchSize                 int
	ProcessedUpdateRetention  time.Duration
	ObservedIdentityRetention time.Duration
}

// Default returns non-secret defaults. Required credentials and identifiers are
// intentionally left empty so a missing production setting fails closed.
func Default() Config {
	return Config{
		AppEnv:   "development",
		LogLevel: "info",
		HTTP: HTTPConfig{
			Addr:              ":8080",
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      20 * time.Second,
			IdleTimeout:       60 * time.Second,
			ShutdownTimeout:   15 * time.Second,
		},
		Telegram: TelegramConfig{
			APIBaseURL:            "https://api.telegram.org",
			UpdateMode:            UpdateModePolling,
			WebhookMaxConnections: 4,
			PollTimeout:           30 * time.Second,
			RequestTimeout:        15 * time.Second,
			GuestModeEnabled:      true,
		},
		Database: DatabaseConfig{
			MaxConns:        10,
			MinConns:        2,
			MaxConnLifetime: time.Hour,
			MaxConnIdleTime: 30 * time.Minute,
			ConnectTimeout:  10 * time.Second,
		},
		Whisper: WhisperConfig{
			DraftTTL:                       10 * time.Minute,
			DefaultTTL:                     24 * time.Hour,
			MaxTTL:                         7 * 24 * time.Hour,
			DefaultOneTime:                 true,
			ProtectContent:                 true,
			MaxActiveDraftsPerUser:         1,
			MaxWhispersPerUserPerHour:      30,
			MaxActiveGuestRequestsPerUser:  25,
			MaxGuestRequestsPerUserPerHour: 100,
			PublishLeaseTimeout:            2 * time.Minute,
			PublishInterval:                2 * time.Second,
			EphemeralDeleteAfter:           30 * time.Second,
			EphemeralDeleteInterval:        2 * time.Second,
		},
		Media: MediaConfig{
			StorageMode:     mediaStoragePostgres,
			MaxBytes:        20 * 1024 * 1024,
			Retention:       30 * 24 * time.Hour,
			DownloadTimeout: 2 * time.Minute,
			EncryptionKeyID: "v1",
		},
		Cleanup: CleanupConfig{
			Interval:                  5 * time.Minute,
			BatchSize:                 500,
			ProcessedUpdateRetention:  7 * 24 * time.Hour,
			ObservedIdentityRetention: 90 * 24 * time.Hour,
		},
	}
}

// Load reads configuration from the process environment. Any variable may
// alternatively be supplied as VAR_FILE pointing at a file containing the
// value (Docker/Compose file-based secrets).
func Load() (Config, error) {
	var fileErr error
	lookup := func(key string) (string, bool) {
		value, ok := os.LookupEnv(key)
		if ok && strings.TrimSpace(value) != "" {
			return value, true
		}
		path, hasFile := os.LookupEnv(key + "_FILE")
		if !hasFile || strings.TrimSpace(path) == "" {
			return value, ok
		}
		data, err := os.ReadFile(strings.TrimSpace(path))
		if err != nil {
			if fileErr == nil {
				fileErr = fmt.Errorf("config %s_FILE: %w", key, err)
			}
			return "", false
		}
		return strings.TrimSpace(string(data)), true
	}
	cfg, err := LoadFromLookup(lookup)
	return cfg, errors.Join(fileErr, err)
}

// LoadFromLookup is Load with an injectable environment lookup, useful for
// tests and embedding the application in another process.
func LoadFromLookup(lookup func(string) (string, bool)) (Config, error) {
	cfg := Default()
	l := envLoader{lookup: lookup}

	l.text("APP_ENV", &cfg.AppEnv)
	l.text("LOG_LEVEL", &cfg.LogLevel)

	l.text("HTTP_ADDR", &cfg.HTTP.Addr)
	l.duration("HTTP_READ_HEADER_TIMEOUT", &cfg.HTTP.ReadHeaderTimeout)
	l.duration("HTTP_READ_TIMEOUT", &cfg.HTTP.ReadTimeout)
	l.duration("HTTP_WRITE_TIMEOUT", &cfg.HTTP.WriteTimeout)
	l.duration("HTTP_IDLE_TIMEOUT", &cfg.HTTP.IdleTimeout)
	l.duration("HTTP_SHUTDOWN_TIMEOUT", &cfg.HTTP.ShutdownTimeout)

	l.text("TELEGRAM_BOT_TOKEN", &cfg.Telegram.BotToken)
	l.text("TELEGRAM_BOT_USERNAME", &cfg.Telegram.BotUsername)
	l.text("TELEGRAM_API_BASE_URL", &cfg.Telegram.APIBaseURL)
	l.updateMode("TELEGRAM_UPDATE_MODE", &cfg.Telegram.UpdateMode)
	l.text("TELEGRAM_WEBHOOK_PUBLIC_URL", &cfg.Telegram.WebhookPublicURL)
	l.text("TELEGRAM_WEBHOOK_SECRET", &cfg.Telegram.WebhookSecret)
	l.integer("TELEGRAM_WEBHOOK_MAX_CONNECTIONS", &cfg.Telegram.WebhookMaxConnections)
	l.duration("TELEGRAM_POLL_TIMEOUT", &cfg.Telegram.PollTimeout)
	l.duration("TELEGRAM_REQUEST_TIMEOUT", &cfg.Telegram.RequestTimeout)
	l.int64List("OWNER_TELEGRAM_IDS", &cfg.Telegram.OwnerIDs, true)
	l.boolean("GUEST_MODE_ENABLED", &cfg.Telegram.GuestModeEnabled)

	l.text("DATABASE_URL", &cfg.Database.URL)
	l.int32("DB_MAX_CONNS", &cfg.Database.MaxConns)
	l.int32("DB_MIN_CONNS", &cfg.Database.MinConns)
	l.duration("DB_MAX_CONN_LIFETIME", &cfg.Database.MaxConnLifetime)
	l.duration("DB_MAX_CONN_IDLE_TIME", &cfg.Database.MaxConnIdleTime)
	l.duration("DB_CONNECT_TIMEOUT", &cfg.Database.ConnectTimeout)

	l.duration("DRAFT_TTL", &cfg.Whisper.DraftTTL)
	l.duration("DEFAULT_WHISPER_TTL", &cfg.Whisper.DefaultTTL)
	l.duration("MAX_WHISPER_TTL", &cfg.Whisper.MaxTTL)
	l.boolean("DEFAULT_ONE_TIME", &cfg.Whisper.DefaultOneTime)
	l.boolean("PROTECT_CONTENT", &cfg.Whisper.ProtectContent)
	l.integer("MAX_ACTIVE_DRAFTS_PER_USER", &cfg.Whisper.MaxActiveDraftsPerUser)
	l.integer("MAX_WHISPERS_PER_USER_PER_HOUR", &cfg.Whisper.MaxWhispersPerUserPerHour)
	l.integer("GUEST_MAX_ACTIVE_REQUESTS_PER_USER", &cfg.Whisper.MaxActiveGuestRequestsPerUser)
	l.integer("GUEST_MAX_REQUESTS_PER_USER_PER_HOUR", &cfg.Whisper.MaxGuestRequestsPerUserPerHour)
	l.int64List("ALLOWED_CHAT_IDS", &cfg.Whisper.AllowedChatIDs, false)
	l.boolean("ALLOW_ALL_CHATS", &cfg.Whisper.AllowAllChats)
	l.duration("PUBLISH_LEASE_TIMEOUT", &cfg.Whisper.PublishLeaseTimeout)
	l.duration("PUBLISH_INTERVAL", &cfg.Whisper.PublishInterval)
	l.duration("EPHEMERAL_DELETE_AFTER", &cfg.Whisper.EphemeralDeleteAfter)
	l.duration("EPHEMERAL_DELETE_INTERVAL", &cfg.Whisper.EphemeralDeleteInterval)

	l.text("MEDIA_STORAGE", &cfg.Media.StorageMode)
	l.int64("MAX_MEDIA_BYTES", &cfg.Media.MaxBytes)
	l.duration("MEDIA_RETENTION", &cfg.Media.Retention)
	l.duration("MEDIA_DOWNLOAD_TIMEOUT", &cfg.Media.DownloadTimeout)
	l.text("MEDIA_ENCRYPTION_KEY_ID", &cfg.Media.EncryptionKeyID)
	l.base64Secret("MEDIA_ENCRYPTION_KEY", &cfg.Media.EncryptionKey)
	l.base64SecretMap("MEDIA_ENCRYPTION_PREVIOUS_KEYS", &cfg.Media.PreviousKeys)

	l.duration("CLEANUP_INTERVAL", &cfg.Cleanup.Interval)
	l.integer("CLEANUP_BATCH_SIZE", &cfg.Cleanup.BatchSize)
	l.duration("PROCESSED_UPDATE_RETENTION", &cfg.Cleanup.ProcessedUpdateRetention)
	l.duration("OBSERVED_IDENTITY_RETENTION", &cfg.Cleanup.ObservedIdentityRetention)

	if l.err != nil {
		return Config{}, l.err
	}

	cfg.Telegram.BotUsername = strings.TrimPrefix(cfg.Telegram.BotUsername, "@")
	cfg.Telegram.APIBaseURL = strings.TrimRight(cfg.Telegram.APIBaseURL, "/")
	cfg.Media.StorageMode = strings.ToLower(cfg.Media.StorageMode)

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	cfg.Warnings = cfg.warnings()
	return cfg, nil
}

// Validate checks cross-field and security-sensitive invariants.
func (c Config) Validate() error {
	if c.AppEnv == "" {
		return errors.New("config APP_ENV: must not be empty")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config LOG_LEVEL: unsupported value %q", c.LogLevel)
	}
	if _, _, err := net.SplitHostPort(c.HTTP.Addr); err != nil {
		return fmt.Errorf("config HTTP_ADDR: must be host:port: %w", err)
	}
	if c.HTTP.ReadHeaderTimeout <= 0 || c.HTTP.ReadTimeout <= 0 ||
		c.HTTP.WriteTimeout <= 0 || c.HTTP.IdleTimeout <= 0 || c.HTTP.ShutdownTimeout <= 0 {
		return errors.New("config HTTP timeouts: all values must be positive")
	}

	if c.Telegram.BotToken == "" || strings.EqualFold(c.Telegram.BotToken, "replace_me") {
		return errors.New("config TELEGRAM_BOT_TOKEN: a real token is required")
	}
	if c.Telegram.BotUsername == "" || strings.ContainsAny(c.Telegram.BotUsername, "@ \t\r\n") {
		return errors.New("config TELEGRAM_BOT_USERNAME: a username without @ is required")
	}
	allowInsecureAPI := c.AppEnv == "development" || c.AppEnv == "test"
	if err := validateHTTPURL("TELEGRAM_API_BASE_URL", c.Telegram.APIBaseURL, allowInsecureAPI); err != nil {
		return err
	}
	if c.Telegram.PollTimeout <= 0 || c.Telegram.PollTimeout > 50*time.Second {
		return errors.New("config TELEGRAM_POLL_TIMEOUT: must be greater than zero and at most 50s")
	}
	if c.Telegram.RequestTimeout <= 0 {
		return errors.New("config TELEGRAM_REQUEST_TIMEOUT: must be positive")
	}
	if len(c.Telegram.OwnerIDs) == 0 {
		return errors.New("config OWNER_TELEGRAM_IDS: at least one owner is required")
	}
	for _, id := range c.Telegram.OwnerIDs {
		if id <= 0 {
			return errors.New("config OWNER_TELEGRAM_IDS: user IDs must be positive")
		}
	}
	if c.Telegram.WebhookMaxConnections < 1 || c.Telegram.WebhookMaxConnections > 100 {
		return errors.New("config TELEGRAM_WEBHOOK_MAX_CONNECTIONS: must be between 1 and 100")
	}
	switch c.Telegram.UpdateMode {
	case UpdateModePolling:
	case UpdateModeWebhook:
		if err := validateHTTPURL("TELEGRAM_WEBHOOK_PUBLIC_URL", c.Telegram.WebhookPublicURL, false); err != nil {
			return err
		}
		webhookURL, _ := url.Parse(c.Telegram.WebhookPublicURL)
		if webhookURL.Path != telegramWebhookPath || webhookURL.RawPath != "" {
			return fmt.Errorf("config TELEGRAM_WEBHOOK_PUBLIC_URL: path must be %s", telegramWebhookPath)
		}
		if len(c.Telegram.WebhookSecret) < 32 || len(c.Telegram.WebhookSecret) > 256 {
			return errors.New("config TELEGRAM_WEBHOOK_SECRET: must contain 32 to 256 characters")
		}
		for _, r := range c.Telegram.WebhookSecret {
			if !isWebhookSecretRune(r) {
				return errors.New("config TELEGRAM_WEBHOOK_SECRET: only A-Z, a-z, 0-9, _ and - are allowed")
			}
		}
		if isRepeatedString(c.Telegram.WebhookSecret) {
			return errors.New("config TELEGRAM_WEBHOOK_SECRET: must not be a single repeated character")
		}
		// Webhook processing is synchronous. Ensure the HTTP response deadline
		// cannot expire during the longest media path: metadata lookup, file
		// download, envelope publication, and the best-effort acknowledgement.
		remaining := c.HTTP.WriteTimeout
		if remaining < c.Media.DownloadTimeout {
			return errors.New("config HTTP_WRITE_TIMEOUT: webhook mode requires at least MEDIA_DOWNLOAD_TIMEOUT + 3*TELEGRAM_REQUEST_TIMEOUT")
		}
		remaining -= c.Media.DownloadTimeout
		for range 3 {
			if remaining < c.Telegram.RequestTimeout {
				return errors.New("config HTTP_WRITE_TIMEOUT: webhook mode requires at least MEDIA_DOWNLOAD_TIMEOUT + 3*TELEGRAM_REQUEST_TIMEOUT")
			}
			remaining -= c.Telegram.RequestTimeout
		}
	default:
		return fmt.Errorf("config TELEGRAM_UPDATE_MODE: unsupported value %q", c.Telegram.UpdateMode)
	}

	if err := validateDatabaseURL(c.Database.URL, c.ProductionLike()); err != nil {
		return err
	}
	if c.Database.MaxConns <= 0 || c.Database.MinConns < 0 || c.Database.MinConns > c.Database.MaxConns {
		return errors.New("config DB pool: require 0 <= DB_MIN_CONNS <= DB_MAX_CONNS and DB_MAX_CONNS > 0")
	}
	if c.Database.MaxConnLifetime <= 0 || c.Database.MaxConnIdleTime <= 0 || c.Database.ConnectTimeout <= 0 {
		return errors.New("config DB pool durations: all values must be positive")
	}

	if c.Whisper.DraftTTL <= 0 {
		return errors.New("config DRAFT_TTL: must be positive")
	}
	if c.Whisper.DefaultTTL <= 0 || c.Whisper.MaxTTL <= 0 || c.Whisper.DefaultTTL > c.Whisper.MaxTTL {
		return errors.New("config whisper TTL: require 0 < DEFAULT_WHISPER_TTL <= MAX_WHISPER_TTL")
	}
	if c.Whisper.MaxActiveDraftsPerUser != 1 {
		return errors.New("config MAX_ACTIVE_DRAFTS_PER_USER: V1 requires exactly 1")
	}
	if c.Whisper.MaxWhispersPerUserPerHour <= 0 {
		return errors.New("config MAX_WHISPERS_PER_USER_PER_HOUR: must be positive")
	}
	if c.Whisper.MaxActiveGuestRequestsPerUser <= 0 || c.Whisper.MaxGuestRequestsPerUserPerHour <= 0 {
		return errors.New("config guest limits: GUEST_MAX_ACTIVE_REQUESTS_PER_USER and GUEST_MAX_REQUESTS_PER_USER_PER_HOUR must be positive")
	}
	// Fail closed in production: an operator who forgets ALLOWED_CHAT_IDS
	// must not silently get a bot that answers in every group on Telegram.
	if c.ProductionLike() && len(c.Whisper.AllowedChatIDs) == 0 && !c.Whisper.AllowAllChats {
		return errors.New("config ALLOWED_CHAT_IDS: production requires an explicit allowlist (or ALLOW_ALL_CHATS=true to opt in deliberately)")
	}
	if c.Whisper.PublishLeaseTimeout <= 0 || c.Whisper.PublishInterval <= 0 {
		return errors.New("config publication: PUBLISH_LEASE_TIMEOUT and PUBLISH_INTERVAL must be positive")
	}
	if c.Whisper.PublishLeaseTimeout < c.Telegram.RequestTimeout ||
		c.Whisper.PublishLeaseTimeout-c.Telegram.RequestTimeout < publicationLeaseFinishGap {
		return errors.New("config PUBLISH_LEASE_TIMEOUT: must exceed TELEGRAM_REQUEST_TIMEOUT by at least 5s")
	}
	if c.Whisper.EphemeralDeleteAfter <= 0 || c.Whisper.EphemeralDeleteInterval <= 0 {
		return errors.New("config ephemeral deletion: EPHEMERAL_DELETE_AFTER and EPHEMERAL_DELETE_INTERVAL must be positive")
	}
	for _, id := range c.Whisper.AllowedChatIDs {
		if id == 0 {
			return errors.New("config ALLOWED_CHAT_IDS: chat ID zero is invalid")
		}
	}

	if c.Media.StorageMode != mediaStoragePostgres {
		return fmt.Errorf("config MEDIA_STORAGE: unsupported value %q", c.Media.StorageMode)
	}
	if c.Media.MaxBytes <= 0 || c.Media.MaxBytes > 20*1024*1024 {
		return errors.New("config MAX_MEDIA_BYTES: must be positive and at most 20971520 bytes")
	}
	if c.Media.Retention <= 0 || c.Media.DownloadTimeout <= 0 {
		return errors.New("config media durations: MEDIA_RETENTION and MEDIA_DOWNLOAD_TIMEOUT must be positive")
	}
	if c.Media.EncryptionKeyID == "" {
		return errors.New("config MEDIA_ENCRYPTION_KEY_ID: must not be empty")
	}
	if len(c.Media.EncryptionKey) != 32 {
		return errors.New("config MEDIA_ENCRYPTION_KEY: must decode to exactly 32 bytes")
	}
	// Catch copy-paste and placeholder deployment errors: an all-zero or
	// single-byte-pattern key silently encrypts everything under a guessable
	// key.
	if isLowEntropyBytes(c.Media.EncryptionKey) {
		return errors.New("config MEDIA_ENCRYPTION_KEY: key material looks non-random (all-zero or repeated bytes)")
	}
	for id, prevKey := range c.Media.PreviousKeys {
		if id == "" {
			return errors.New("config MEDIA_ENCRYPTION_PREVIOUS_KEYS: key ID cannot be empty")
		}
		if id == c.Media.EncryptionKeyID {
			return errors.New("config MEDIA_ENCRYPTION_PREVIOUS_KEYS: contains the active key ID")
		}
		if len(prevKey) != 32 {
			return fmt.Errorf("config MEDIA_ENCRYPTION_PREVIOUS_KEYS: key %q must decode to exactly 32 bytes", id)
		}
		if isLowEntropyBytes(prevKey) {
			return fmt.Errorf("config MEDIA_ENCRYPTION_PREVIOUS_KEYS: key %q material looks non-random", id)
		}
	}

	if c.Cleanup.Interval <= 0 || c.Cleanup.BatchSize <= 0 || c.Cleanup.ProcessedUpdateRetention <= 0 || c.Cleanup.ObservedIdentityRetention < 0 {
		return errors.New("config cleanup: interval, batch size, and processed-update retention must be positive, and observed-identity retention must be non-negative")
	}
	return nil
}

func (c Config) IsOwner(userID int64) bool {
	for _, id := range c.Telegram.OwnerIDs {
		if id == userID {
			return true
		}
	}
	return false
}

// ProductionLike reports whether the deployment demands production posture.
func (c Config) ProductionLike() bool {
	return c.AppEnv == "production"
}

// warnings lists non-fatal conditions an operator should see at startup.
func (c Config) warnings() []string {
	var warnings []string
	switch c.AppEnv {
	case "development", "test", "production":
	default:
		warnings = append(warnings,
			"APP_ENV "+c.AppEnv+" is not one of development, test, or production; production posture is not applied")
	}
	if !c.ProductionLike() && len(c.Whisper.AllowedChatIDs) == 0 && c.Whisper.AllowAllChats {
		warnings = append(warnings,
			"ALLOW_ALL_CHATS is enabled with an empty ALLOWED_CHAT_IDS: the bot will operate in every group that adds it")
	}
	if database, err := url.Parse(c.Database.URL); err == nil {
		switch database.Query().Get("sslmode") {
		case "disable", "allow":
			if c.ProductionLike() {
				// Validate already rejected this; unreachable defensively.
				warnings = append(warnings, "DATABASE_URL uses an unencrypted transport")
			} else {
				warnings = append(warnings,
					"DATABASE_URL sslmode=disable: database traffic is unencrypted; acceptable only on a trusted internal network")
			}
		case "":
			warnings = append(warnings,
				"DATABASE_URL has no explicit sslmode; PostgreSQL defaults to 'prefer'. Set sslmode=require or verify-full explicitly")
		}
	}
	if !c.Telegram.GuestModeEnabled {
		warnings = append(warnings, "GUEST_MODE_ENABLED=false: guest mentions and inline locked envelopes are disabled")
	}
	return warnings
}

func isRepeatedString(value string) bool {
	if len(value) < 2 {
		return false
	}
	first := value[0]
	for index := 1; index < len(value); index++ {
		if value[index] != first {
			return false
		}
	}
	return true
}

func isLowEntropyBytes(value []byte) bool {
	if len(value) == 0 {
		return true
	}
	allZero := true
	repeated := true
	for index, b := range value {
		if b != 0 {
			allZero = false
		}
		if b != value[0] {
			repeated = false
		}
		if index > 0 && !allZero && !repeated {
			return false
		}
	}
	return allZero || repeated
}

// IsChatAllowed returns true when the allowlist is empty or contains chatID.
func (c Config) IsChatAllowed(chatID int64) bool {
	if len(c.Whisper.AllowedChatIDs) == 0 {
		return true
	}
	for _, id := range c.Whisper.AllowedChatIDs {
		if id == chatID {
			return true
		}
	}
	return false
}

func validateHTTPURL(name, raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("config %s: must be an absolute URL without credentials, query, or fragment", name)
	}
	if u.Scheme == "https" || (allowHTTP && u.Scheme == "http") {
		return nil
	}
	return fmt.Errorf("config %s: must use HTTPS", name)
}

func validateDatabaseURL(raw string, productionLike bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return errors.New("config DATABASE_URL: must be an absolute postgres or postgresql URL")
	}
	// Ciphertext, token hashes, and audit rows cross this channel. Production
	// deployments must not send them in plaintext; internal Compose networks
	// in development may.
	if productionLike {
		switch u.Query().Get("sslmode") {
		case "disable", "allow":
			return errors.New("config DATABASE_URL: sslmode=disable/allow is not permitted outside development; use require or verify-full")
		}
	}
	return nil
}

func isWebhookSecretRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
		r >= '0' && r <= '9' || r == '_' || r == '-'
}

type envLoader struct {
	lookup func(string) (string, bool)
	err    error
}

func (l *envLoader) value(key string) (string, bool) {
	if l.err != nil {
		return "", false
	}
	v, ok := l.lookup(key)
	return strings.TrimSpace(v), ok
}

func (l *envLoader) fail(key string, err error) {
	if l.err == nil {
		l.err = fmt.Errorf("config %s: %w", key, err)
	}
}

func (l *envLoader) text(key string, dst *string) {
	if v, ok := l.value(key); ok {
		*dst = v
	}
}

func (l *envLoader) duration(key string, dst *time.Duration) {
	v, ok := l.value(key)
	if !ok {
		return
	}
	parsed, err := time.ParseDuration(v)
	if err != nil {
		l.fail(key, err)
		return
	}
	*dst = parsed
}

func (l *envLoader) boolean(key string, dst *bool) {
	v, ok := l.value(key)
	if !ok {
		return
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		l.fail(key, err)
		return
	}
	*dst = parsed
}

func (l *envLoader) integer(key string, dst *int) {
	v, ok := l.value(key)
	if !ok {
		return
	}
	parsed, err := strconv.Atoi(v)
	if err != nil {
		l.fail(key, err)
		return
	}
	*dst = parsed
}

func (l *envLoader) int32(key string, dst *int32) {
	v, ok := l.value(key)
	if !ok {
		return
	}
	parsed, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		l.fail(key, err)
		return
	}
	*dst = int32(parsed)
}

func (l *envLoader) int64(key string, dst *int64) {
	v, ok := l.value(key)
	if !ok {
		return
	}
	parsed, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		l.fail(key, err)
		return
	}
	*dst = parsed
}

func (l *envLoader) int64List(key string, dst *[]int64, positiveOnly bool) {
	v, ok := l.value(key)
	if !ok {
		return
	}
	if v == "" {
		*dst = nil
		return
	}
	seen := make(map[int64]struct{})
	result := make([]int64, 0, strings.Count(v, ",")+1)
	for _, part := range strings.Split(v, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id == 0 || positiveOnly && id < 0 {
			l.fail(key, fmt.Errorf("invalid ID %q", strings.TrimSpace(part)))
			return
		}
		if _, exists := seen[id]; !exists {
			seen[id] = struct{}{}
			result = append(result, id)
		}
	}
	*dst = result
}

func (l *envLoader) updateMode(key string, dst *UpdateMode) {
	v, ok := l.value(key)
	if ok {
		*dst = UpdateMode(strings.ToLower(v))
	}
}

func (l *envLoader) base64Secret(key string, dst *SecretBytes) {
	v, ok := l.value(key)
	if !ok {
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		decoded, err = base64.RawURLEncoding.DecodeString(v)
	}
	if err != nil {
		l.fail(key, errors.New("must be standard or unpadded URL-safe base64"))
		return
	}
	*dst = SecretBytes(decoded)
}

func (l *envLoader) base64SecretMap(key string, dst *map[string]SecretBytes) {
	v, ok := l.value(key)
	if !ok || v == "" {
		return
	}
	result := make(map[string]SecretBytes)
	for _, entry := range strings.Split(v, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 {
			l.fail(key, fmt.Errorf("invalid key entry %q; expected id:base64", entry))
			return
		}
		id := strings.TrimSpace(parts[0])
		secretStr := strings.TrimSpace(parts[1])
		if id == "" {
			l.fail(key, errors.New("key ID cannot be empty"))
			return
		}
		decoded, err := base64.StdEncoding.DecodeString(secretStr)
		if err != nil {
			decoded, err = base64.RawURLEncoding.DecodeString(secretStr)
		}
		if err != nil {
			l.fail(key, fmt.Errorf("key %q: must be standard or unpadded URL-safe base64", id))
			return
		}
		result[id] = SecretBytes(decoded)
	}
	*dst = result
}
