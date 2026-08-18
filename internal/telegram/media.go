package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path"
	"strconv"
	"strings"

	"github.com/idan/secretmediabot/internal/domain"
)

var (
	ErrNoSupportedMedia = errors.New("telegram: message has no supported media")
	ErrMultipleMedia    = errors.New("telegram: message contains multiple supported media values")
	ErrUnsupportedMedia = errors.New("telegram: unsupported media type")
)

type SendEphemeralMediaRequest struct {
	ChatID          int64
	MessageThreadID *int64
	ReceiverUserID  int64
	CallbackQueryID string
	Type            domain.MediaType
	FileID          string
	Caption         string
	ProtectContent  bool
}

// SendEphemeralMediaUploadRequest carries decrypted media which is uploaded
// multipart as an ephemeral response to a callback query. Used when Telegram
// permanently rejects the stored file_id and the retained bytes must be
// re-uploaded. Data is request-only and never included in errors or logs.
type SendEphemeralMediaUploadRequest struct {
	ChatID          int64
	MessageThreadID *int64
	ReceiverUserID  int64
	CallbackQueryID string
	Type            domain.MediaType
	Data            []byte
	FileName        string
	ContentType     string
	Caption         string
	ProtectContent  bool
}

type SendPrivateMediaByFileIDRequest struct {
	ChatID         int64
	Type           domain.MediaType
	FileID         string
	Caption        string
	ProtectContent bool
}

// SendPrivateMediaRequest contains decrypted media which will be uploaded to a
// private owner chat. Data and Caption are request-only values and are never
// included in errors or logged by this package.
type SendPrivateMediaRequest struct {
	ChatID         int64
	Type           domain.MediaType
	Data           []byte
	FileName       string
	ContentType    string
	Caption        string
	ProtectContent bool
}

// ExtractMedia converts exactly one supported Telegram message attachment to
// the domain's provider-neutral reference. Albums are intentionally handled by
// higher-level code one message at a time.
func ExtractMedia(message Message) (domain.MediaReference, error) {
	type candidate struct {
		mediaType   domain.MediaType
		fileID      string
		uniqueID    string
		contentType string
		size        int64
	}
	var found []candidate

	if len(message.Photo) > 0 {
		photo := largestPhoto(message.Photo)
		found = append(found, candidate{
			mediaType:   domain.MediaPhoto,
			fileID:      photo.FileID,
			uniqueID:    photo.FileUniqueID,
			contentType: "image/jpeg",
			size:        photo.FileSize,
		})
	}
	if message.Voice != nil {
		found = append(found, candidate{
			mediaType:   domain.MediaVoice,
			fileID:      message.Voice.FileID,
			uniqueID:    message.Voice.FileUniqueID,
			contentType: defaultString(message.Voice.MIMEType, "audio/ogg"),
			size:        message.Voice.FileSize,
		})
	}
	if message.Video != nil {
		found = append(found, candidate{
			mediaType:   domain.MediaVideo,
			fileID:      message.Video.FileID,
			uniqueID:    message.Video.FileUniqueID,
			contentType: defaultString(message.Video.MIMEType, "video/mp4"),
			size:        message.Video.FileSize,
		})
	}
	if message.Audio != nil {
		found = append(found, candidate{
			mediaType:   domain.MediaAudio,
			fileID:      message.Audio.FileID,
			uniqueID:    message.Audio.FileUniqueID,
			contentType: defaultString(message.Audio.MIMEType, "audio/mpeg"),
			size:        message.Audio.FileSize,
		})
	}
	if message.Document != nil {
		found = append(found, candidate{
			mediaType:   domain.MediaDocument,
			fileID:      message.Document.FileID,
			uniqueID:    message.Document.FileUniqueID,
			contentType: message.Document.MIMEType,
			size:        message.Document.FileSize,
		})
	}

	if len(found) == 0 {
		return domain.MediaReference{}, ErrNoSupportedMedia
	}
	if len(found) != 1 {
		return domain.MediaReference{}, ErrMultipleMedia
	}

	selected := found[0]
	reference := domain.MediaReference{
		Provider:    domain.MediaProviderTelegram,
		Type:        selected.mediaType,
		Ref:         selected.fileID,
		UniqueRef:   selected.uniqueID,
		ContentType: selected.contentType,
		SizeBytes:   selected.size,
	}
	if err := reference.Validate(); err != nil {
		return domain.MediaReference{}, fmt.Errorf("telegram: invalid extracted media: %w", err)
	}
	return reference, nil
}

// SendEphemeralMedia resends a Telegram file_id to one group member. It makes
// exactly one API request and returns the ID required by deleteEphemeralMessage.
func (c *Client) SendEphemeralMedia(ctx context.Context, req SendEphemeralMediaRequest) (int64, error) {
	method, field, err := mediaMethod(req.Type)
	if err != nil {
		return 0, err
	}
	if req.ChatID == 0 || req.ReceiverUserID <= 0 || strings.TrimSpace(req.FileID) == "" {
		return 0, fmt.Errorf("%w: ephemeral media requires chat ID, receiver user ID, and file ID", ErrInvalidArgument)
	}

	wire := struct {
		ChatID          int64  `json:"chat_id"`
		MessageThreadID *int64 `json:"message_thread_id,omitempty"`
		ReceiverUserID  int64  `json:"receiver_user_id"`
		CallbackQueryID string `json:"callback_query_id,omitempty"`
		Photo           string `json:"photo,omitempty"`
		Voice           string `json:"voice,omitempty"`
		Video           string `json:"video,omitempty"`
		Audio           string `json:"audio,omitempty"`
		Document        string `json:"document,omitempty"`
		Caption         string `json:"caption,omitempty"`
		ProtectContent  bool   `json:"protect_content,omitempty"`
	}{
		ChatID:          req.ChatID,
		MessageThreadID: req.MessageThreadID,
		ReceiverUserID:  req.ReceiverUserID,
		CallbackQueryID: req.CallbackQueryID,
		Caption:         req.Caption,
		ProtectContent:  req.ProtectContent,
	}
	switch field {
	case "photo":
		wire.Photo = req.FileID
	case "voice":
		wire.Voice = req.FileID
	case "video":
		wire.Video = req.FileID
	case "audio":
		wire.Audio = req.FileID
	case "document":
		wire.Document = req.FileID
	}

	var message Message
	if err := c.callJSON(ctx, method, wire, &message); err != nil {
		return 0, err
	}
	if message.EphemeralMessageID <= 0 {
		return 0, &ProtocolError{Method: method, Reason: "result has no ephemeral message ID"}
	}
	return message.EphemeralMessageID, nil
}

// SendPrivateMediaByFileID sends a stored Telegram file to a private chat. The
// file ID belongs to this bot, so no plaintext fallback upload is required.
func (c *Client) SendPrivateMediaByFileID(ctx context.Context, req SendPrivateMediaByFileIDRequest) (Message, error) {
	method, field, err := mediaMethod(req.Type)
	if err != nil {
		return Message{}, err
	}
	if req.ChatID <= 0 || strings.TrimSpace(req.FileID) == "" {
		return Message{}, fmt.Errorf("%w: private media requires chat ID and file ID", ErrInvalidArgument)
	}
	wire := struct {
		ChatID         int64  `json:"chat_id"`
		Photo          string `json:"photo,omitempty"`
		Voice          string `json:"voice,omitempty"`
		Video          string `json:"video,omitempty"`
		Audio          string `json:"audio,omitempty"`
		Document       string `json:"document,omitempty"`
		Caption        string `json:"caption,omitempty"`
		ProtectContent bool   `json:"protect_content,omitempty"`
	}{ChatID: req.ChatID, Caption: req.Caption, ProtectContent: req.ProtectContent}
	switch field {
	case "photo":
		wire.Photo = req.FileID
	case "voice":
		wire.Voice = req.FileID
	case "video":
		wire.Video = req.FileID
	case "audio":
		wire.Audio = req.FileID
	case "document":
		wire.Document = req.FileID
	}
	var message Message
	if err := c.callJSON(ctx, method, wire, &message); err != nil {
		return Message{}, err
	}
	if message.Chat.ID == 0 || message.MessageID <= 0 {
		return Message{}, &ProtocolError{Method: method, Reason: "result has no private message ID"}
	}
	return message, nil
}

// SendPrivateMedia uploads decrypted bytes to a private owner chat using
// multipart/form-data. The returned Message can be used by the caller to audit
// or delete the owner-review message.
func (c *Client) SendPrivateMedia(ctx context.Context, req SendPrivateMediaRequest) (Message, error) {
	method, field, err := mediaMethod(req.Type)
	if err != nil {
		return Message{}, err
	}
	if req.ChatID <= 0 || len(req.Data) == 0 {
		return Message{}, fmt.Errorf("%w: private media requires a private chat ID and non-empty data", ErrInvalidArgument)
	}
	if int64(len(req.Data)) > c.maxUploadBytes {
		return Message{}, &FileTooLargeError{Limit: c.maxUploadBytes, ReportedSize: int64(len(req.Data))}
	}

	fields := map[string]string{"chat_id": strconv.FormatInt(req.ChatID, 10)}
	body, contentTypeHeader, bodyErr := buildMultipartBody(method, fields, field, req, req.Data)
	if bodyErr != nil {
		return Message{}, bodyErr
	}
	// Zero the duplicated plaintext once the request completes, mirroring the
	// buffer hygiene applied to decrypted payloads elsewhere.
	payload := body.Bytes()
	defer func() { clear(payload); body.Reset() }()

	var message Message
	if err := c.call(ctx, method, contentTypeHeader, body, &message); err != nil {
		return Message{}, err
	}
	if message.Chat.ID == 0 {
		return Message{}, &ProtocolError{Method: method, Reason: "result has no chat"}
	}
	return message, nil
}

// SendEphemeralMediaUpload uploads decrypted bytes as an ephemeral media
// message visible only to the callback sender. It makes exactly one API
// request and returns the ephemeral message ID required by
// deleteEphemeralMessage.
func (c *Client) SendEphemeralMediaUpload(ctx context.Context, req SendEphemeralMediaUploadRequest) (int64, error) {
	method, field, err := mediaMethod(req.Type)
	if err != nil {
		return 0, err
	}
	if req.ChatID == 0 || req.ReceiverUserID <= 0 || len(req.Data) == 0 {
		return 0, fmt.Errorf("%w: ephemeral media upload requires chat ID, receiver user ID, and data", ErrInvalidArgument)
	}
	if int64(len(req.Data)) > c.maxUploadBytes {
		return 0, &FileTooLargeError{Limit: c.maxUploadBytes, ReportedSize: int64(len(req.Data))}
	}
	fields := map[string]string{
		"chat_id":           strconv.FormatInt(req.ChatID, 10),
		"receiver_user_id":  strconv.FormatInt(req.ReceiverUserID, 10),
		"callback_query_id": req.CallbackQueryID,
	}
	if req.MessageThreadID != nil {
		fields["message_thread_id"] = strconv.FormatInt(*req.MessageThreadID, 10)
	}
	body, contentTypeHeader, bodyErr := buildMultipartBody(method, fields, field, req, req.Data)
	if bodyErr != nil {
		return 0, bodyErr
	}
	payload := body.Bytes()
	defer func() { clear(payload); body.Reset() }()

	var message Message
	if err := c.call(ctx, method, contentTypeHeader, body, &message); err != nil {
		return 0, err
	}
	if message.EphemeralMessageID <= 0 {
		return 0, &ProtocolError{Method: method, Reason: "result has no ephemeral message ID"}
	}
	return message.EphemeralMessageID, nil
}

// multipartMedia describes the media part of a multipart upload.
type multipartMedia interface {
	multipartMetadata() (fileName, contentType, caption string, protectContent bool)
	mediaType() domain.MediaType
}

func (req SendPrivateMediaRequest) multipartMetadata() (string, string, string, bool) {
	return req.FileName, req.ContentType, req.Caption, req.ProtectContent
}

func (req SendPrivateMediaRequest) mediaType() domain.MediaType { return req.Type }

func (req SendEphemeralMediaUploadRequest) multipartMetadata() (string, string, string, bool) {
	return req.FileName, req.ContentType, req.Caption, req.ProtectContent
}

func (req SendEphemeralMediaUploadRequest) mediaType() domain.MediaType { return req.Type }

// buildMultipartBody assembles the multipart upload. The returned buffer is
// owned by the caller, which must Reset it after the request completes so the
// duplicated plaintext does not linger on the heap.
func buildMultipartBody(method string, fields map[string]string, mediaField string, media multipartMedia, data []byte) (*bytes.Buffer, string, error) {
	fileName, contentType, caption, protectContent := media.multipartMetadata()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, value := range fields {
		if value == "" {
			continue
		}
		if err := writer.WriteField(name, value); err != nil {
			return nil, "", &RequestError{Method: method, cause: errors.New("could not encode multipart metadata")}
		}
	}
	if caption != "" {
		if err := writer.WriteField("caption", caption); err != nil {
			return nil, "", &RequestError{Method: method, cause: errors.New("could not encode multipart metadata")}
		}
	}
	if protectContent {
		if err := writer.WriteField("protect_content", "true"); err != nil {
			return nil, "", &RequestError{Method: method, cause: errors.New("could not encode multipart metadata")}
		}
	}

	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		contentType = http.DetectContentType(data)
	} else {
		parsedContentType, _, parseErr := mime.ParseMediaType(contentType)
		if parseErr != nil || parsedContentType == "" {
			return nil, "", fmt.Errorf("%w: media content type is malformed", ErrInvalidArgument)
		}
		contentType = parsedContentType
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
		"name":     mediaField,
		"filename": safeFilename(fileName, media.mediaType()),
	}))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", &RequestError{Method: method, cause: errors.New("could not encode multipart media")}
	}
	if _, err := part.Write(data); err != nil {
		return nil, "", &RequestError{Method: method, cause: errors.New("could not encode multipart media")}
	}
	if err := writer.Close(); err != nil {
		return nil, "", &RequestError{Method: method, cause: errors.New("could not finish multipart media")}
	}
	return &body, writer.FormDataContentType(), nil
}

func mediaMethod(mediaType domain.MediaType) (method, field string, err error) {
	switch mediaType {
	case domain.MediaPhoto:
		return "sendPhoto", "photo", nil
	case domain.MediaVoice:
		return "sendVoice", "voice", nil
	case domain.MediaVideo:
		return "sendVideo", "video", nil
	case domain.MediaAudio:
		return "sendAudio", "audio", nil
	case domain.MediaDocument:
		return "sendDocument", "document", nil
	default:
		return "", "", fmt.Errorf("%w: %q", ErrUnsupportedMedia, mediaType)
	}
}

func largestPhoto(photos []PhotoSize) PhotoSize {
	largest := photos[0]
	for _, photo := range photos[1:] {
		largestArea := int64(largest.Width) * int64(largest.Height)
		area := int64(photo.Width) * int64(photo.Height)
		if area > largestArea || area == largestArea && photo.FileSize > largest.FileSize {
			largest = photo
		}
	}
	return largest
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func safeFilename(value string, mediaType domain.MediaType) string {
	value = strings.ReplaceAll(value, "\\", "/")
	value = strings.ReplaceAll(value, "\x00", "")
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", "")
	value = strings.TrimSpace(path.Base(value))
	if value != "" && value != "." && value != "/" {
		return value
	}
	switch mediaType {
	case domain.MediaPhoto:
		return "photo.jpg"
	case domain.MediaVoice:
		return "voice.ogg"
	case domain.MediaVideo:
		return "video.mp4"
	case domain.MediaAudio:
		return "audio.mp3"
	default:
		return "document.bin"
	}
}
