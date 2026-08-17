package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/secretcrypto"
	"github.com/idan/secretmediabot/internal/token"
)

type CreatedWhisper struct {
	Whisper      domain.Whisper
	Recipient    domain.User
	CallbackData string
}

func (s *Service) ClaimIngest(ctx context.Context, senderID int64) (domain.Draft, error) {
	now := s.now()
	draft, err := s.store.ClaimLatestDraftIngest(ctx, senderID, now, now.Add(s.options.IngestLease))
	if errors.Is(err, repository.ErrNotFound) {
		return domain.Draft{}, ErrDraftNotFound
	}
	if errors.Is(err, repository.ErrConflict) {
		return domain.Draft{}, ErrDraftBusy
	}
	return draft, err
}

func (s *Service) ReleaseIngest(ctx context.Context, draft domain.Draft) error {
	if draft.IngestLeaseUntil == nil {
		return ErrDraftBusy
	}
	return s.store.ReleaseDraftIngest(ctx, repository.ReleaseDraftIngestParams{
		DraftID: draft.ID, SenderID: draft.SenderID,
		ExpectedLeaseUntil: *draft.IngestLeaseUntil, Now: s.now(),
	})
}

func (s *Service) FinalizeText(ctx context.Context, draft domain.Draft, text string) (CreatedWhisper, error) {
	if strings.TrimSpace(text) == "" {
		return CreatedWhisper{}, ErrUnsupportedContent
	}
	if utf8.RuneCountInString(text) > MaxSecretTextRunes {
		return CreatedWhisper{}, ErrTextTooLong
	}
	plaintext := []byte(text)
	defer secretcrypto.Zero(plaintext)
	return s.finalize(ctx, draft, plaintext, nil, nil, nil)
}

func (s *Service) FinalizeMedia(
	ctx context.Context,
	draft domain.Draft,
	telegramMedia domain.MediaReference,
	mediaBytes []byte,
	caption string,
) (CreatedWhisper, error) {
	defer secretcrypto.Zero(mediaBytes)
	if telegramMedia.Provider != domain.MediaProviderTelegram {
		return CreatedWhisper{}, ErrUnsupportedContent
	}
	if err := telegramMedia.Validate(); err != nil {
		return CreatedWhisper{}, fmt.Errorf("%w: %v", ErrUnsupportedContent, err)
	}
	if len(mediaBytes) == 0 {
		return CreatedWhisper{}, ErrUnsupportedContent
	}
	if int64(len(mediaBytes)) > s.options.MaxMediaBytes || telegramMedia.SizeBytes > s.options.MaxMediaBytes {
		return CreatedWhisper{}, ErrContentTooLarge
	}
	if utf8.RuneCountInString(caption) > MaxCaptionRunes {
		return CreatedWhisper{}, ErrCaptionTooLong
	}
	var captionBytes []byte
	if caption != "" {
		captionBytes = []byte(caption)
		defer secretcrypto.Zero(captionBytes)
	}
	return s.finalize(ctx, draft, nil, &telegramMedia, mediaBytes, captionBytes)
}

// finalize has two call shapes: text passes textBytes and nil media; media
// passes a Telegram reference, mediaBytes, and an optional caption.
func (s *Service) finalize(
	ctx context.Context,
	draft domain.Draft,
	textBytes []byte,
	telegramMedia *domain.MediaReference,
	mediaBytes []byte,
	captionBytes []byte,
) (CreatedWhisper, error) {
	if draft.IngestLeaseUntil == nil || draft.State != domain.DraftIngestingMedia {
		return CreatedWhisper{}, ErrDraftBusy
	}
	now := s.now()
	retention := now.Add(s.options.ContentRetention)
	whisperID := uuid.New()
	callbackToken, err := token.Generate()
	if err != nil {
		return CreatedWhisper{}, err
	}

	callbackRowID := uuid.New()
	callbackBytes := []byte(callbackToken.Data)
	defer secretcrypto.Zero(callbackBytes)
	callbackInput, err := s.encryptInput(
		secretcrypto.PurposeCallback, callbackRowID, whisperID,
		callbackBytes, "text/plain; charset=utf-8", retention,
	)
	if err != nil {
		return CreatedWhisper{}, err
	}

	var (
		content      domain.ContentReference
		textInput    *repository.EncryptedBlobInput
		mediaInput   *repository.EncryptedBlobInput
		captionInput *repository.EncryptedBlobInput
		fileID       string
		fileUniqueID string
	)
	if telegramMedia == nil {
		textID := uuid.New()
		encrypted, encryptErr := s.encryptInput(
			secretcrypto.PurposeText, textID, whisperID, textBytes,
			"text/plain; charset=utf-8", retention,
		)
		if encryptErr != nil {
			return CreatedWhisper{}, encryptErr
		}
		textInput = &encrypted
		content = domain.ContentReference{Kind: domain.PayloadText, TextBlobID: &textID}
	} else {
		if len(mediaBytes) == 0 {
			return CreatedWhisper{}, ErrUnsupportedContent
		}
		mediaID := uuid.New()
		encrypted, encryptErr := s.encryptInput(
			secretcrypto.PurposeMedia, mediaID, whisperID, mediaBytes,
			telegramMedia.ContentType, retention,
		)
		if encryptErr != nil {
			return CreatedWhisper{}, encryptErr
		}
		mediaInput = &encrypted
		storedMedia := *telegramMedia
		storedMedia.Provider = domain.MediaProviderPostgresBlob
		storedMedia.BlobID = &mediaID
		storedMedia.SizeBytes = int64(len(mediaBytes))
		content = domain.ContentReference{Kind: domain.PayloadMedia, Media: &storedMedia}
		fileID, fileUniqueID = telegramMedia.Ref, telegramMedia.UniqueRef

		if len(captionBytes) > 0 {
			captionID := uuid.New()
			caption, encryptErr := s.encryptInput(
				secretcrypto.PurposeCaption, captionID, whisperID, captionBytes,
				"text/plain; charset=utf-8", retention,
			)
			if encryptErr != nil {
				return CreatedWhisper{}, encryptErr
			}
			captionInput = &caption
			content.CaptionBlobID = &captionID
		}
	}

	oneTime, protect := s.options.DefaultOneTime, s.options.ProtectContent
	whisper, err := domain.NewWhisper(domain.NewWhisperParams{
		ID: whisperID, DraftID: draft.ID, OpenTokenHash: callbackToken.Hash[:],
		SenderID: draft.SenderID, RecipientID: draft.RecipientID,
		SourceChatID: draft.SourceChatID, SourceThreadID: draft.SourceThreadID,
		Content: content, OneTime: &oneTime, ProtectContent: &protect,
		CreatedAt: now, ExpiresAt: now.Add(s.options.WhisperTTL),
		ContentRetainUntil: &retention, MetadataRetainUntil: &retention,
	})
	if err != nil {
		return CreatedWhisper{}, err
	}

	whisper, err = s.store.FinalizeDraft(ctx, repository.FinalizeDraftParams{
		DraftID: draft.ID, SenderID: draft.SenderID,
		ExpectedLeaseUntil: *draft.IngestLeaseUntil,
		Whisper:            whisper, TelegramFileID: fileID, TelegramFileUniqueID: fileUniqueID,
		CallbackToken: &callbackInput, Text: textInput, Media: mediaInput, Caption: captionInput,
		Now: now,
	})
	if err != nil {
		return CreatedWhisper{}, err
	}
	recipient, err := s.store.FindObservedUserByID(ctx, draft.SourceChatID, draft.RecipientID)
	if err != nil {
		return CreatedWhisper{}, err
	}
	return CreatedWhisper{Whisper: whisper, Recipient: recipient, CallbackData: callbackToken.Data}, nil
}

func (s *Service) encryptInput(
	purpose secretcrypto.RecordPurpose,
	id, whisperID uuid.UUID,
	plaintext []byte,
	contentType string,
	retainUntil time.Time,
) (repository.EncryptedBlobInput, error) {
	aad, err := secretcrypto.AssociatedData(purpose, id, whisperID)
	if err != nil {
		return repository.EncryptedBlobInput{}, err
	}
	encrypted, err := s.cipher.Encrypt(plaintext, aad)
	if err != nil {
		return repository.EncryptedBlobInput{}, err
	}
	return repository.EncryptedBlobInput{
		ID: id, Payload: encrypted, ContentType: contentType,
		PlaintextSize: int64(len(plaintext)), RetainUntil: retainUntil,
	}, nil
}
