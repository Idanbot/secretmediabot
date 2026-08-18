package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/idan/secretmediabot/internal/telegram"
)

type RecordedCall struct {
	Method string
	Header http.Header
	Body   []byte
}

type TelegramMockServer struct {
	Server   *httptest.Server
	BaseURL  string
	BotToken string

	mu        sync.Mutex
	messageID atomic.Int64
	calls     []RecordedCall

	SentMessages       []telegram.SendMessageRequest
	AnsweredCallbacks  []telegram.AnswerCallbackQueryRequest
	DeletedMessages    []telegram.DeleteMessageRequest
	DeletedEphemerals  []telegram.DeleteEphemeralMessageRequest
	CustomFilePayloads map[string][]byte
}

func NewTelegramMockServer(botUsername string) *TelegramMockServer {
	mock := &TelegramMockServer{
		BotToken:           "mock_bot_token_12345",
		CustomFilePayloads: make(map[string][]byte),
	}
	mock.messageID.Store(1000)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		parts := strings.Split(path, "/")
		if len(parts) == 0 {
			http.NotFound(w, r)
			return
		}

		// Handle file downloads
		if parts[0] == "file" && len(parts) >= 3 {
			filePath := strings.Join(parts[2:], "/")
			mock.mu.Lock()
			payload, ok := mock.CustomFilePayloads[filePath]
			mock.mu.Unlock()
			if ok {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(payload)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("mock-media-binary-data-content"))
			return
		}

		method := parts[len(parts)-1]
		body, _ := io.ReadAll(r.Body)

		mock.mu.Lock()
		mock.calls = append(mock.calls, RecordedCall{
			Method: method,
			Header: r.Header.Clone(),
			Body:   body,
		})
		mock.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")

		switch method {
		case "getMe":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"id":                          999888777,
					"is_bot":                      true,
					"first_name":                  "Secret Media Bot",
					"username":                    botUsername,
					"can_join_groups":             true,
					"can_read_all_group_messages": true,
					"supports_inline_queries":     true,
					"supports_guest_queries":      true,
				},
			})

		case "setMyCommands", "setWebhook", "deleteWebhook":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": true,
			})

		case "sendMessage":
			var req telegram.SendMessageRequest
			_ = json.Unmarshal(body, &req)
			msgID := mock.messageID.Add(1)

			mock.mu.Lock()
			mock.SentMessages = append(mock.SentMessages, req)
			mock.mu.Unlock()

			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"message_id":           msgID,
					"ephemeral_message_id": msgID,
					"chat": map[string]any{
						"id":   req.ChatID,
						"type": "supergroup",
					},
					"text": req.Text,
				},
			})

		case "sendPhoto", "sendVoice", "sendVideo", "sendAudio", "sendDocument":
			msgID := mock.messageID.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"message_id":           msgID,
					"ephemeral_message_id": msgID,
					"chat": map[string]any{
						"id":   100,
						"type": "supergroup",
					},
				},
			})

		case "answerCallbackQuery":
			var req telegram.AnswerCallbackQueryRequest
			_ = json.Unmarshal(body, &req)
			mock.mu.Lock()
			mock.AnsweredCallbacks = append(mock.AnsweredCallbacks, req)
			mock.mu.Unlock()

			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": true,
			})

		case "deleteMessage":
			var req telegram.DeleteMessageRequest
			_ = json.Unmarshal(body, &req)
			mock.mu.Lock()
			mock.DeletedMessages = append(mock.DeletedMessages, req)
			mock.mu.Unlock()

			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": true,
			})

		case "deleteEphemeralMessage":
			var req telegram.DeleteEphemeralMessageRequest
			_ = json.Unmarshal(body, &req)
			mock.mu.Lock()
			mock.DeletedEphemerals = append(mock.DeletedEphemerals, req)
			mock.mu.Unlock()

			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": true,
			})

		case "getFile":
			var req telegram.GetFileRequest
			_ = json.Unmarshal(body, &req)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"file_id":        req.FileID,
					"file_unique_id": "unique_" + req.FileID,
					"file_size":      30,
					"file_path":      fmt.Sprintf("media/%s.dat", req.FileID),
				},
			})

		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": true,
			})
		}
	})

	mock.Server = httptest.NewServer(mux)
	mock.BaseURL = mock.Server.URL
	return mock
}

func (m *TelegramMockServer) Close() {
	m.Server.Close()
}
