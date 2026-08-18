package telegram_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/idan/secretmediabot/internal/telegram"
)

func TestTelegramChaosRateLimiting(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{
			"ok": false,
			"error_code": 429,
			"description": "Too Many Requests: retry after 3",
			"parameters": {
				"retry_after": 3
			}
		}`))
	}))
	defer server.Close()

	client, err := telegram.NewClient(telegram.ClientConfig{
		Token:   "dummy_token",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewClient error = %v", err)
	}

	_, err = client.GetMe(context.Background())
	if err == nil {
		t.Fatal("expected 429 error, got nil")
	}

	var apiErr *telegram.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *telegram.APIError, got %T: %v", err, err)
	}

	if !apiErr.RateLimited() {
		t.Errorf("apiErr.RateLimited() = false, want true")
	}
	if apiErr.Permanent() {
		t.Errorf("apiErr.Permanent() = true, want false for 429")
	}
	if got := apiErr.RetryAfter(); got != 3*time.Second {
		t.Errorf("apiErr.RetryAfter() = %v, want 3s", got)
	}
}

func TestTelegramChaosTransientServerErrors(t *testing.T) {
	t.Parallel()

	for _, statusCode := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(statusCode)
			_, _ = w.Write([]byte(`{"ok": false, "error_code": ` + http.StatusText(statusCode) + `}`))
		}))

		client, err := telegram.NewClient(telegram.ClientConfig{
			Token:   "dummy_token",
			BaseURL: server.URL,
		})
		if err != nil {
			t.Fatalf("NewClient error = %v", err)
		}

		_, err = client.GetMe(context.Background())
		server.Close()

		var apiErr *telegram.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("status %d: expected APIError, got %T: %v", statusCode, err, err)
		}
		if apiErr.Permanent() {
			t.Errorf("status %d: apiErr.Permanent() = true, want false (transient)", statusCode)
		}
	}
}

func TestTelegramChaosTruncatedDownloadFails(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("short")) // 5 bytes written instead of 100 expected
	}))
	defer server.Close()

	client, err := telegram.NewClient(telegram.ClientConfig{
		Token:   "dummy_token",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewClient error = %v", err)
	}

	_, err = client.DownloadFile(context.Background(), "voice/file_1.oga", 100)
	if err == nil {
		t.Fatal("expected download to fail on size mismatch / truncated stream")
	}
	if !errors.Is(err, telegram.ErrIncompleteDownload) {
		t.Fatalf("expected ErrIncompleteDownload, got %v", err)
	}
}
