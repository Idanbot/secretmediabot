package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/idan/secretmediabot/internal/domain"
)

func TestExtractMedia(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		message    Message
		wantType   domain.MediaType
		wantFileID string
		wantUnique string
		wantMIME   string
		wantSize   int64
	}{
		{
			name: "largest photo",
			message: Message{Photo: []PhotoSize{
				{FileID: "small", FileUniqueID: "u-small", Width: 90, Height: 90, FileSize: 10},
				{FileID: "large", FileUniqueID: "u-large", Width: 1280, Height: 720, FileSize: 200},
			}},
			wantType: domain.MediaPhoto, wantFileID: "large", wantUnique: "u-large", wantMIME: "image/jpeg", wantSize: 200,
		},
		{
			name:     "voice defaults MIME",
			message:  Message{Voice: &Voice{FileID: "voice", FileUniqueID: "uv", FileSize: 22}},
			wantType: domain.MediaVoice, wantFileID: "voice", wantUnique: "uv", wantMIME: "audio/ogg", wantSize: 22,
		},
		{
			name:     "video",
			message:  Message{Video: &Video{FileID: "video", FileUniqueID: "uvi", MIMEType: "video/webm", FileSize: 33}},
			wantType: domain.MediaVideo, wantFileID: "video", wantUnique: "uvi", wantMIME: "video/webm", wantSize: 33,
		},
		{
			name:     "audio",
			message:  Message{Audio: &Audio{FileID: "audio", FileUniqueID: "ua", MIMEType: "audio/m4a", FileSize: 44}},
			wantType: domain.MediaAudio, wantFileID: "audio", wantUnique: "ua", wantMIME: "audio/m4a", wantSize: 44,
		},
		{
			name:     "document",
			message:  Message{Document: &Document{FileID: "document", FileUniqueID: "ud", MIMEType: "application/pdf", FileSize: 55}},
			wantType: domain.MediaDocument, wantFileID: "document", wantUnique: "ud", wantMIME: "application/pdf", wantSize: 55,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ExtractMedia(tt.message)
			if err != nil {
				t.Fatalf("ExtractMedia() error = %v", err)
			}
			if got.Provider != domain.MediaProviderTelegram || got.Type != tt.wantType || got.Ref != tt.wantFileID ||
				got.UniqueRef != tt.wantUnique || got.ContentType != tt.wantMIME || got.SizeBytes != tt.wantSize {
				t.Fatalf("ExtractMedia() = %+v", got)
			}
		})
	}
}

func TestExtractMediaRejectsMissingAndAmbiguousMedia(t *testing.T) {
	t.Parallel()

	if _, err := ExtractMedia(Message{Text: "hello"}); !errors.Is(err, ErrNoSupportedMedia) {
		t.Fatalf("missing media error = %v", err)
	}
	if _, err := ExtractMedia(Message{
		Voice: &Voice{FileID: "voice"},
		Audio: &Audio{FileID: "audio"},
	}); !errors.Is(err, ErrMultipleMedia) {
		t.Fatalf("ambiguous media error = %v", err)
	}
}

func TestSendEphemeralMediaByFileID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		mediaType domain.MediaType
		method    string
		field     string
	}{
		{domain.MediaPhoto, "sendPhoto", "photo"},
		{domain.MediaVoice, "sendVoice", "voice"},
		{domain.MediaVideo, "sendVideo", "video"},
		{domain.MediaAudio, "sendAudio", "audio"},
		{domain.MediaDocument, "sendDocument", "document"},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		method := strings.TrimPrefix(r.URL.Path, "/bot"+testToken+"/")
		var expectedField string
		for _, tt := range tests {
			if tt.method == method {
				expectedField = tt.field
				break
			}
		}
		if expectedField == "" {
			t.Errorf("unexpected method path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if body[expectedField] != "stored-file-id" || body["caption"] != "optional secret caption" {
			t.Errorf("media body = %#v", body)
		}
		if body["receiver_user_id"] != float64(88) || body["callback_query_id"] != "callback-id" || body["protect_content"] != true {
			t.Errorf("ephemeral fields = %#v", body)
		}
		writeJSON(t, w, map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": 0, "ephemeral_message_id": 901, "date": 1,
				"chat": map[string]any{"id": -1001, "type": "supergroup"},
			},
		})
	}))
	defer server.Close()
	client := mustClient(t, server.URL, DefaultMaxDownloadSize)

	for _, tt := range tests {
		tt := tt
		t.Run(string(tt.mediaType), func(t *testing.T) {
			id, err := client.SendEphemeralMedia(context.Background(), SendEphemeralMediaRequest{
				ChatID: -1001, ReceiverUserID: 88, CallbackQueryID: "callback-id",
				Type: tt.mediaType, FileID: "stored-file-id", Caption: "optional secret caption", ProtectContent: true,
			})
			if err != nil || id != 901 {
				t.Fatalf("SendEphemeralMedia() = %d, %v", id, err)
			}
		})
	}
}

func TestEphemeralMediaRequiresEphemeralResultID(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": 12, "date": 1,
				"chat": map[string]any{"id": -1001, "type": "supergroup"},
			},
		})
	}))
	defer server.Close()
	client := mustClient(t, server.URL, DefaultMaxDownloadSize)

	_, err := client.SendEphemeralMedia(context.Background(), SendEphemeralMediaRequest{
		ChatID: -1001, ReceiverUserID: 88, CallbackQueryID: "callback-id",
		Type: domain.MediaVoice, FileID: "stored-file-id",
	})
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("error = %v, want ErrInvalidResponse", err)
	}
}

func TestSendPrivateMediaMultipart(t *testing.T) {
	t.Parallel()

	secretCaption := "owner-only caption"
	secretBytes := []byte("decrypted-media")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot"+testToken+"/sendVoice" {
			t.Errorf("path = %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		if r.FormValue("chat_id") != "123" || r.FormValue("caption") != secretCaption || r.FormValue("protect_content") != "true" {
			t.Errorf("multipart fields = %#v", r.MultipartForm.Value)
		}
		file, header, err := r.FormFile("voice")
		if err != nil {
			t.Errorf("FormFile: %v", err)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			t.Errorf("read multipart file: %v", err)
			return
		}
		if string(data) != string(secretBytes) || header.Filename != "recording.ogg" || header.Header.Get("Content-Type") != "audio/ogg" {
			t.Errorf("multipart file name=%q type=%q data=%q", header.Filename, header.Header.Get("Content-Type"), data)
		}
		writeJSON(t, w, map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": 44, "date": 1,
				"chat":  map[string]any{"id": 123, "type": "private"},
				"voice": map[string]any{"file_id": "owner-copy", "file_unique_id": "owner-unique", "duration": 1},
			},
		})
	}))
	defer server.Close()
	client := mustClient(t, server.URL, int64(len(secretBytes)))

	message, err := client.SendPrivateMedia(context.Background(), SendPrivateMediaRequest{
		ChatID: 123, Type: domain.MediaVoice, Data: secretBytes,
		FileName: "../../recording.ogg", ContentType: "audio/ogg",
		Caption: secretCaption, ProtectContent: true,
	})
	if err != nil {
		t.Fatalf("SendPrivateMedia() error = %v", err)
	}
	if message.MessageID != 44 || message.Chat.ID != 123 || message.Voice == nil || message.Voice.FileID != "owner-copy" {
		t.Fatalf("SendPrivateMedia() = %+v", message)
	}

	_, err = client.SendPrivateMedia(context.Background(), SendPrivateMediaRequest{
		ChatID: 123, Type: domain.MediaVoice, Data: append(secretBytes, '!'), Caption: secretCaption,
	})
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("oversize error = %v", err)
	}
	if strings.Contains(err.Error(), secretCaption) || strings.Contains(err.Error(), string(secretBytes)) {
		t.Fatalf("error exposed private media data: %v", err)
	}
}
