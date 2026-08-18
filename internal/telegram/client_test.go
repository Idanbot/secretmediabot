package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testToken = "123456:TOP_SECRET_TOKEN"

func TestGetMeAndGetUpdates(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		switch r.URL.Path {
		case "/bot" + testToken + "/getMe":
			writeJSON(t, w, map[string]any{
				"ok": true,
				"result": map[string]any{
					"id": 99, "is_bot": true, "first_name": "Secret", "username": "secret_bot",
				},
			})
		case "/bot" + testToken + "/getUpdates":
			var body struct {
				Offset         int64    `json:"offset"`
				Limit          int      `json:"limit"`
				Timeout        int      `json:"timeout"`
				AllowedUpdates []string `json:"allowed_updates"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode getUpdates request: %v", err)
				return
			}
			if body.Offset != 41 || body.Limit != 12 || body.Timeout != 2 || len(body.AllowedUpdates) != 2 {
				t.Errorf("unexpected getUpdates request: %+v", body)
			}
			writeJSON(t, w, map[string]any{
				"ok": true,
				"result": []any{map[string]any{
					"update_id": 42,
					"message": map[string]any{
						"message_id": 7,
						"date":       1,
						"chat":       map[string]any{"id": -1001, "type": "supergroup"},
						"text":       "/whisper",
					},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := mustClient(t, server.URL, DefaultMaxDownloadSize)
	bot, err := client.GetMe(context.Background())
	if err != nil {
		t.Fatalf("GetMe() error = %v", err)
	}
	if bot.ID != 99 || bot.Username != "secret_bot" {
		t.Fatalf("GetMe() = %+v", bot)
	}

	updates, err := client.GetUpdates(context.Background(), GetUpdatesRequest{
		Offset:         41,
		Limit:          12,
		Timeout:        1500 * time.Millisecond,
		AllowedUpdates: []string{"message", "callback_query"},
	})
	if err != nil {
		t.Fatalf("GetUpdates() error = %v", err)
	}
	if len(updates) != 1 || updates[0].UpdateID != 42 || updates[0].Message == nil || updates[0].Message.Text != "/whisper" {
		t.Fatalf("GetUpdates() = %+v", updates)
	}
}

func TestAPIErrorIsTypedAndRedacted(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		writeJSON(t, w, map[string]any{
			"ok":          false,
			"error_code":  429,
			"description": "token " + testToken + " must stay secret",
			"parameters":  map[string]any{"retry_after": 3},
		})
	}))
	defer server.Close()

	client := mustClient(t, server.URL, DefaultMaxDownloadSize)
	_, err := client.GetMe(context.Background())
	if err == nil {
		t.Fatal("GetMe() unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaked token: %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.ErrorCode != 429 || apiErr.StatusCode != http.StatusTooManyRequests || apiErr.RetryAfter() != 3*time.Second {
		t.Fatalf("APIError = %+v", apiErr)
	}
	if strings.Contains(apiErr.Description, testToken) {
		t.Fatalf("description leaked token: %q", apiErr.Description)
	}
}

func TestTransportErrorRedactsRequestURL(t *testing.T) {
	t.Parallel()

	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial failed for %s", req.URL)
	})}
	client, err := NewClient(ClientConfig{
		Token:      testToken,
		BaseURL:    "https://telegram.invalid",
		HTTPClient: httpClient,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetMe(context.Background())
	if err == nil {
		t.Fatal("GetMe() unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("transport error leaked token: %v", err)
	}
	var requestErr *RequestError
	if !errors.As(err, &requestErr) {
		t.Fatalf("error type = %T, want *RequestError", err)
	}
}

func TestSuccessfulCallsRequireResultAndTrue(t *testing.T) {
	t.Parallel()

	responses := map[string]string{
		"/bot" + testToken + "/setWebhook":    `{"ok":true,"result":false}`,
		"/bot" + testToken + "/deleteWebhook": `{"ok":true}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responses[r.URL.Path])
	}))
	defer server.Close()
	client := mustClient(t, server.URL, DefaultMaxDownloadSize)

	if err := client.SetWebhook(context.Background(), SetWebhookRequest{URL: "https://bot.example/webhook"}); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("SetWebhook() error = %v, want ErrInvalidResponse", err)
	}
	if err := client.DeleteWebhook(context.Background(), DeleteWebhookRequest{}); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("DeleteWebhook() error = %v, want ErrInvalidResponse", err)
	}
}

func TestEphemeralTextAndControlMethods(t *testing.T) {
	t.Parallel()

	seen := make(map[string]map[string]any)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode control request: %v", err)
			return
		}
		seen[r.URL.Path] = body
		switch r.URL.Path {
		case "/bot" + testToken + "/sendMessage":
			writeJSON(t, w, map[string]any{
				"ok": true,
				"result": map[string]any{
					"message_id": 0, "ephemeral_message_id": 777, "date": 1,
					"chat": map[string]any{"id": -1001, "type": "supergroup"},
				},
			})
		default:
			writeJSON(t, w, map[string]any{"ok": true, "result": true})
		}
	}))
	defer server.Close()
	client := mustClient(t, server.URL, DefaultMaxDownloadSize)

	eid, err := client.SendEphemeralText(context.Background(), SendEphemeralTextRequest{
		ChatID: -1001, ReceiverUserID: 55, CallbackQueryID: "callback-1",
		Text: "private text", ProtectContent: true,
	})
	if err != nil || eid != 777 {
		t.Fatalf("SendEphemeralText() = %d, %v", eid, err)
	}
	textBody := seen["/bot"+testToken+"/sendMessage"]
	if textBody["receiver_user_id"] != float64(55) || textBody["callback_query_id"] != "callback-1" || textBody["protect_content"] != true {
		t.Fatalf("sendMessage body = %#v", textBody)
	}

	if err := client.AnswerCallbackQuery(context.Background(), AnswerCallbackQueryRequest{CallbackQueryID: "callback-1"}); err != nil {
		t.Fatalf("AnswerCallbackQuery() error = %v", err)
	}
	if err := client.DeleteEphemeralMessage(context.Background(), DeleteEphemeralMessageRequest{
		ChatID: -1001, ReceiverUserID: 55, EphemeralMessageID: eid,
	}); err != nil {
		t.Fatalf("DeleteEphemeralMessage() error = %v", err)
	}
	deleteBody := seen["/bot"+testToken+"/deleteEphemeralMessage"]
	if deleteBody["ephemeral_message_id"] != float64(777) || deleteBody["receiver_user_id"] != float64(55) {
		t.Fatalf("delete body = %#v", deleteBody)
	}

	if err := client.SetWebhook(context.Background(), SetWebhookRequest{
		URL: "https://bot.example/telegram/webhook", SecretToken: "webhook-secret",
		AllowedUpdates: []string{"message", "callback_query"}, MaxConnections: 40,
	}); err != nil {
		t.Fatalf("SetWebhook() error = %v", err)
	}
	if err := client.DeleteWebhook(context.Background(), DeleteWebhookRequest{DropPendingUpdates: true}); err != nil {
		t.Fatalf("DeleteWebhook() error = %v", err)
	}
	if seen["/bot"+testToken+"/setWebhook"]["secret_token"] != "webhook-secret" {
		t.Fatal("setWebhook omitted secret token")
	}
}

func TestGuestInlineAndPrivateControlMethods(t *testing.T) {
	t.Parallel()

	seen := make(map[string]map[string]any)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode guest request: %v", err)
			return
		}
		seen[r.URL.Path] = body
		switch r.URL.Path {
		case "/bot" + testToken + "/answerGuestQuery":
			writeJSON(t, w, map[string]any{"ok": true, "result": map[string]any{"inline_message_id": "inline-1"}})
		default:
			writeJSON(t, w, map[string]any{"ok": true, "result": true})
		}
	}))
	defer server.Close()
	client := mustClient(t, server.URL, DefaultMaxDownloadSize)

	sent, err := client.AnswerGuestQuery(context.Background(), AnswerGuestQueryRequest{
		GuestQueryID: "guest-query",
		Result: InlineQueryResultArticle{
			Type: "article", ID: "request-1", Title: "Locked secret",
			InputMessageContent: InputTextMessageContent{MessageText: "Locked secret"},
		},
	})
	if err != nil || sent.InlineMessageID != "inline-1" {
		t.Fatalf("AnswerGuestQuery() = %#v, %v", sent, err)
	}
	guestBody := seen["/bot"+testToken+"/answerGuestQuery"]
	result := guestBody["result"].(map[string]any)
	if guestBody["guest_query_id"] != "guest-query" || result["type"] != "article" {
		t.Fatalf("guest body = %#v", guestBody)
	}

	if err := client.AnswerInlineQuery(context.Background(), AnswerInlineQueryRequest{
		InlineQueryID: "inline-query", IsPersonal: true,
		Results: []InlineQueryResultArticle{{Type: "article", ID: "request-1", Title: "Locked", InputMessageContent: InputTextMessageContent{MessageText: "Locked"}}},
	}); err != nil {
		t.Fatalf("AnswerInlineQuery() error = %v", err)
	}
	if err := client.SetMyCommands(context.Background(), SetMyCommandsRequest{Commands: []BotCommand{{Command: "whisper", Description: "Whisper", IsEphemeral: true}}}); err != nil {
		t.Fatalf("SetMyCommands() error = %v", err)
	}
	if err := client.DeleteMessage(context.Background(), DeleteMessageRequest{ChatID: 202, MessageID: 303}); err != nil {
		t.Fatalf("DeleteMessage() error = %v", err)
	}
	if seen["/bot"+testToken+"/deleteMessage"]["chat_id"] != float64(202) || seen["/bot"+testToken+"/deleteMessage"]["message_id"] != float64(303) {
		t.Fatalf("delete body = %#v", seen["/bot"+testToken+"/deleteMessage"])
	}
}

func TestGetChatMemberAndGetFile(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bot" + testToken + "/getChatMember":
			writeJSON(t, w, map[string]any{
				"ok": true,
				"result": map[string]any{
					"status": "member", "user": map[string]any{"id": 88, "is_bot": false, "first_name": "Recipient"},
				},
			})
		case "/bot" + testToken + "/getFile":
			writeJSON(t, w, map[string]any{
				"ok": true,
				"result": map[string]any{
					"file_id": "file-id", "file_unique_id": "unique", "file_size": 4, "file_path": "voice/file.ogg",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := mustClient(t, server.URL, 4)

	member, err := client.GetChatMember(context.Background(), GetChatMemberRequest{ChatID: -1001, UserID: 88})
	if err != nil || member.Status != "member" || member.User.ID != 88 {
		t.Fatalf("GetChatMember() = %+v, %v", member, err)
	}
	file, err := client.GetFile(context.Background(), GetFileRequest{FileID: "file-id"})
	if err != nil || file.FilePath != "voice/file.ogg" || file.FileSize != 4 {
		t.Fatalf("GetFile() = %+v, %v", file, err)
	}
}

func TestDownloadFileEnforcesHeaderAndActualByteLimit(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/file/bot" + testToken + "/ok/file.bin":
			w.Header().Set("Content-Length", "4")
			_, _ = io.WriteString(w, "data")
		case "/file/bot" + testToken + "/large/header.bin":
			w.Header().Set("Content-Length", "5")
			_, _ = io.WriteString(w, "12345")
		case "/file/bot" + testToken + "/large/chunked.bin":
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			_, _ = io.WriteString(w, "12345")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := mustClient(t, server.URL, 4)

	data, err := client.DownloadFile(context.Background(), "ok/file.bin", 0)
	if err != nil || string(data) != "data" {
		t.Fatalf("DownloadFile(ok) = %q, %v", data, err)
	}
	for _, filePath := range []string{"large/header.bin", "large/chunked.bin"} {
		if _, err := client.DownloadFile(context.Background(), filePath, 0); !errors.Is(err, ErrFileTooLarge) {
			t.Errorf("DownloadFile(%q) error = %v, want ErrFileTooLarge", filePath, err)
		}
	}
	if _, err := client.DownloadFile(context.Background(), "../token", 0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unsafe path error = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func mustClient(t *testing.T, baseURL string, maxDownload int64) *Client {
	t.Helper()
	client, err := NewClient(ClientConfig{
		Token:            testToken,
		BaseURL:          baseURL,
		HTTPClient:       &http.Client{Timeout: 5 * time.Second},
		MaxDownloadBytes: maxDownload,
		MaxUploadBytes:   maxDownload,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return client
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func TestGetUpdatesQuarantinesMalformedUpdates(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"ok": true,
			"result": []any{
				map[string]any{
					"update_id": 10,
					// message.date is a string instead of an integer: this update
					// fails to decode but its update_id must still be recoverable.
					"message": map[string]any{"message_id": 1, "date": "not-a-number", "chat": map[string]any{"id": 1, "type": "private"}},
				},
				map[string]any{
					"update_id": 11,
					"message":   map[string]any{"message_id": 2, "date": 5, "chat": map[string]any{"id": 1, "type": "private"}, "text": "hello"},
				},
				// Not even update_id decodes: dropped entirely.
				"totally-unexpected",
			},
		})
	}))
	defer server.Close()
	client := mustClient(t, server.URL, 1<<20)

	updates, err := client.GetUpdates(context.Background(), GetUpdatesRequest{})
	if err != nil {
		t.Fatalf("GetUpdates() error = %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("GetUpdates() = %+v, want 2 surviving updates", updates)
	}
	if updates[0].UpdateID != 10 || updates[0].Message != nil {
		t.Errorf("malformed update should keep only its ID, got %+v", updates[0])
	}
	if updates[1].UpdateID != 11 || updates[1].Message == nil {
		t.Errorf("healthy update should decode fully, got %+v", updates[1])
	}
}

func TestClientRefusesToFollowRedirects(t *testing.T) {
	t.Parallel()

	var redirectTarget *httptest.Server
	redirectTarget = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// If the client ever followed the redirect, the secret body would land
		// here and the test would observe a request.
		t.Error("client followed a cross-host redirect carrying the request body")
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", redirectTarget.URL+"/collect")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := mustClient(t, server.URL, 1<<20)

	_, err := client.SendMessage(context.Background(), SendMessageRequest{ChatID: 1, Text: "secret"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("SendMessage() redirect error = %v, want APIError", err)
	}
	if apiErr.StatusCode < 300 || apiErr.StatusCode >= 400 {
		t.Errorf("redirect surfaced as HTTP %d, want a 3xx status", apiErr.StatusCode)
	}
}

func TestDownloadFileVerifiesExpectedSize(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("truncated-but-cleanly-closed"))
	}))
	defer server.Close()
	client := mustClient(t, server.URL, 1<<20)

	data, err := client.DownloadFile(context.Background(), "ok/file.bin", 1024)
	if !errors.Is(err, ErrIncompleteDownload) {
		t.Fatalf("DownloadFile() = %q, %v; want ErrIncompleteDownload", data, err)
	}
	if _, err := client.DownloadFile(context.Background(), "ok/file.bin", int64(len("truncated-but-cleanly-closed"))); err != nil {
		t.Fatalf("DownloadFile() with matching size error = %v", err)
	}
}

func TestAPIErrorPredicates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		err        *APIError
		permanent  bool
		rateLimit  bool
		retryAfter time.Duration
	}{
		{"flood with retry-after", &APIError{ErrorCode: 429, Parameters: &ResponseParameters{RetryAfter: 12}}, false, true, 12 * time.Second},
		{"proxy html 429", &APIError{StatusCode: 429}, false, true, 0},
		{"bad request", &APIError{ErrorCode: 400}, true, false, 0},
		{"unauthorized", &APIError{StatusCode: 401}, true, false, 0},
		{"server error", &APIError{ErrorCode: 500}, false, false, 0},
		{"transport style", &APIError{Method: "sendMessage"}, false, false, 0},
	}
	for _, tc := range cases {
		if got := tc.err.Permanent(); got != tc.permanent {
			t.Errorf("%s: Permanent() = %v, want %v", tc.name, got, tc.permanent)
		}
		if got := tc.err.RateLimited(); got != tc.rateLimit {
			t.Errorf("%s: RateLimited() = %v, want %v", tc.name, got, tc.rateLimit)
		}
		if got := tc.err.RetryAfter(); got != tc.retryAfter {
			t.Errorf("%s: RetryAfter() = %v, want %v", tc.name, got, tc.retryAfter)
		}
	}
	if IsPermanent(&APIError{ErrorCode: 400}) != true || IsRateLimited(&APIError{ErrorCode: 429}) != true {
		t.Error("package-level predicate helpers misclassified")
	}
	if IsPermanent(errors.New("boom")) || IsRateLimited(nil) {
		t.Error("non-API errors must not classify as permanent or rate-limited")
	}
}
