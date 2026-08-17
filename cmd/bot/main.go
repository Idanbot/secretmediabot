package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/idan/secretmediabot/internal/app"
	"github.com/idan/secretmediabot/internal/bot"
	"github.com/idan/secretmediabot/internal/config"
	"github.com/idan/secretmediabot/internal/httpserver"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/secretcrypto"
	"github.com/idan/secretmediabot/internal/service"
	"github.com/idan/secretmediabot/internal/telegram"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.Error("bot stopped", "err", err)
		os.Exit(1)
	}
}

func healthcheck() error {
	client := http.Client{Timeout: 4 * time.Second}
	response, err := client.Get("http://127.0.0.1:8080/readyz")
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("readiness endpoint returned HTTP %d", response.StatusCode)
	}
	return nil
}

func run(parent context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger(cfg.AppEnv, cfg.LogLevel)
	slog.SetDefault(logger)

	connectCtx, cancelConnect := context.WithTimeout(parent, cfg.Database.ConnectTimeout)
	database, err := repository.Open(connectCtx, repository.DatabaseOptions{
		URL:             cfg.Database.URL,
		MaxOpenConns:    int(cfg.Database.MaxConns),
		MinIdleConns:    int(cfg.Database.MinConns),
		ConnMaxLifetime: cfg.Database.MaxConnLifetime,
		ConnMaxIdleTime: cfg.Database.MaxConnIdleTime,
	})
	cancelConnect()
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()

	migrateCtx, cancelMigrate := context.WithTimeout(parent, 2*time.Minute)
	err = database.Migrate(migrateCtx)
	cancelMigrate()
	if err != nil {
		return err
	}

	activeKey := append([]byte(nil), cfg.Media.EncryptionKey...)
	keyring, err := secretcrypto.NewKeyring(cfg.Media.EncryptionKeyID, map[string][]byte{
		cfg.Media.EncryptionKeyID: activeKey,
	})
	secretcrypto.Zero(activeKey)
	secretcrypto.Zero(cfg.Media.EncryptionKey)
	if err != nil {
		return fmt.Errorf("initialize content encryption: %w", err)
	}

	httpClient := &http.Client{
		Transport: tunedTransport(),
		Timeout: maxDuration(
			cfg.Media.DownloadTimeout+cfg.Telegram.RequestTimeout,
			cfg.Telegram.PollTimeout+cfg.Telegram.RequestTimeout,
		),
	}
	defer httpClient.CloseIdleConnections()
	telegramClient, err := telegram.NewClient(telegram.ClientConfig{
		Token: cfg.Telegram.BotToken, BaseURL: cfg.Telegram.APIBaseURL,
		HTTPClient: httpClient, MaxDownloadBytes: cfg.Media.MaxBytes, MaxUploadBytes: cfg.Media.MaxBytes,
	})
	if err != nil {
		return err
	}

	startupCtx, cancelStartup := context.WithTimeout(parent, cfg.Telegram.RequestTimeout)
	me, err := telegramClient.GetMe(startupCtx)
	cancelStartup()
	if err != nil {
		return fmt.Errorf("verify Telegram bot identity: %w", err)
	}
	if !strings.EqualFold(me.Username, cfg.Telegram.BotUsername) {
		return errors.New("configured Telegram bot username does not match the bot token")
	}
	commandsCtx, cancelCommands := context.WithTimeout(parent, cfg.Telegram.RequestTimeout)
	err = telegramClient.SetMyCommands(commandsCtx, telegram.SetMyCommandsRequest{Commands: []telegram.BotCommand{
		{Command: "whisper", Description: "Create a private locked secret", IsEphemeral: true},
		{Command: "start", Description: "Open the private composer"},
		{Command: "cancel", Description: "Cancel the active secret"},
		{Command: "privacy", Description: "Show the privacy model"},
		{Command: "help", Description: "Show help"},
	}})
	cancelCommands()
	if err != nil {
		return fmt.Errorf("configure Telegram commands: %w", err)
	}
	if !me.SupportsGuestQueries {
		logger.Warn("Telegram Guest Mode is disabled; enable it in BotFather to use @bot target guest requests")
	}
	if !me.SupportsInlineQueries {
		logger.Warn("Telegram inline mode is disabled; enable it in BotFather to use inline locked envelopes")
	}

	store := repository.NewStore(database)
	useCases, err := service.New(store, keyring, service.Options{
		DraftTTL:                  cfg.Whisper.DraftTTL,
		WhisperTTL:                cfg.Whisper.DefaultTTL,
		ContentRetention:          cfg.Media.Retention,
		IngestLease:               cfg.Media.DownloadTimeout + cfg.Telegram.RequestTimeout + 30*time.Second,
		OpenLease:                 telegram.EphemeralCallbackWindow + 5*time.Second,
		PublishLease:              cfg.Whisper.PublishLeaseTimeout,
		EphemeralDeleteAfter:      cfg.Whisper.EphemeralDeleteAfter,
		MaxMediaBytes:             cfg.Media.MaxBytes,
		MaxActiveDraftsPerUser:    cfg.Whisper.MaxActiveDraftsPerUser,
		MaxWhispersPerUserPerHour: cfg.Whisper.MaxWhispersPerUserPerHour,
		DefaultOneTime:            cfg.Whisper.DefaultOneTime,
		ProtectContent:            cfg.Whisper.ProtectContent,
		AllowedChatIDs:            cfg.Whisper.AllowedChatIDs,
		OwnerIDs:                  cfg.Telegram.OwnerIDs,
	})
	if err != nil {
		return fmt.Errorf("initialize service: %w", err)
	}

	handler, err := bot.New(bot.Config{
		Service: useCases, Telegram: telegramClient, BotUsername: cfg.Telegram.BotUsername,
		MaxMediaBytes: cfg.Media.MaxBytes, MediaDownloadTimeout: cfg.Media.DownloadTimeout,
		RequestTimeout: cfg.Telegram.RequestTimeout, Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("initialize Telegram handler: %w", err)
	}
	updateLease := maxDuration(3*time.Minute, cfg.Media.DownloadTimeout+3*cfg.Telegram.RequestTimeout+30*time.Second)
	processor, err := app.NewUpdateProcessor(store, handler, updateLease)
	if err != nil {
		return fmt.Errorf("initialize update processor: %w", err)
	}

	if err := configureTelegramTransport(parent, cfg, telegramClient); err != nil {
		return err
	}

	server := httpserver.New(httpserver.Config{
		Addr: cfg.HTTP.Addr, ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		ReadTimeout: cfg.HTTP.ReadTimeout, WriteTimeout: cfg.HTTP.WriteTimeout,
		IdleTimeout: cfg.HTTP.IdleTimeout, WebhookEnabled: cfg.Telegram.UpdateMode == config.UpdateModeWebhook,
		WebhookSecret: cfg.Telegram.WebhookSecret,
	}, database, processor, logger)
	cleanup := app.NewCleanupWorker(
		store, cfg.Cleanup.Interval, cfg.Cleanup.BatchSize, cfg.Cleanup.ProcessedUpdateRetention, logger,
	)
	deleter := app.NewEphemeralDeleteWorker(store, telegramClient, cfg.Whisper.EphemeralDeleteInterval, logger)
	guestDeleter := app.NewGuestPrivateDeleteWorker(store, telegramClient, cfg.Whisper.EphemeralDeleteInterval, logger)
	publisher, err := app.NewPublicationWorkerWithTimeout(
		handler,
		cfg.Whisper.PublishInterval,
		cfg.Telegram.RequestTimeout,
		logger,
	)
	if err != nil {
		return fmt.Errorf("initialize publication worker: %w", err)
	}

	runCtx, cancelRun := context.WithCancel(parent)
	defer cancelRun()
	type runnerResult struct {
		name string
		err  error
	}
	results := make(chan runnerResult, 6)
	var runners sync.WaitGroup
	start := func(name string, fn func(context.Context) error) {
		runners.Add(1)
		go func() {
			defer runners.Done()
			results <- runnerResult{name: name, err: fn(runCtx)}
		}()
	}

	start("http server", func(context.Context) error { return server.ListenAndServe() })
	start("cleanup worker", cleanup.Run)
	start("ephemeral deletion worker", deleter.Run)
	start("guest private deletion worker", guestDeleter.Run)
	start("publication worker", publisher.Run)
	if cfg.Telegram.UpdateMode == config.UpdateModePolling {
		poller := app.NewPoller(
			telegramClient,
			processor,
			cfg.Telegram.PollTimeout,
			cfg.Telegram.RequestTimeout,
			logger,
		)
		start("Telegram poller", poller.Run)
	}

	logger.Info("bot started", "version", version, "commit", commit, "update_mode", cfg.Telegram.UpdateMode)
	var runErr error
	select {
	case <-parent.Done():
	case result := <-results:
		if parent.Err() != nil {
			break
		}
		if result.err != nil {
			runErr = fmt.Errorf("%s: %w", result.name, result.err)
		} else {
			runErr = fmt.Errorf("%s stopped unexpectedly", result.name)
		}
	}

	cancelRun()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(parent), cfg.HTTP.ShutdownTimeout)
	shutdownErr := server.Shutdown(shutdownCtx)
	cancelShutdown()
	runners.Wait()
	if shutdownErr != nil && runErr == nil {
		runErr = fmt.Errorf("shut down HTTP server: %w", shutdownErr)
	}
	logger.Info("bot stopped")
	return runErr
}

func configureTelegramTransport(ctx context.Context, cfg config.Config, client *telegram.Client) error {
	requestCtx, cancel := context.WithTimeout(ctx, cfg.Telegram.RequestTimeout)
	defer cancel()
	if cfg.Telegram.UpdateMode == config.UpdateModePolling {
		if err := client.DeleteWebhook(requestCtx, telegram.DeleteWebhookRequest{DropPendingUpdates: false}); err != nil {
			return fmt.Errorf("disable Telegram webhook: %w", err)
		}
		return nil
	}
	if err := client.SetWebhook(requestCtx, telegram.SetWebhookRequest{
		URL: cfg.Telegram.WebhookPublicURL, SecretToken: cfg.Telegram.WebhookSecret,
		AllowedUpdates: []string{"message", "callback_query", "guest_message", "inline_query"}, MaxConnections: cfg.Telegram.WebhookMaxConnections,
	}); err != nil {
		return fmt.Errorf("configure Telegram webhook: %w", err)
	}
	return nil
}

func tunedTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 20
	transport.IdleConnTimeout = 90 * time.Second
	transport.ResponseHeaderTimeout = 60 * time.Second
	return transport
}

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}
	return right
}

func newLogger(environment, levelName string) *slog.Logger {
	var level slog.Level
	switch levelName {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	options := &slog.HandlerOptions{Level: level, AddSource: environment == "development"}
	if environment == "development" {
		return slog.New(slog.NewTextHandler(os.Stderr, options))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, options))
}
