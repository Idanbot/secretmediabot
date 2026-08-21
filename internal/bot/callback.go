package bot

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/secretcrypto"
	"github.com/idan/secretmediabot/internal/service"
	"github.com/idan/secretmediabot/internal/telegram"
)

const ephemeralSendTimeout = 12 * time.Second

func (h *Handler) handleCallback(ctx context.Context, callback telegram.CallbackQuery) error {
	if callback.ID == "" || callback.From.ID <= 0 {
		return nil
	}
	if strings.HasPrefix(callback.Data, ownerCallbackPrefix) {
		return h.handleOwnerCallback(ctx, callback)
	}
	if callback.Message == nil || callback.Message.Chat.ID == 0 {
		return h.answerCallback(ctx, callback.ID, "This whisper cannot be opened here.", true)
	}

	deliveryCtx, cancel := context.WithTimeout(ctx, ephemeralSendTimeout)
	defer cancel()
	delivery, err := h.service.ReserveOpen(deliveryCtx, callback.Data, callback.From.ID, callback.ID)
	if err != nil {
		text, alert := openErrorMessage(err)
		return h.answerCallback(deliveryCtx, callback.ID, text, alert)
	}
	defer delivery.Content.Zero()

	ephemeralID, err := h.sendDelivery(deliveryCtx, delivery, callback.From.ID)
	if err != nil {
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		finishErr := h.service.FailOpen(finishCtx, delivery, "telegram_delivery_failed")
		finishCancel()
		answerErr := h.answerCallback(ctx, callback.ID, "Delivery failed. Press the button again to retry.", true)
		if finishErr != nil {
			return errors.Join(finishErr, answerErr)
		}
		// The recipient explicitly retries with a new callback. Automatically
		// replaying an ambiguous Telegram transport error could duplicate a
		// supposedly one-time delivery.
		return answerErr
	}

	if err := h.completeOpen(ctx, delivery, ephemeralID); err != nil {
		_ = h.answerCallback(ctx, callback.ID, "Telegram accepted the secret; finalization is still pending.", true)
		return err
	}
	return h.answerCallback(ctx, callback.ID, "Telegram accepted the secret for this app. It should appear in this group.", false)
}

func (h *Handler) sendDelivery(
	ctx context.Context,
	delivery service.OpenDelivery,
	recipientID int64,
) (int64, error) {
	switch delivery.Content.Kind {
	case domain.PayloadText:
		return h.telegram.SendEphemeralText(ctx, telegram.SendEphemeralTextRequest{
			ChatID: delivery.Whisper.SourceChatID, MessageThreadID: delivery.Whisper.SourceThreadID,
			ReceiverUserID: recipientID, CallbackQueryID: delivery.CallbackQueryID,
			Text: string(delivery.Content.Text), ProtectContent: delivery.Whisper.ProtectContent,
		})
	case domain.PayloadMedia:
		if delivery.Content.Media == nil {
			return 0, errors.New("reserved media delivery has no media reference")
		}
		ephemeralID, err := h.telegram.SendEphemeralMedia(ctx, telegram.SendEphemeralMediaRequest{
			ChatID: delivery.Whisper.SourceChatID, MessageThreadID: delivery.Whisper.SourceThreadID,
			ReceiverUserID: recipientID, CallbackQueryID: delivery.CallbackQueryID,
			Type: delivery.Content.Media.Type, FileID: delivery.Content.Media.TelegramFileID,
			Caption: string(delivery.Content.Caption), ProtectContent: delivery.Whisper.ProtectContent,
		})
		if err == nil {
			return ephemeralID, nil
		}
		// A permanently dead file_id (revoked resend) falls back to uploading
		// the retained decrypted bytes. Transient errors retry normally.
		if !telegram.IsPermanent(err) {
			return 0, err
		}
		return h.sendDeliveryFallback(ctx, delivery, recipientID)
	default:
		return 0, errors.New("reserved delivery has unsupported payload kind")
	}
}

func (h *Handler) sendDeliveryFallback(
	ctx context.Context,
	delivery service.OpenDelivery,
	recipientID int64,
) (int64, error) {
	data, mediaType, contentType, err := h.service.WhisperMediaFallback(ctx, delivery.Whisper.ID)
	if err != nil {
		return 0, err
	}
	defer secretcrypto.Zero(data)
	if int64(len(data)) != delivery.Content.Media.PlaintextSize {
		return 0, errors.New("retained media size mismatch")
	}
	return h.telegram.SendEphemeralMediaUpload(ctx, telegram.SendEphemeralMediaUploadRequest{
		ChatID: delivery.Whisper.SourceChatID, MessageThreadID: delivery.Whisper.SourceThreadID,
		ReceiverUserID: recipientID, CallbackQueryID: delivery.CallbackQueryID,
		Type: mediaType, Data: data, ContentType: contentType,
		Caption: string(delivery.Content.Caption), ProtectContent: delivery.Whisper.ProtectContent,
	})
}

func (h *Handler) completeOpen(ctx context.Context, delivery service.OpenDelivery, ephemeralID int64) error {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Second)
	defer cancel()
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		err = h.service.CompleteOpen(finishCtx, delivery, ephemeralID)
		if err == nil {
			return nil
		}
		if !wait(finishCtx, time.Duration(attempt+1)*75*time.Millisecond) {
			break
		}
	}
	return err
}

func (h *Handler) answerCallback(ctx context.Context, id, text string, alert bool) error {
	answerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	err := h.telegram.AnswerCallbackQuery(answerCtx, telegram.AnswerCallbackQueryRequest{
		CallbackQueryID: id, Text: text, ShowAlert: alert, CacheTime: 0,
	})
	// A stale or unknown callback query is already handled server-side; no
	// need to surface it to the user.
	if telegram.IsPermanent(err) {
		return nil
	}
	return err
}

func openErrorMessage(err error) (string, bool) {
	switch {
	case errors.Is(err, service.ErrWrongRecipient):
		return "This secret is for someone else.", true
	case errors.Is(err, service.ErrWhisperExpired):
		return "This secret has expired.", true
	case errors.Is(err, service.ErrWhisperRevoked):
		return "This secret was revoked.", true
	case errors.Is(err, service.ErrWhisperAlreadyOpened):
		return "This one-time secret was already delivered.", true
	case errors.Is(err, service.ErrWhisperUnavailable):
		return "This secret is not available right now.", true
	case errors.Is(err, service.ErrInvalidOpenToken), errors.Is(err, service.ErrWhisperNotFound):
		return "This whisper link is invalid or no longer available.", true
	default:
		return "Could not open this secret. Try again shortly.", true
	}
}
