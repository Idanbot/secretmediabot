// Package httpserver exposes health probes and the optional Telegram webhook.
package httpserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/idan/secretmediabot/internal/telegram"
)

const (
	WebhookPath          = "/telegram/webhook"
	WebhookSecretHeader  = "X-Telegram-Bot-Api-Secret-Token"
	maxWebhookBodyBytes  = 1 << 20
	readinessPingTimeout = 2 * time.Second
)

type ReadinessChecker interface {
	Ping(context.Context) error
}

type UpdateProcessor interface {
	Process(context.Context, telegram.Update) error
}

type Config struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	WebhookEnabled    bool
	WebhookSecret     string
}

type Server struct {
	httpServer *http.Server
	logger     *slog.Logger
}

func New(cfg Config, readiness ReadinessChecker, updates UpdateProcessor, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if readiness == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), readinessPingTimeout)
		defer cancel()
		if err := readiness.Ping(ctx); err != nil {
			logger.WarnContext(r.Context(), "readiness check failed")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	if cfg.WebhookEnabled && updates != nil {
		mux.Handle(WebhookPath, webhookHandler(cfg.WebhookSecret, updates, logger))
	}

	return &Server{
		httpServer: &http.Server{
			Addr:              cfg.Addr,
			Handler:           securityHeaders(mux),
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
			ReadTimeout:       cfg.ReadTimeout,
			WriteTimeout:      cfg.WriteTimeout,
			IdleTimeout:       cfg.IdleTimeout,
		},
		logger: logger,
	}
}

func (s *Server) ListenAndServe() error {
	err := s.httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

func webhookHandler(secret string, updates UpdateProcessor, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !constantTimeEqual(r.Header.Get(WebhookSecretHeader), secret) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !IsWebhookContentType(r.Header.Get("Content-Type")) {
			http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)
		decoder := json.NewDecoder(r.Body)
		var update telegram.Update
		if err := decoder.Decode(&update); err != nil || update.UpdateID == 0 {
			http.Error(w, "invalid update", http.StatusBadRequest)
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			http.Error(w, "invalid update", http.StatusBadRequest)
			return
		}

		if err := updates.Process(r.Context(), update); err != nil {
			// Processor errors can contain repository or Telegram request details.
			// Keep webhook logs free of message and account identifiers.
			logger.ErrorContext(r.Context(), "webhook update processing failed")
			http.Error(w, "update failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func constantTimeEqual(got, want string) bool {
	if got == "" || want == "" || len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func IsWebhookContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}
