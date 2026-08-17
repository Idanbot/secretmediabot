package repository

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Store) FinalizeDraft(ctx context.Context, params FinalizeDraftParams) (domain.Whisper, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return domain.Whisper{}, err
	}
	now := nowOr(params.Now)
	if err := validateFinalization(params, now); err != nil {
		return domain.Whisper{}, err
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		var draft draftRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", params.DraftID).Take(&draft).Error; err != nil {
			return translateError(err)
		}
		if draft.SenderID != params.SenderID {
			return ErrUnauthorized
		}
		if draft.State != string(domain.DraftIngestingMedia) || draft.IngestLeaseUntil == nil ||
			!draft.IngestLeaseUntil.Equal(params.ExpectedLeaseUntil.UTC()) {
			return ErrLeaseLost
		}
		if !draft.ExpiresAt.After(now) || !draft.IngestLeaseUntil.After(now) {
			return ErrExpired
		}
		if params.Whisper.DraftID != draft.ID || params.Whisper.SenderID != draft.SenderID || params.Whisper.RecipientID != draft.RecipientID ||
			params.Whisper.SourceChatID != draft.SourceChatID || params.Whisper.SourceThreadID == nil != (draft.SourceThreadID == nil) ||
			(params.Whisper.SourceThreadID != nil && *params.Whisper.SourceThreadID != *draft.SourceThreadID) {
			return fmt.Errorf("%w: whisper does not match its draft", ErrInvalidInput)
		}

		whisper := whisperRowFromFinalize(params, now)
		if err := tx.Create(&whisper).Error; err != nil {
			return translateError(err)
		}
		callback := callbackTokenRow(params.Whisper.ID, *params.CallbackToken, now)
		if err := tx.Create(&callback).Error; err != nil {
			return translateError(err)
		}
		if params.Text != nil {
			row := textPayloadRow(params.Whisper.ID, "text", *params.Text, now, *params.Whisper.ContentRetainUntil)
			if err := tx.Create(&row).Error; err != nil {
				return translateError(err)
			}
		}
		if params.Media != nil {
			row := mediaRow(params.Whisper.ID, *params.Media, now, *params.Whisper.ContentRetainUntil)
			if err := tx.Create(&row).Error; err != nil {
				return translateError(err)
			}
		}
		if params.Caption != nil {
			row := textPayloadRow(params.Whisper.ID, "caption", *params.Caption, now, *params.Whisper.ContentRetainUntil)
			if err := tx.Create(&row).Error; err != nil {
				return translateError(err)
			}
		}

		result := tx.Model(&draftRow{}).
			Where("id = ? AND state = 'ingesting_media' AND ingest_lease_until = ?",
				params.DraftID, params.ExpectedLeaseUntil.UTC()).
			Updates(map[string]any{
				"state":              string(domain.DraftCompleted),
				"ingest_lease_until": nil,
				"completed_at":       now,
				"updated_at":         now,
			})
		if result.Error != nil {
			return translateError(result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrLeaseLost
		}
		return nil
	})
	if err != nil {
		return domain.Whisper{}, err
	}
	return params.Whisper, nil
}

func validateFinalization(params FinalizeDraftParams, now time.Time) error {
	if params.DraftID == uuid.Nil || params.SenderID <= 0 || params.ExpectedLeaseUntil.IsZero() {
		return fmt.Errorf("%w: draft ID, sender, and ingest lease are required", ErrInvalidInput)
	}
	if err := params.Whisper.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if params.Whisper.Status != domain.WhisperActive || params.Whisper.PublishState != domain.PublishPending {
		return fmt.Errorf("%w: finalized whisper must start active and pending publication", ErrInvalidInput)
	}
	if params.CallbackToken == nil {
		return fmt.Errorf("%w: encrypted callback token is required", ErrInvalidInput)
	}
	if err := validateEncryptedInput(*params.CallbackToken, now, true); err != nil {
		return err
	}
	if params.Whisper.ContentRetainUntil == nil || params.Whisper.MetadataRetainUntil == nil ||
		!params.Whisper.ContentRetainUntil.After(now) || !params.Whisper.MetadataRetainUntil.After(now) {
		return fmt.Errorf("%w: finite future content and metadata retention are required", ErrInvalidInput)
	}
	if !params.Whisper.ContentRetainUntil.Equal(*params.Whisper.MetadataRetainUntil) {
		return fmt.Errorf("%w: V1 requires content and metadata retention deadlines to match", ErrInvalidInput)
	}
	if len(params.Whisper.OpenTokenHash) != sha256.Size {
		return fmt.Errorf("%w: open token hash must be SHA-256", ErrInvalidInput)
	}

	switch params.Whisper.Content.Kind {
	case domain.PayloadText:
		if params.Text == nil || params.Media != nil || params.Caption != nil ||
			params.Whisper.Content.TextBlobID == nil || params.Text.ID != *params.Whisper.Content.TextBlobID ||
			params.TelegramFileID != "" || params.TelegramFileUniqueID != "" {
			return fmt.Errorf("%w: text finalization has invalid payload cardinality", ErrInvalidInput)
		}
		if err := validateEncryptedInput(*params.Text, now, true); err != nil {
			return err
		}
		if err := validatePayloadRetention(*params.Text, *params.Whisper.ContentRetainUntil); err != nil {
			return err
		}
	case domain.PayloadMedia:
		media := params.Whisper.Content.Media
		if params.Media == nil || params.Text != nil || media == nil || media.Provider != domain.MediaProviderPostgresBlob ||
			media.BlobID == nil || params.Media.ID != *media.BlobID || strings.TrimSpace(params.TelegramFileID) == "" {
			return fmt.Errorf("%w: media finalization has invalid payload cardinality", ErrInvalidInput)
		}
		if err := validateEncryptedInput(*params.Media, now, false); err != nil {
			return err
		}
		if err := validatePayloadRetention(*params.Media, *params.Whisper.ContentRetainUntil); err != nil {
			return err
		}
		if params.Whisper.Content.CaptionBlobID == nil != (params.Caption == nil) {
			return fmt.Errorf("%w: caption reference and encrypted row must agree", ErrInvalidInput)
		}
		if params.Caption != nil {
			if params.Caption.ID != *params.Whisper.Content.CaptionBlobID {
				return fmt.Errorf("%w: caption row ID does not match content reference", ErrInvalidInput)
			}
			if err := validateEncryptedInput(*params.Caption, now, true); err != nil {
				return err
			}
			if err := validatePayloadRetention(*params.Caption, *params.Whisper.ContentRetainUntil); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%w: unsupported payload kind", ErrInvalidInput)
	}
	return nil
}

func validateEncryptedInput(input EncryptedBlobInput, now time.Time, requireNonEmpty bool) error {
	if input.ID == uuid.Nil || input.Payload.KeyID == "" || len(input.Payload.Nonce) != 12 ||
		len(input.Payload.Ciphertext) == 0 || len(input.Payload.CiphertextSHA256) != sha256.Size || input.PlaintextSize < 0 {
		return fmt.Errorf("%w: malformed encrypted payload", ErrInvalidInput)
	}
	if requireNonEmpty && input.PlaintextSize == 0 {
		return fmt.Errorf("%w: encrypted text payload is empty", ErrInvalidInput)
	}
	if !input.RetainUntil.IsZero() && !input.RetainUntil.After(now) {
		return fmt.Errorf("%w: encrypted payload retention must be in the future", ErrInvalidInput)
	}
	return nil
}

func validatePayloadRetention(input EncryptedBlobInput, expected time.Time) error {
	if !input.RetainUntil.IsZero() && !input.RetainUntil.Equal(expected) {
		return fmt.Errorf("%w: payload retention must match whisper retention in V1", ErrInvalidInput)
	}
	return nil
}

func whisperRowFromFinalize(params FinalizeDraftParams, now time.Time) whisperRow {
	w := params.Whisper
	nextAttempt := w.NextPublishAttemptAt.UTC()
	row := whisperRow{
		ID:                   w.ID,
		DraftID:              params.DraftID,
		OpenTokenHash:        cloneBytes(w.OpenTokenHash),
		SenderID:             w.SenderID,
		RecipientID:          w.RecipientID,
		SourceChatID:         w.SourceChatID,
		SourceThreadID:       cloneInt64Pointer(w.SourceThreadID),
		PayloadKind:          string(w.Content.Kind),
		OneTime:              w.OneTime,
		ProtectContent:       w.ProtectContent,
		Status:               string(w.Status),
		PublishState:         string(w.PublishState),
		PublishAttemptCount:  w.PublishAttemptCount,
		NextPublishAttemptAt: nextAttempt,
		CreatedAt:            w.CreatedAt.UTC(),
		UpdatedAt:            now,
		ExpiresAt:            w.ExpiresAt.UTC(),
		RetentionDeleteAt:    w.MetadataRetainUntil.UTC(),
	}
	if w.Content.Media != nil {
		provider := string(w.Content.Media.Provider)
		mediaType := string(w.Content.Media.Type)
		fileID := params.TelegramFileID
		row.MediaProvider = &provider
		row.MediaType = &mediaType
		row.TelegramFileID = &fileID
		if params.TelegramFileUniqueID != "" {
			uniqueID := params.TelegramFileUniqueID
			row.TelegramFileUniqueID = &uniqueID
		}
	}
	return row
}

func mediaRow(whisperID uuid.UUID, input EncryptedBlobInput, now, fallbackRetention time.Time) mediaBlobRow {
	retention := input.RetainUntil.UTC()
	if input.RetainUntil.IsZero() {
		retention = fallbackRetention.UTC()
	}
	return mediaBlobRow{
		ID:                  input.ID,
		WhisperID:           whisperID,
		EncryptionAlgorithm: "AES-256-GCM",
		EncryptionKeyID:     input.Payload.KeyID,
		Nonce:               cloneBytes(input.Payload.Nonce),
		Ciphertext:          cloneBytes(input.Payload.Ciphertext),
		CiphertextSHA256:    cloneBytes(input.Payload.CiphertextSHA256[:]),
		ContentType:         input.ContentType,
		PlaintextSizeBytes:  input.PlaintextSize,
		CreatedAt:           now,
		RetentionDeleteAt:   retention,
	}
}

func textPayloadRow(whisperID uuid.UUID, purpose string, input EncryptedBlobInput, now, fallbackRetention time.Time) encryptedTextPayloadRow {
	retention := input.RetainUntil.UTC()
	if input.RetainUntil.IsZero() {
		retention = fallbackRetention.UTC()
	}
	return encryptedTextPayloadRow{
		ID:                  input.ID,
		WhisperID:           whisperID,
		Purpose:             purpose,
		EncryptionAlgorithm: "AES-256-GCM",
		EncryptionKeyID:     input.Payload.KeyID,
		Nonce:               cloneBytes(input.Payload.Nonce),
		Ciphertext:          cloneBytes(input.Payload.Ciphertext),
		CiphertextSHA256:    cloneBytes(input.Payload.CiphertextSHA256[:]),
		PlaintextSizeBytes:  input.PlaintextSize,
		CreatedAt:           now,
		RetentionDeleteAt:   retention,
	}
}

func callbackTokenRow(whisperID uuid.UUID, input EncryptedBlobInput, now time.Time) encryptedCallbackTokenRow {
	return encryptedCallbackTokenRow{
		ID:                  input.ID,
		WhisperID:           whisperID,
		EncryptionAlgorithm: "AES-256-GCM",
		EncryptionKeyID:     input.Payload.KeyID,
		Nonce:               cloneBytes(input.Payload.Nonce),
		Ciphertext:          cloneBytes(input.Payload.Ciphertext),
		CiphertextSHA256:    cloneBytes(input.Payload.CiphertextSHA256[:]),
		PlaintextSizeBytes:  input.PlaintextSize,
		CreatedAt:           now,
	}
}

func loadStoredContent(db *gorm.DB, whisper whisperRow, includeMedia bool) (StoredContent, error) {
	content := StoredContent{Kind: domain.PayloadKind(whisper.PayloadKind)}
	switch content.Kind {
	case domain.PayloadText:
		var row encryptedTextPayloadRow
		if err := db.Where("whisper_id = ? AND purpose = 'text'", whisper.ID).Take(&row).Error; err != nil {
			return StoredContent{}, translateError(err)
		}
		stored := row.toStored()
		content.Text = &stored
	case domain.PayloadMedia:
		if includeMedia {
			var row mediaBlobRow
			if err := db.Where("whisper_id = ?", whisper.ID).Take(&row).Error; err != nil {
				return StoredContent{}, translateError(err)
			}
			stored := row.toStored()
			content.Media = &stored
		}
		var caption encryptedTextPayloadRow
		err := db.Where("whisper_id = ? AND purpose = 'caption'", whisper.ID).Take(&caption).Error
		if err == nil {
			stored := caption.toStored()
			content.Caption = &stored
		} else if err != gorm.ErrRecordNotFound {
			return StoredContent{}, translateError(err)
		}
	default:
		return StoredContent{}, fmt.Errorf("%w: unsupported payload kind", ErrConflict)
	}
	return content, nil
}
