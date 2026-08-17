package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/secretcrypto"
	"github.com/idan/secretmediabot/internal/token"
)

type Publication struct {
	Whisper      domain.Whisper
	Sender       domain.User
	Recipient    domain.User
	CallbackData string
}

func (s *Service) ClaimPublication(ctx context.Context, whisperID uuid.UUID) (Publication, error) {
	now := s.now()
	claim, err := s.store.ClaimPublish(ctx, repository.ClaimPublishParams{
		WhisperID: whisperID, Now: now, LeaseUntil: now.Add(s.options.PublishLease),
	})
	if err != nil {
		return Publication{}, err
	}
	return s.publicationFromClaim(ctx, claim)
}

func (s *Service) ClaimNextPublication(ctx context.Context) (Publication, error) {
	now := s.now()
	claim, err := s.store.ClaimNextPublish(ctx, now, now.Add(s.options.PublishLease))
	if err != nil {
		return Publication{}, err
	}
	return s.publicationFromClaim(ctx, claim)
}

func (s *Service) publicationFromClaim(ctx context.Context, claim repository.PublishClaim) (Publication, error) {
	plaintext, err := s.decryptStored(secretcrypto.PurposeCallback, claim.Whisper.ID, claim.CallbackToken)
	if err != nil {
		return Publication{}, err
	}
	defer secretcrypto.Zero(plaintext)
	callbackData := string(plaintext)
	if _, err := token.ParseCallbackData(callbackData); err != nil {
		return Publication{}, ErrInvalidOpenToken
	}
	sender, err := s.store.FindObservedUserByID(ctx, claim.Whisper.SourceChatID, claim.Whisper.SenderID)
	if err != nil {
		return Publication{}, err
	}
	recipient, err := s.store.FindObservedUserByID(ctx, claim.Whisper.SourceChatID, claim.Whisper.RecipientID)
	if err != nil {
		return Publication{}, err
	}
	return Publication{
		Whisper: claim.Whisper, Sender: sender, Recipient: recipient, CallbackData: callbackData,
	}, nil
}

func (s *Service) CompletePublication(ctx context.Context, publication Publication, publicMessageID int64) error {
	if publication.Whisper.PublishLeaseUntil == nil {
		return repository.ErrLeaseLost
	}
	return s.store.MarkPublished(ctx, repository.MarkPublishedParams{
		WhisperID: publication.Whisper.ID, ExpectedLeaseUntil: *publication.Whisper.PublishLeaseUntil,
		PublicMessageID: publicMessageID, Now: s.now(),
	})
}

func (s *Service) FailPublication(
	ctx context.Context,
	publication Publication,
	errorCode string,
	retryAfter time.Duration,
) error {
	if publication.Whisper.PublishLeaseUntil == nil {
		return repository.ErrLeaseLost
	}
	now := s.now()
	params := repository.MarkPublishFailedParams{
		WhisperID: publication.Whisper.ID, ExpectedLeaseUntil: *publication.Whisper.PublishLeaseUntil,
		Now: now, ErrorCode: errorCode, Terminal: retryAfter <= 0,
	}
	if retryAfter > 0 {
		retryAt := now.Add(retryAfter)
		params.RetryAt = &retryAt
	}
	return s.store.MarkPublishFailed(ctx, params)
}

func IsNoPublication(err error) bool {
	return errors.Is(err, repository.ErrNotFound) || errors.Is(err, repository.ErrNotActive)
}
