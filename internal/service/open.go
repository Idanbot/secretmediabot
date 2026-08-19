package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/secretcrypto"
)

type PlaintextContent struct {
	Kind       domain.PayloadKind
	Text       []byte
	Media      *repository.DeliveryMedia
	MediaBytes []byte
	Caption    []byte
}

func (c *PlaintextContent) Zero() {
	secretcrypto.Zero(c.Text)
	secretcrypto.Zero(c.MediaBytes)
	secretcrypto.Zero(c.Caption)
	c.Text = nil
	c.MediaBytes = nil
	c.Caption = nil
}

type OpenDelivery struct {
	Whisper         domain.Whisper
	EventID         int64
	CallbackQueryID string
	Content         PlaintextContent
}

func (s *Service) ReserveOpen(
	ctx context.Context,
	callbackData string,
	telegramUserID int64,
	callbackQueryID string,
) (OpenDelivery, error) {
	hash, err := tokenHashFromCallback(callbackData)
	if err != nil {
		return OpenDelivery{}, ErrInvalidOpenToken
	}
	now := s.now()
	reservation, err := s.store.ReserveOpen(ctx, repository.ReserveOpenParams{
		OpenTokenHash: hash, TelegramUserID: telegramUserID, CallbackQueryID: callbackQueryID,
		Now: now, LeaseUntil: now.Add(s.options.OpenLease),
	})
	if err != nil {
		return OpenDelivery{}, mapOpenError(err)
	}

	content := PlaintextContent{Kind: reservation.Content.Kind, Media: reservation.Content.Media}
	if reservation.Content.Text != nil {
		content.Text, err = s.decryptStored(
			secretcrypto.PurposeText, reservation.Whisper.ID, *reservation.Content.Text,
		)
		if err != nil {
			_ = s.failOpen(ctx, reservation, callbackQueryID, "decrypt_text_failed")
			return OpenDelivery{}, err
		}
	}
	if reservation.Content.Caption != nil {
		content.Caption, err = s.decryptStored(
			secretcrypto.PurposeCaption, reservation.Whisper.ID, *reservation.Content.Caption,
		)
		if err != nil {
			content.Zero()
			_ = s.failOpen(ctx, reservation, callbackQueryID, "decrypt_caption_failed")
			return OpenDelivery{}, err
		}
	}
	return OpenDelivery{
		Whisper: reservation.Whisper, EventID: reservation.EventID,
		CallbackQueryID: callbackQueryID, Content: content,
	}, nil
}

func (s *Service) CompleteOpen(ctx context.Context, delivery OpenDelivery, ephemeralMessageID int64) error {
	now := s.now()
	messageID := ephemeralMessageID
	deleteAfter := s.GetEphemeralDeleteAfter()
	var deleteAt time.Time
	if deleteAfter > 0 {
		deleteAt = now.Add(deleteAfter)
	}
	return s.store.CompleteOpen(ctx, repository.CompleteOpenParams{
		WhisperID: delivery.Whisper.ID, EventID: delivery.EventID,
		CallbackQueryID:   delivery.CallbackQueryID,
		TelegramMessageID: &messageID, EphemeralMessageID: &ephemeralMessageID,
		DeleteAt: deleteAt, Now: now,
	})
}

func (s *Service) FailOpen(ctx context.Context, delivery OpenDelivery, errorCode string) error {
	return s.store.FailOpen(ctx, repository.FailOpenParams{
		WhisperID: delivery.Whisper.ID, EventID: delivery.EventID,
		CallbackQueryID: delivery.CallbackQueryID, ErrorCode: errorCode, Now: s.now(),
	})
}

// WhisperMediaFallback decrypts the retained media blob for a whisper whose
// stored Telegram file_id can no longer be resent. The caller must zero the
// returned buffer after use.
func (s *Service) WhisperMediaFallback(ctx context.Context, whisperID uuid.UUID) ([]byte, domain.MediaType, string, error) {
	blob, err := s.store.FetchWhisperMedia(ctx, whisperID)
	if err != nil {
		return nil, "", "", mapRepositoryError(err)
	}
	plaintext, err := s.decryptStored(secretcrypto.PurposeMedia, blob.WhisperID, blob.Stored)
	if err != nil {
		return nil, "", "", err
	}
	return plaintext, blob.MediaType, blob.ContentType, nil
}

func (s *Service) failOpen(
	ctx context.Context,
	reservation repository.OpenReservation,
	callbackQueryID string,
	errorCode string,
) error {
	return s.store.FailOpen(ctx, repository.FailOpenParams{
		WhisperID: reservation.Whisper.ID, EventID: reservation.EventID,
		CallbackQueryID: callbackQueryID, ErrorCode: errorCode, Now: s.now(),
	})
}

func mapOpenError(err error) error {
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return ErrWhisperNotFound
	case errors.Is(err, repository.ErrUnauthorized):
		return ErrWrongRecipient
	case errors.Is(err, repository.ErrExpired):
		return ErrWhisperExpired
	case errors.Is(err, repository.ErrAlreadyOpened):
		return ErrWhisperAlreadyOpened
	case errors.Is(err, repository.ErrOpenAmbiguous):
		return ErrWhisperUnavailable
	case errors.Is(err, repository.ErrNotActive), errors.Is(err, repository.ErrConflict):
		return ErrWhisperUnavailable
	default:
		return err
	}
}
