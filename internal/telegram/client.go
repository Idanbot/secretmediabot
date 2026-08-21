package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/idan/secretmediabot/internal/metrics"
)

const (
	DefaultAPIBaseURL      = "https://api.telegram.org"
	DefaultMaxDownloadSize = int64(20 << 20)
	defaultHTTPTimeout     = 60 * time.Second
	maxAPIResponseBytes    = int64(32 << 20)
	maxInlineQueryResults  = 50
)

var (
	ErrInvalidArgument    = errors.New("telegram: invalid argument")
	ErrInvalidResponse    = errors.New("telegram: invalid API response")
	ErrFileTooLarge       = errors.New("telegram: file exceeds configured size limit")
	ErrIncompleteDownload = errors.New("telegram: downloaded bytes do not match the reported file size")
)

// APIError is returned when Telegram responds with ok=false or a non-2xx HTTP
// status. Description is redacted before it is stored so formatting the error
// cannot expose the bot token.
type APIError struct {
	Method      string
	StatusCode  int
	ErrorCode   int
	Description string
	Parameters  *ResponseParameters
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := "telegram " + e.Method + " failed"
	if e.ErrorCode != 0 {
		message += " (API " + strconv.Itoa(e.ErrorCode) + ")"
	} else if e.StatusCode != 0 {
		message += " (HTTP " + strconv.Itoa(e.StatusCode) + ")"
	}
	if e.Description != "" {
		message += ": " + e.Description
	}
	return message
}

// RetryAfter returns Telegram's requested retry delay, if one was supplied.
func (e *APIError) RetryAfter() time.Duration {
	if e == nil || e.Parameters == nil || e.Parameters.RetryAfter <= 0 {
		return 0
	}
	return time.Duration(e.Parameters.RetryAfter) * time.Second
}

// RateLimited reports whether Telegram rejected the request as too frequent.
// It honors both the documented error code and a Retry-After parameter
// arriving without one, as produced by some proxies.
func (e *APIError) RateLimited() bool {
	if e == nil {
		return false
	}
	return e.ErrorCode == 429 || e.StatusCode == 429 || e.RetryAfter() > 0
}

// Permanent reports whether retrying an identical request can never succeed.
// All non-rate-limited 4xx responses are permanent; 5xx and transport errors
// are transient. This is the canonical classification for the whole
// application — callers must not re-implement it.
func (e *APIError) Permanent() bool {
	if e == nil || e.RateLimited() {
		return false
	}
	code := e.ErrorCode
	if code == 0 {
		code = e.StatusCode
	}
	return code >= 400 && code < 500
}

// IsPermanent classifies an arbitrary error returned by this package.
func IsPermanent(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Permanent()
}

// IsRateLimited classifies an arbitrary error returned by this package.
func IsRateLimited(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.RateLimited()
}

// RequestError describes a failure before Telegram returned an API response.
// Its cause is either a context error or a sanitized error string.
type RequestError struct {
	Method string
	cause  error
}

func (e *RequestError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.cause == nil {
		return "telegram " + e.Method + " request failed"
	}
	return "telegram " + e.Method + " request failed: " + e.cause.Error()
}

func (e *RequestError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

type ProtocolError struct {
	Method string
	Reason string
}

func (e *ProtocolError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Reason == "" {
		return "telegram " + e.Method + ": " + ErrInvalidResponse.Error()
	}
	return "telegram " + e.Method + ": " + ErrInvalidResponse.Error() + ": " + e.Reason
}

func (e *ProtocolError) Is(target error) bool { return target == ErrInvalidResponse }

type FileTooLargeError struct {
	Limit        int64
	ReportedSize int64
}

func (e *FileTooLargeError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.ReportedSize > 0 {
		return fmt.Sprintf("%s: limit=%d reported=%d", ErrFileTooLarge, e.Limit, e.ReportedSize)
	}
	return fmt.Sprintf("%s: limit=%d", ErrFileTooLarge, e.Limit)
}

func (e *FileTooLargeError) Is(target error) bool { return target == ErrFileTooLarge }

type ClientConfig struct {
	Token            string
	BaseURL          string
	HTTPClient       *http.Client
	MaxDownloadBytes int64
	MaxUploadBytes   int64
}

type Client struct {
	token            string
	baseURL          *url.URL
	httpClient       *http.Client
	maxDownloadBytes int64
	maxUploadBytes   int64
}

func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Token == "" || strings.ContainsAny(cfg.Token, "/\r\n\t ") {
		return nil, fmt.Errorf("%w: bot token is missing or malformed", ErrInvalidArgument)
	}

	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = DefaultAPIBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("%w: API base URL must be an absolute HTTP(S) URL without credentials, query, or fragment", ErrInvalidArgument)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	// The Bot API never legitimately redirects, and 307/308 preserve the
	// request body — following one would forward plaintext secret payloads to
	// an attacker-chosen host. Enforce redirect refusal on every client,
	// including injected ones, unless the caller deliberately set a policy.
	if httpClient.CheckRedirect == nil {
		cloned := new(http.Client)
		*cloned = *httpClient
		cloned.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
		httpClient = cloned
	}

	maxDownload := cfg.MaxDownloadBytes
	if maxDownload == 0 {
		maxDownload = DefaultMaxDownloadSize
	}
	if maxDownload < 1 || maxDownload == math.MaxInt64 {
		return nil, fmt.Errorf("%w: maximum download size must be positive and bounded", ErrInvalidArgument)
	}
	maxUpload := cfg.MaxUploadBytes
	if maxUpload == 0 {
		maxUpload = maxDownload
	}
	if maxUpload < 1 {
		return nil, fmt.Errorf("%w: maximum upload size must be positive", ErrInvalidArgument)
	}

	return &Client{
		token:            cfg.Token,
		baseURL:          parsed,
		httpClient:       httpClient,
		maxDownloadBytes: maxDownload,
		maxUploadBytes:   maxUpload,
	}, nil
}

func (c *Client) GetMe(ctx context.Context) (User, error) {
	var user User
	if err := c.callJSON(ctx, "getMe", struct{}{}, &user); err != nil {
		return User{}, err
	}
	if user.ID == 0 || !user.IsBot {
		return User{}, &ProtocolError{Method: "getMe", Reason: "result is not a bot user"}
	}
	return user, nil
}

func (c *Client) GetUpdates(ctx context.Context, req GetUpdatesRequest) ([]Update, error) {
	if req.Limit < 0 || req.Limit > 100 || req.Timeout < 0 || req.Timeout > 50*time.Second {
		return nil, fmt.Errorf("%w: getUpdates limit or timeout is outside Telegram's range", ErrInvalidArgument)
	}
	timeoutSeconds := 0
	if req.Timeout > 0 {
		timeoutSeconds = int((req.Timeout + time.Second - 1) / time.Second)
	}
	wire := struct {
		Offset         int64    `json:"offset,omitempty"`
		Limit          int      `json:"limit,omitempty"`
		Timeout        int      `json:"timeout,omitempty"`
		AllowedUpdates []string `json:"allowed_updates,omitempty"`
	}{
		Offset:         req.Offset,
		Limit:          req.Limit,
		Timeout:        timeoutSeconds,
		AllowedUpdates: req.AllowedUpdates,
	}
	var rawUpdates []json.RawMessage
	if err := c.callJSON(ctx, "getUpdates", wire, &rawUpdates); err != nil {
		return nil, err
	}
	// Decode each update individually: one malformed entry must never wedge
	// polling for the whole batch. When the update_id survives decoding, emit
	// a shell update so the processor can acknowledge it and the polling
	// offset advances; entries without a decodable ID are dropped and skipped
	// by the next successfully acknowledged update.
	updates := make([]Update, 0, len(rawUpdates))
	for _, raw := range rawUpdates {
		var update Update
		if err := json.Unmarshal(raw, &update); err != nil {
			var idOnly struct {
				UpdateID int64 `json:"update_id"`
			}
			if json.Unmarshal(raw, &idOnly) == nil && idOnly.UpdateID > 0 {
				updates = append(updates, Update{UpdateID: idOnly.UpdateID})
			}
			continue
		}
		updates = append(updates, update)
	}
	return updates, nil
}

func (c *Client) SendMessage(ctx context.Context, req SendMessageRequest) (Message, error) {
	if req.ChatID == 0 || req.Text == "" {
		return Message{}, fmt.Errorf("%w: sendMessage requires chat ID and text", ErrInvalidArgument)
	}
	var message Message
	if err := c.callJSON(ctx, "sendMessage", req, &message); err != nil {
		return Message{}, err
	}
	if message.Chat.ID == 0 || (message.MessageID <= 0 && message.EphemeralMessageID <= 0) {
		return Message{}, &ProtocolError{Method: "sendMessage", Reason: "result has no chat or message ID"}
	}
	return message, nil
}

func (c *Client) EditMessageText(ctx context.Context, req EditMessageTextRequest) (Message, error) {
	if req.ChatID == 0 || req.MessageID <= 0 || req.Text == "" {
		return Message{}, fmt.Errorf("%w: editMessageText requires chat ID, message ID, and text", ErrInvalidArgument)
	}
	var message Message
	if err := c.callJSON(ctx, "editMessageText", req, &message); err != nil {
		return Message{}, err
	}
	return message, nil
}

func (c *Client) SendEphemeralText(ctx context.Context, req SendEphemeralTextRequest) (int64, error) {
	if req.ReceiverUserID <= 0 {
		return 0, fmt.Errorf("%w: ephemeral text requires a receiver user ID", ErrInvalidArgument)
	}
	message, err := c.SendMessage(ctx, SendMessageRequest{
		ChatID:          req.ChatID,
		MessageThreadID: req.MessageThreadID,
		ReceiverUserID:  req.ReceiverUserID,
		CallbackQueryID: req.CallbackQueryID,
		Text:            req.Text,
		ProtectContent:  req.ProtectContent,
		ReplyMarkup:     req.ReplyMarkup,
	})
	if err != nil {
		return 0, err
	}
	if message.EphemeralMessageID <= 0 {
		return 0, &ProtocolError{Method: "sendMessage", Reason: "result has no ephemeral message ID"}
	}
	return message.EphemeralMessageID, nil
}

func (c *Client) AnswerCallbackQuery(ctx context.Context, req AnswerCallbackQueryRequest) error {
	if req.CallbackQueryID == "" {
		return fmt.Errorf("%w: callback query ID is required", ErrInvalidArgument)
	}
	return c.callTrue(ctx, "answerCallbackQuery", req)
}

func (c *Client) AnswerGuestQuery(ctx context.Context, req AnswerGuestQueryRequest) (SentGuestMessage, error) {
	if req.GuestQueryID == "" || validateInlineArticle(req.Result) != nil {
		return SentGuestMessage{}, fmt.Errorf("%w: answerGuestQuery requires a guest query and article result", ErrInvalidArgument)
	}
	var message SentGuestMessage
	if err := c.callJSON(ctx, "answerGuestQuery", req, &message); err != nil {
		return SentGuestMessage{}, err
	}
	if message.InlineMessageID == "" {
		return SentGuestMessage{}, &ProtocolError{Method: "answerGuestQuery", Reason: "result has no inline message ID"}
	}
	return message, nil
}

func (c *Client) AnswerInlineQuery(ctx context.Context, req AnswerInlineQueryRequest) error {
	if req.InlineQueryID == "" || len(req.Results) == 0 || len(req.Results) > maxInlineQueryResults || req.CacheTime < 0 {
		return fmt.Errorf("%w: answerInlineQuery requires an inline query and at least one result", ErrInvalidArgument)
	}
	seen := make(map[string]struct{}, len(req.Results))
	for _, result := range req.Results {
		if err := validateInlineArticle(result); err != nil {
			return fmt.Errorf("%w: answerInlineQuery result: %v", ErrInvalidArgument, err)
		}
		if _, exists := seen[result.ID]; exists {
			return fmt.Errorf("%w: answerInlineQuery result IDs must be unique", ErrInvalidArgument)
		}
		seen[result.ID] = struct{}{}
	}
	return c.callTrue(ctx, "answerInlineQuery", req)
}

func validateInlineArticle(result InlineQueryResultArticle) error {
	if result.Type != "article" || result.ID == "" || result.Title == "" || result.InputMessageContent.MessageText == "" {
		return errors.New("article result is missing type, ID, title, or message content")
	}
	return nil
}

func (c *Client) GetChatMember(ctx context.Context, req GetChatMemberRequest) (ChatMember, error) {
	if req.ChatID == 0 || req.UserID <= 0 {
		return ChatMember{}, fmt.Errorf("%w: getChatMember requires chat and user IDs", ErrInvalidArgument)
	}
	var member ChatMember
	if err := c.callJSON(ctx, "getChatMember", req, &member); err != nil {
		return ChatMember{}, err
	}
	if member.User.ID == 0 || member.Status == "" {
		return ChatMember{}, &ProtocolError{Method: "getChatMember", Reason: "result is missing member identity or status"}
	}
	return member, nil
}

func (c *Client) GetFile(ctx context.Context, req GetFileRequest) (File, error) {
	if strings.TrimSpace(req.FileID) == "" {
		return File{}, fmt.Errorf("%w: file ID is required", ErrInvalidArgument)
	}
	var file File
	if err := c.callJSON(ctx, "getFile", req, &file); err != nil {
		return File{}, err
	}
	if file.FileID == "" || file.FilePath == "" {
		return File{}, &ProtocolError{Method: "getFile", Reason: "result has no reusable file ID or download path"}
	}
	if file.FileSize > c.maxDownloadBytes {
		return File{}, &FileTooLargeError{Limit: c.maxDownloadBytes, ReportedSize: file.FileSize}
	}
	return file, nil
}

// DownloadFile downloads an already resolved Telegram file_path. Both the
// declared Content-Length and bytes actually read are checked, and when the
// caller supplies the size reported by getFile the received length must match
// it exactly — a cleanly closed but truncated body is otherwise silently
// accepted. LimitReader is deliberately used even when Content-Length looks
// safe because that header is not an integrity boundary.
func (c *Client) DownloadFile(ctx context.Context, filePath string, expectedSize int64) ([]byte, error) {
	if !validFilePath(filePath) {
		return nil, fmt.Errorf("%w: file path is missing or unsafe", ErrInvalidArgument)
	}
	if expectedSize < 0 {
		return nil, fmt.Errorf("%w: expected size must not be negative", ErrInvalidArgument)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.fileEndpoint(filePath).String(), nil)
	if err != nil {
		return nil, &RequestError{Method: "downloadFile", cause: c.safeCause(ctx, err)}
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &RequestError{Method: "downloadFile", cause: c.safeCause(ctx, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &APIError{
			Method:      "downloadFile",
			StatusCode:  resp.StatusCode,
			Description: "file endpoint returned a non-success status",
		}
	}
	if resp.ContentLength > c.maxDownloadBytes {
		return nil, &FileTooLargeError{Limit: c.maxDownloadBytes, ReportedSize: resp.ContentLength}
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxDownloadBytes+1))
	if err != nil {
		return nil, &RequestError{Method: "downloadFile", cause: c.safeCause(ctx, err)}
	}
	if int64(len(data)) > c.maxDownloadBytes {
		return nil, &FileTooLargeError{Limit: c.maxDownloadBytes}
	}
	if expectedSize > 0 && int64(len(data)) != expectedSize {
		return nil, fmt.Errorf("%w: expected=%d received=%d", ErrIncompleteDownload, expectedSize, len(data))
	}
	return data, nil
}

func (c *Client) SetWebhook(ctx context.Context, req SetWebhookRequest) error {
	if strings.TrimSpace(req.URL) == "" {
		return fmt.Errorf("%w: webhook URL is required", ErrInvalidArgument)
	}
	if req.MaxConnections < 0 || req.MaxConnections > 100 {
		return fmt.Errorf("%w: webhook max connections must be between 1 and 100 when supplied", ErrInvalidArgument)
	}
	return c.callTrue(ctx, "setWebhook", req)
}

func (c *Client) DeleteWebhook(ctx context.Context, req DeleteWebhookRequest) error {
	return c.callTrue(ctx, "deleteWebhook", req)
}

func (c *Client) SetMyCommands(ctx context.Context, req SetMyCommandsRequest) error {
	if len(req.Commands) == 0 {
		return fmt.Errorf("%w: at least one bot command is required", ErrInvalidArgument)
	}
	return c.callTrue(ctx, "setMyCommands", req)
}

func (c *Client) DeleteEphemeralMessage(ctx context.Context, req DeleteEphemeralMessageRequest) error {
	if req.ChatID == 0 || req.ReceiverUserID <= 0 || req.EphemeralMessageID <= 0 {
		return fmt.Errorf("%w: deleting an ephemeral message requires chat, receiver, and ephemeral message IDs", ErrInvalidArgument)
	}
	return c.callTrue(ctx, "deleteEphemeralMessage", req)
}

func (c *Client) DeleteMessage(ctx context.Context, req DeleteMessageRequest) error {
	if req.ChatID == 0 || req.MessageID <= 0 {
		return fmt.Errorf("%w: deleting a message requires chat and message IDs", ErrInvalidArgument)
	}
	return c.callTrue(ctx, "deleteMessage", req)
}

func (c *Client) callTrue(ctx context.Context, method string, request any) error {
	var result bool
	if err := c.callJSON(ctx, method, request, &result); err != nil {
		return err
	}
	if !result {
		return &ProtocolError{Method: method, Reason: "result was false"}
	}
	return nil
}

func (c *Client) callJSON(ctx context.Context, method string, request, result any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return &RequestError{Method: method, cause: errors.New("could not encode request")}
	}
	return c.call(ctx, method, "application/json", bytes.NewReader(body), result)
}

func (c *Client) call(ctx context.Context, method, contentType string, body io.Reader, result any) error {
	started := time.Now()
	err := c.doCall(ctx, method, contentType, body, result)
	recordRequest(method, started, err)
	return err
}

func recordRequest(method string, started time.Time, err error) {
	outcome := "ok"
	switch {
	case err == nil:
	case errors.Is(err, ErrInvalidResponse):
		outcome = "protocol_error"
	default:
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			if apiErr.RateLimited() {
				outcome = "rate_limited"
			} else {
				outcome = "api_error"
			}
		} else {
			outcome = "request_error"
		}
	}
	requestCount.Add(1, method, outcome)
	requestDuration.Add(int64(time.Since(started).Microseconds()), method)
}

var (
	requestCount = metrics.Counter(
		"telegram_api_requests_total", "Telegram Bot API requests by method and outcome.", "method", "outcome")
	requestDuration = metrics.Counter(
		"telegram_api_request_duration_microseconds_total", "Cumulative Telegram Bot API request duration by method.", "method")
)

func (c *Client) doCall(ctx context.Context, method, contentType string, body io.Reader, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodEndpoint(method).String(), body)
	if err != nil {
		return &RequestError{Method: method, cause: c.safeCause(ctx, err)}
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &RequestError{Method: method, cause: c.safeCause(ctx, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.ContentLength > maxAPIResponseBytes {
		return &ProtocolError{Method: method, Reason: "response exceeds safety limit"}
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes+1))
	if err != nil {
		return &RequestError{Method: method, cause: c.safeCause(ctx, err)}
	}
	if int64(len(payload)) > maxAPIResponseBytes {
		return &ProtocolError{Method: method, Reason: "response exceeds safety limit"}
	}

	var envelope struct {
		OK          bool                `json:"ok"`
		Result      json.RawMessage     `json:"result"`
		ErrorCode   int                 `json:"error_code"`
		Description string              `json:"description"`
		Parameters  *ResponseParameters `json:"parameters"`
	}
	decoded := json.Unmarshal(payload, &envelope)
	if decoded != nil {
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return &APIError{Method: method, StatusCode: resp.StatusCode, Description: "Telegram returned a non-JSON error response"}
		}
		return &ProtocolError{Method: method, Reason: "response is not valid JSON"}
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices || !envelope.OK {
		description := c.redact(envelope.Description)
		if description == "" && (resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices) {
			description = "Telegram returned a non-success status"
		}
		return &APIError{
			Method:      method,
			StatusCode:  resp.StatusCode,
			ErrorCode:   envelope.ErrorCode,
			Description: description,
			Parameters:  envelope.Parameters,
		}
	}
	if len(envelope.Result) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Result), []byte("null")) {
		return &ProtocolError{Method: method, Reason: "successful response has no result"}
	}
	if result == nil {
		return &ProtocolError{Method: method, Reason: "caller supplied no result destination"}
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return &ProtocolError{Method: method, Reason: "result has an unexpected shape"}
	}
	return nil
}

func (c *Client) methodEndpoint(method string) *url.URL {
	u := *c.baseURL
	u.Path = c.baseURL.Path + "/bot" + c.token + "/" + method
	u.RawPath = ""
	return &u
}

func (c *Client) fileEndpoint(filePath string) *url.URL {
	u := *c.baseURL
	u.Path = c.baseURL.Path + "/file/bot" + c.token + "/" + strings.TrimLeft(filePath, "/")
	u.RawPath = ""
	return &u
}

func (c *Client) safeCause(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err == nil {
		return nil
	}
	return errors.New(c.redact(err.Error()))
}

func (c *Client) redact(value string) string {
	if value == "" {
		return ""
	}
	redacted := strings.ReplaceAll(value, c.token, "[REDACTED]")
	redacted = strings.ReplaceAll(redacted, url.PathEscape(c.token), "[REDACTED]")
	redacted = strings.ReplaceAll(redacted, url.QueryEscape(c.token), "[REDACTED]")
	return redacted
}

func validFilePath(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\r\n") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
