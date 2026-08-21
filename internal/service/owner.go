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

type OwnerReview struct {
	Whisper domain.Whisper
	Content PlaintextContent
}

// OwnerListOptions controls the metadata-only owner browser. MediaTypes is
// intentionally a slice so a user-facing category such as "recording" can
// include both voice notes and audio files.
type OwnerListOptions struct {
	Limit          int
	Offset         int
	SenderID       *int64
	SenderUsername string
	MediaTypes     []domain.MediaType
}

func (r *OwnerReview) Zero() {
	r.Content.Zero()
}

func (s *Service) OwnerList(ctx context.Context, ownerID int64, limit, offset int) ([]domain.Whisper, error) {
	if !s.IsOwner(ownerID) {
		return nil, ErrOwnerOnly
	}
	return s.store.OwnerListWhispers(ctx, repository.OwnerListWhispersParams{
		OwnerTelegramUserID: ownerID, Limit: limit, Offset: offset, Reason: "owner_command",
	})
}

func (s *Service) OwnerListDetails(ctx context.Context, ownerID int64, options OwnerListOptions) ([]domain.OwnerWhisper, error) {
	if !s.IsOwner(ownerID) {
		return nil, ErrOwnerOnly
	}
	return s.store.OwnerListWhisperDetails(ctx, repository.OwnerListWhispersParams{
		OwnerTelegramUserID: ownerID,
		Limit:               options.Limit,
		Offset:              options.Offset,
		SenderID:            cloneOwnerInt64(options.SenderID),
		SenderUsername:      options.SenderUsername,
		MediaTypes:          append([]domain.MediaType(nil), options.MediaTypes...),
		Reason:              "owner_command",
	})
}

// OwnerMetadata reads only the safe metadata projection for an expanded list
// item. Content decryption remains behind OwnerReview and the explicit
// /owner_open action.
func (s *Service) OwnerMetadata(ctx context.Context, ownerID int64, whisperID uuid.UUID) (domain.OwnerWhisper, error) {
	if !s.IsOwner(ownerID) {
		return domain.OwnerWhisper{}, ErrOwnerOnly
	}
	whisper, err := s.store.OwnerGetWhisper(ctx, repository.OwnerGetWhisperParams{
		OwnerTelegramUserID: ownerID, WhisperID: whisperID, Reason: "owner_command",
	})
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return domain.OwnerWhisper{}, ErrWhisperNotFound
		}
		return domain.OwnerWhisper{}, err
	}

	detail := domain.OwnerWhisper{
		Whisper:   whisper,
		Sender:    domain.User{TelegramUserID: whisper.SenderID},
		Recipient: domain.User{TelegramUserID: whisper.RecipientID},
	}
	for userID, target := range map[int64]*domain.User{
		whisper.SenderID:    &detail.Sender,
		whisper.RecipientID: &detail.Recipient,
	} {
		user, lookupErr := s.store.FindObservedUserByID(ctx, whisper.SourceChatID, userID)
		if lookupErr == nil {
			*target = user
			continue
		}
		if errors.Is(lookupErr, repository.ErrNotFound) {
			continue
		}
		return domain.OwnerWhisper{}, lookupErr
	}
	return detail, nil
}

func (s *Service) OwnerReview(ctx context.Context, ownerID int64, whisperID uuid.UUID) (OwnerReview, error) {
	if !s.IsOwner(ownerID) {
		return OwnerReview{}, ErrOwnerOnly
	}
	params := repository.OwnerGetWhisperParams{
		OwnerTelegramUserID: ownerID, WhisperID: whisperID, Reason: "owner_debug_review",
	}
	whisper, err := s.store.OwnerGetWhisper(ctx, params)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return OwnerReview{}, ErrWhisperNotFound
		}
		return OwnerReview{}, err
	}
	stored, err := s.store.OwnerFetchEncryptedContent(ctx, params)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return OwnerReview{}, ErrWhisperNotFound
		}
		return OwnerReview{}, err
	}

	content := PlaintextContent{Kind: stored.Kind}
	if stored.Text != nil {
		content.Text, err = s.decryptStored(secretcrypto.PurposeText, whisper.ID, *stored.Text)
		if err != nil {
			return OwnerReview{}, err
		}
	}
	if stored.Media != nil {
		mediaBytes, decryptErr := s.decryptStored(secretcrypto.PurposeMedia, whisper.ID, *stored.Media)
		if decryptErr != nil {
			content.Zero()
			return OwnerReview{}, decryptErr
		}
		content.Media = &repository.DeliveryMedia{
			BlobID: stored.Media.ID, Type: whisper.Content.Media.Type,
			ContentType: stored.Media.ContentType, PlaintextSize: stored.Media.PlaintextSize,
		}
		content.MediaBytes = mediaBytes
	}
	if stored.Caption != nil {
		content.Caption, err = s.decryptStored(secretcrypto.PurposeCaption, whisper.ID, *stored.Caption)
		if err != nil {
			content.Zero()
			return OwnerReview{}, err
		}
	}
	return OwnerReview{Whisper: whisper, Content: content}, nil
}

func (s *Service) OwnerDelete(ctx context.Context, ownerID int64, whisperID uuid.UUID) error {
	if !s.IsOwner(ownerID) {
		return ErrOwnerOnly
	}
	err := s.store.OwnerDeleteWhisper(ctx, repository.OwnerDeleteWhisperParams{
		OwnerTelegramUserID: ownerID, WhisperID: whisperID, Reason: "owner_command", Now: s.now(),
	})
	if errors.Is(err, repository.ErrNotFound) {
		return ErrWhisperNotFound
	}
	return err
}

func (s *Service) OwnerSetRetention(
	ctx context.Context,
	ownerID int64,
	whisperID uuid.UUID,
	retention time.Duration,
) error {
	if !s.IsOwner(ownerID) {
		return ErrOwnerOnly
	}
	if retention <= 0 {
		return repository.ErrInvalidInput
	}
	now := s.now()
	err := s.store.OwnerUpdateRetention(ctx, repository.OwnerUpdateRetentionParams{
		OwnerTelegramUserID: ownerID, WhisperID: whisperID,
		RetainUntil: now.Add(retention), Reason: "owner_command", Now: now,
	})
	if errors.Is(err, repository.ErrNotFound) {
		return ErrWhisperNotFound
	}
	return err
}

func cloneOwnerInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
