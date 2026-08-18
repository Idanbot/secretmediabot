package httpserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/idan/secretmediabot/internal/app"
	"github.com/idan/secretmediabot/internal/telegram"
)

type readinessStub struct{ err error }

func (r readinessStub) Ping(context.Context) error { return r.err }

type processorStub struct {
	updates []telegram.Update
	err     error
}

func (p *processorStub) Process(_ context.Context, update telegram.Update) error {
	p.updates = append(p.updates, update)
	return p.err
}

func TestHealthAndReadiness(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ready := New(Config{}, readinessStub{}, nil, logger)
	response := httptest.NewRecorder()
	ready.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("ready status = %d", response.Code)
	}

	notReady := New(Config{}, readinessStub{err: errors.New("db down")}, nil, logger)
	response = httptest.NewRecorder()
	notReady.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready status = %d", response.Code)
	}

	health := httptest.NewRecorder()
	ready.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}

	metricsResp := httptest.NewRecorder()
	ready.Handler().ServeHTTP(metricsResp, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metricsResp.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", metricsResp.Code)
	}
	if contentType := metricsResp.Header().Get("Content-Type"); !strings.Contains(contentType, "text/plain") {
		t.Fatalf("metrics content type = %q, want text/plain", contentType)
	}
}

func TestWebhookAuthenticationAndDispatch(t *testing.T) {
	t.Parallel()
	processor := &processorStub{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(Config{WebhookEnabled: true, WebhookSecret: "a-long-secret"}, readinessStub{}, processor, logger)

	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(`{"update_id":42}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(`{"update_id":42}`))
	request.Header.Set(WebhookSecretHeader, "a-long-secret")
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("webhook status = %d, body = %q", response.Code, response.Body.String())
	}
	if len(processor.updates) != 1 || processor.updates[0].UpdateID != 42 {
		t.Fatalf("processed updates = %+v", processor.updates)
	}
}

func TestWebhookRejectsNonJSONContent(t *testing.T) {
	t.Parallel()

	processor := &processorStub{}
	server := New(
		Config{WebhookEnabled: true, WebhookSecret: "a-long-secret"},
		readinessStub{},
		processor,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	request := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(`{"update_id":42}`))
	request.Header.Set(WebhookSecretHeader, "a-long-secret")
	request.Header.Set("Content-Type", "text/plain")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("webhook status = %d, want %d", response.Code, http.StatusUnsupportedMediaType)
	}
	if len(processor.updates) != 0 {
		t.Fatalf("processed updates = %+v, want none", processor.updates)
	}
}

func TestWebhookReturnsRetryableFailureForBusyUpdateAndRedactsDetails(t *testing.T) {
	t.Parallel()

	const sensitive = "secret-media user_id=987654"
	processor := &processorStub{err: errors.Join(app.ErrUpdateBusy, errors.New(sensitive))}
	var logs bytes.Buffer
	server := New(
		Config{WebhookEnabled: true, WebhookSecret: "a-long-secret"},
		readinessStub{},
		processor,
		slog.New(slog.NewTextHandler(&logs, nil)),
	)
	request := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(`{"update_id":987654}`))
	request.Header.Set(WebhookSecretHeader, "a-long-secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("webhook status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	if output := logs.String(); strings.Contains(output, sensitive) || strings.Contains(output, "987654") {
		t.Fatalf("webhook log leaked processor details or update ID: %q", output)
	}
}

func TestIsWebhookContentType(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"application/json", "application/json; charset=utf-8", "APPLICATION/JSON"} {
		if !IsWebhookContentType(value) {
			t.Errorf("IsWebhookContentType(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"", "text/plain", "application/jsonp", "application/json; broken"} {
		if IsWebhookContentType(value) {
			t.Errorf("IsWebhookContentType(%q) = true, want false", value)
		}
	}
}
