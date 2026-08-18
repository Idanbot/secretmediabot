package bot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/idan/secretmediabot/internal/service"
	"github.com/idan/secretmediabot/internal/telegram"
)

func (h *Handler) PublishNext(ctx context.Context) (bool, error) {
	publication, err := h.service.ClaimNextPublication(ctx)
	if service.IsNoPublication(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, h.Publish(ctx, publication)
}

func (h *Handler) Publish(ctx context.Context, publication service.Publication) error {
	request := telegram.SendMessageRequest{
		ChatID:          publication.Whisper.SourceChatID,
		MessageThreadID: publication.Whisper.SourceThreadID,
		Text: fmt.Sprintf(
			"A secret whisper from %s is waiting for %s.",
			publication.Sender.DisplayName(), publication.Recipient.DisplayName(),
		),
		ProtectContent: publication.Whisper.ProtectContent,
		ReplyMarkup: &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{{{
			Text: "Open secret", CallbackData: publication.CallbackData,
		}}}},
	}
	requestCtx, cancelRequest := context.WithTimeout(ctx, h.requestTimeout)
	message, err := h.telegram.SendMessage(requestCtx, request)
	cancelRequest()
	if err != nil {
		retryAfter := publicationRetryDelay(err, publication.Whisper.PublishAttemptCount)
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		finishErr := h.service.FailPublication(
			finishCtx, publication, publicationErrorCode(err), retryAfter,
		)
		if telegram.IsPermanent(err) {
			h.notifySenderPublicationFailed(ctx, publication)
		}
		return errors.Join(err, finishErr)
	}
	if message.MessageID <= 0 {
		err = errors.New("telegram envelope response has no message ID")
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		finishErr := h.service.FailPublication(
			finishCtx, publication, "invalid_envelope_response", 5*time.Second,
		)
		return errors.Join(err, finishErr)
	}
	return h.completePublication(ctx, publication, message.MessageID)
}

func (h *Handler) notifySenderPublicationFailed(ctx context.Context, publication service.Publication) {
	senderID := publication.Whisper.SenderID
	if senderID == 0 {
		senderID = publication.Sender.TelegramUserID
	}
	if senderID == 0 {
		return
	}
	recipientName := publication.Recipient.DisplayName()
	if recipientName == "" {
		recipientName = "the recipient"
	}
	text := fmt.Sprintf(
		"I couldn't post the secret envelope for %s in the group (the group may have removed the bot or restricted posting permissions). The secret will not be delivered.",
		recipientName,
	)
	notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.requestTimeout)
	defer cancel()
	_, _ = h.telegram.SendMessage(notifyCtx, telegram.SendMessageRequest{
		ChatID: senderID,
		Text:   text,
	})
}

func (h *Handler) completePublication(ctx context.Context, publication service.Publication, messageID int64) error {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = h.service.CompletePublication(finishCtx, publication, messageID)
		if err == nil {
			return nil
		}
		if !wait(finishCtx, time.Duration(attempt+1)*50*time.Millisecond) {
			break
		}
	}
	return err
}

func publicationRetryDelay(err error, attempt int) time.Duration {
	var apiErr *telegram.APIError
	if errors.As(err, &apiErr) {
		if retry := apiErr.RetryAfter(); retry > 0 {
			return retry
		}
		if apiErr.RateLimited() {
			return 30 * time.Second
		}
		if apiErr.Permanent() {
			return 0
		}
	}
	if attempt < 1 {
		attempt = 1
	}
	shift := min(attempt-1, 6)
	return min(time.Duration(1<<shift)*5*time.Second, 5*time.Minute)
}

func publicationErrorCode(err error) string {
	var apiErr *telegram.APIError
	if errors.As(err, &apiErr) {
		if apiErr.RateLimited() {
			return "telegram_rate_limited"
		}
		if apiErr.Permanent() {
			return "telegram_rejected_envelope"
		}
		return "telegram_api_unavailable"
	}
	return "telegram_transport_unavailable"
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
