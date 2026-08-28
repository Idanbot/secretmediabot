package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
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
	version       = "dev"
	commit        = "unknown"
	commitMessage = "unknown"
	buildTime     = "unknown"
	ciRunNumber   = "local"
)

func resolveBuildInfo() bot.BuildInfo {
	v := version
	c := commit
	msg := commitMessage
	bt := buildTime
	ci := ciRunNumber

	// 1. Environment variable overrides (from .env or compose)
	if env := os.Getenv("APP_VERSION"); env != "" && (v == "dev" || v == "") {
		v = env
	}
	if env := os.Getenv("BOT_VERSION"); env != "" && (v == "dev" || v == "") {
		v = env
	}
	if env := os.Getenv("GIT_COMMIT"); env != "" && (c == "unknown" || c == "") {
		c = env
	}
	if env := os.Getenv("COMMIT_MESSAGE"); env != "" && (msg == "unknown" || msg == "") {
		msg = env
	}
	if env := os.Getenv("BUILD_TIME"); env != "" && (bt == "unknown" || bt == "") {
		bt = env
	}
	if env := os.Getenv("CI_RUN_NUMBER"); env != "" && (ci == "local" || ci == "") {
		ci = env
	}

	// 2. Go runtime debug buildinfo
	if info, ok := debug.ReadBuildInfo(); ok {
		if (v == "dev" || v == "") && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if c == "unknown" || c == "" {
					c = setting.Value
				}
			case "vcs.time":
				if bt == "unknown" || bt == "" {
					bt = setting.Value
				}
			}
		}
	}

	// 3. Local git working tree fallback (if binary was built or run in a repo checkout)
	if c == "unknown" || c == "" {
		if out, err := exec.Command("git", "rev-parse", "HEAD").Output(); err == nil {
			c = strings.TrimSpace(string(out))
		}
	}
	if msg == "unknown" || msg == "" {
		if out, err := exec.Command("git", "log", "-1", "--pretty=%s").Output(); err == nil {
			msg = strings.TrimSpace(string(out))
		}
	}
	if bt == "unknown" || bt == "" {
		if out, err := exec.Command("git", "log", "-1", "--pretty=%cI").Output(); err == nil {
			bt = strings.TrimSpace(string(out))
		}
	}
	if v == "dev" || v == "" {
		if out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
			branch := strings.TrimSpace(string(out))
			if branch != "" && branch != "HEAD" {
				v = branch
			}
		}
	}

	return bot.BuildInfo{
		Version:       v,
		Commit:        c,
		CommitMessage: msg,
		BuildTime:     bt,
		CIRunNumber:   ci,
	}
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A second signal during graceful shutdown forces an immediate exit so a
	// stuck shutdown never wedges the container past its stop grace period.
	forceExit := make(chan os.Signal, 1)
	signal.Notify(forceExit, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(forceExit)
	go func() {
		<-ctx.Done()
		<-forceExit
		slog.Warn("second shutdown signal received; exiting immediately")
		os.Exit(130)
	}()

	if err := run(ctx); err != nil {
		slog.Error("bot stopped", "err", err)
		os.Exit(1)
	}
}

func healthcheck() error {
	// Honor a custom HTTP bind. The container healthcheck probes liveness
	// (/healthz), not readiness: a transient database outage must not restart
	// a process whose workers are designed to ride it out.
	addr := strings.TrimSpace(os.Getenv("HTTP_ADDR"))
	host := "127.0.0.1"
	switch {
	case addr == "":
		addr = ":8080"
	case strings.HasPrefix(addr, ":"):
		// port-only bind; probe loopback
	case strings.Contains(addr, ":"):
		bindHost, port, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("parse HTTP_ADDR %q: %w", addr, err)
		}
		addr = ":" + port
		if bindHost != "" && bindHost != "0.0.0.0" && bindHost != "::" {
			host = bindHost
		}
	default:
		return fmt.Errorf("HTTP_ADDR %q is not host:port", addr)
	}
	client := http.Client{
		Timeout: 4 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Get("http://" + net.JoinHostPort(host, strings.TrimPrefix(addr, ":")) + "/healthz")
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned HTTP %d", response.StatusCode)
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
	for _, warning := range cfg.Warnings {
		logger.Warn("configuration warning", "detail", warning)
	}

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

	migrateCtx, cancelMigrate := context.WithTimeout(parent, 10*time.Minute)
	err = database.Migrate(migrateCtx)
	cancelMigrate()
	if err != nil {
		return err
	}

	keys := make(map[string][]byte, len(cfg.Media.PreviousKeys)+1)
	keys[cfg.Media.EncryptionKeyID] = append([]byte(nil), cfg.Media.EncryptionKey...)
	for id, prevKey := range cfg.Media.PreviousKeys {
		keys[id] = append([]byte(nil), prevKey...)
	}
	keyring, err := secretcrypto.NewKeyring(cfg.Media.EncryptionKeyID, keys)
	for _, key := range keys {
		secretcrypto.Zero(key)
	}
	secretcrypto.Zero(cfg.Media.EncryptionKey)
	for _, prevKey := range cfg.Media.PreviousKeys {
		secretcrypto.Zero(prevKey)
	}
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
		DraftTTL:                       cfg.Whisper.DraftTTL,
		WhisperTTL:                     cfg.Whisper.DefaultTTL,
		ContentRetention:               cfg.Media.Retention,
		IngestLease:                    cfg.Media.DownloadTimeout + cfg.Telegram.RequestTimeout + 30*time.Second,
		OpenLease:                      telegram.EphemeralCallbackWindow + 5*time.Second,
		PublishLease:                   cfg.Whisper.PublishLeaseTimeout,
		EphemeralDeleteAfter:           cfg.Whisper.EphemeralDeleteAfter,
		MaxMediaBytes:                  cfg.Media.MaxBytes,
		MaxActiveDraftsPerUser:         cfg.Whisper.MaxActiveDraftsPerUser,
		MaxWhispersPerUserPerHour:      cfg.Whisper.MaxWhispersPerUserPerHour,
		MaxActiveGuestRequestsPerUser:  cfg.Whisper.MaxActiveGuestRequestsPerUser,
		MaxGuestRequestsPerUserPerHour: cfg.Whisper.MaxGuestRequestsPerUserPerHour,
		DefaultOneTime:                 cfg.Whisper.DefaultOneTime,
		ProtectContent:                 cfg.Whisper.ProtectContent,
		AllowedChatIDs:                 cfg.Whisper.AllowedChatIDs,
		OwnerIDs:                       cfg.Telegram.OwnerIDs,
		GuestModeEnabled:               cfg.Telegram.GuestModeEnabled,
	})
	if err != nil {
		return fmt.Errorf("initialize service: %w", err)
	}

	handler, err := bot.New(bot.Config{
		Service: useCases, Telegram: telegramClient, BotUsername: cfg.Telegram.BotUsername,
		MaxMediaBytes: cfg.Media.MaxBytes, MediaDownloadTimeout: cfg.Media.DownloadTimeout,
		RequestTimeout: cfg.Telegram.RequestTimeout, Logger: logger,
		BuildInfo: resolveBuildInfo(),
	})
	if err != nil {
		return fmt.Errorf("initialize Telegram handler: %w", err)
	}
	updateLease := maxDuration(3*time.Minute, cfg.Media.DownloadTimeout+3*cfg.Telegram.RequestTimeout+30*time.Second)
	processor, err := app.NewUpdateProcessor(store, handler, updateLease, app.ProcessorOptions{
		Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("initialize update processor: %w", err)
	}

	server := httpserver.New(httpserver.Config{
		Addr: cfg.HTTP.Addr, ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		ReadTimeout: cfg.HTTP.ReadTimeout, WriteTimeout: cfg.HTTP.WriteTimeout,
		IdleTimeout: cfg.HTTP.IdleTimeout, WebhookEnabled: cfg.Telegram.UpdateMode == config.UpdateModeWebhook,
		WebhookSecret: cfg.Telegram.WebhookSecret,
	}, database, processor, logger)

	ln, err := server.Listen()
	if err != nil {
		return fmt.Errorf("listen HTTP %s: %w", cfg.HTTP.Addr, err)
	}
	defer func() { _ = ln.Close() }()

	if err := configureTelegramTransport(parent, cfg, telegramClient); err != nil {
		return err
	}

	cleanup, err := app.NewCleanupWorkerWithOptions(
		store, app.CleanupWorkerOptions{
			Interval:                  cfg.Cleanup.Interval,
			BatchSize:                 cfg.Cleanup.BatchSize,
			ProcessedUpdateRetention:  cfg.Cleanup.ProcessedUpdateRetention,
			ObservedIdentityRetention: cfg.Cleanup.ObservedIdentityRetention,
			Notifier:                  handler,
		}, logger,
	)
	if err != nil {
		return fmt.Errorf("initialize cleanup worker: %w", err)
	}
	deleter, err := app.NewEphemeralDeleteWorker(store, telegramClient, cfg.Whisper.EphemeralDeleteInterval, logger)
	if err != nil {
		return fmt.Errorf("initialize ephemeral deletion worker: %w", err)
	}
	guestDeleter, err := app.NewGuestPrivateDeleteWorker(store, telegramClient, cfg.Whisper.EphemeralDeleteInterval, logger)
	if err != nil {
		return fmt.Errorf("initialize guest deletion worker: %w", err)
	}
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
	runnersList := make([]func(context.Context) error, 0, 6)
	runnersList = append(runnersList,
		func(context.Context) error { return server.Serve(ln) },
		cleanup.Run,
		deleter.Run,
		guestDeleter.Run,
		publisher.Run,
	)
	if cfg.Telegram.UpdateMode == config.UpdateModePolling {
		poller, err := app.NewPoller(
			telegramClient,
			processor,
			cfg.Telegram.PollTimeout,
			cfg.Telegram.RequestTimeout,
			logger,
		)
		if err != nil {
			return fmt.Errorf("initialize Telegram poller: %w", err)
		}
		runnersList = append(runnersList, poller.Run)
	}
	runnerNames := []string{"http server", "cleanup worker", "ephemeral deletion worker",
		"guest private deletion worker", "publication worker", "Telegram poller"}
	results := make(chan runnerResult, len(runnersList))
	var runners sync.WaitGroup
	for index, runner := range runnersList {
		name := runnerNames[index]
		runners.Add(1)
		go func() {
			defer runners.Done()
			results <- runnerResult{name: name, err: runner(runCtx)}
		}()
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
