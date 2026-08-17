package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/secretcrypto"
)

func TestOwnerOperationsRejectUnconfiguredActorsBeforeStoreAccess(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	service, _ := newTestService(t, store, validServiceOptions())
	ctx := context.Background()
	whisperID := uuid.New()

	if _, err := service.OwnerList(ctx, 9999, 10, 0); !errors.Is(err, ErrOwnerOnly) {
		t.Fatalf("OwnerList() error = %v, want ErrOwnerOnly", err)
	}
	if _, err := service.OwnerReview(ctx, 9999, whisperID); !errors.Is(err, ErrOwnerOnly) {
		t.Fatalf("OwnerReview() error = %v, want ErrOwnerOnly", err)
	}
	if err := service.OwnerDelete(ctx, 9999, whisperID); !errors.Is(err, ErrOwnerOnly) {
		t.Fatalf("OwnerDelete() error = %v, want ErrOwnerOnly", err)
	}
	if err := service.OwnerSetRetention(ctx, 9999, whisperID, time.Hour); !errors.Is(err, ErrOwnerOnly) {
		t.Fatalf("OwnerSetRetention() error = %v, want ErrOwnerOnly", err)
	}
	if store.ownerListCalls != 0 || store.ownerGetCalls != 0 || store.ownerReadCalls != 0 ||
		len(store.ownerDeletes) != 0 || len(store.ownerRetentions) != 0 {
		t.Fatal("unauthorized owner request reached repository")
	}
}

func TestOwnerListForwardsBoundedPage(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	service, _ := newTestService(t, store, validServiceOptions())
	if _, err := service.OwnerList(context.Background(), 9001, 25, 75); err != nil {
		t.Fatalf("OwnerList() error = %v", err)
	}
	if len(store.ownerLists) != 1 {
		t.Fatalf("owner list calls = %d, want 1", len(store.ownerLists))
	}
	params := store.ownerLists[0]
	if params.OwnerTelegramUserID != 9001 || params.Limit != 25 || params.Offset != 75 || params.Reason != "owner_command" {
		t.Fatalf("owner list params = %#v", params)
	}
}

func TestOwnerReviewDecryptsRetainedMediaAndZeroErasesPlaintext(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	service, keyring := newTestService(t, store, validServiceOptions())
	whisperID := uuid.New()
	media := storedPayload(t, keyring, secretcrypto.PurposeMedia, whisperID, []byte("owner media bytes"), "audio/ogg")
	caption := storedPayload(t, keyring, secretcrypto.PurposeCaption, whisperID, []byte("owner caption"), "text/plain")
	store.ownerWhisper = domain.Whisper{
		ID: whisperID,
		Content: domain.ContentReference{
			Kind: domain.PayloadMedia,
			Media: &domain.MediaReference{
				Provider: domain.MediaProviderPostgresBlob,
				Type:     domain.MediaVoice,
				Ref:      "retained-media",
				BlobID:   &media.ID,
			},
			CaptionBlobID: &caption.ID,
		},
	}
	store.ownerContent = repository.StoredContent{
		Kind: domain.PayloadMedia, Media: &media, Caption: &caption,
	}

	review, err := service.OwnerReview(context.Background(), 9001, whisperID)
	if err != nil {
		t.Fatalf("OwnerReview() error = %v", err)
	}
	if string(review.Content.MediaBytes) != "owner media bytes" || string(review.Content.Caption) != "owner caption" {
		t.Fatalf("owner plaintext = media %q caption %q", review.Content.MediaBytes, review.Content.Caption)
	}
	if review.Content.Media == nil || review.Content.Media.BlobID != media.ID ||
		review.Content.Media.Type != domain.MediaVoice || review.Content.Media.ContentType != "audio/ogg" {
		t.Fatalf("owner media metadata = %#v", review.Content.Media)
	}
	if store.ownerGetCalls != 1 || store.ownerReadCalls != 1 {
		t.Fatalf("owner repository reads = metadata %d content %d, want 1/1", store.ownerGetCalls, store.ownerReadCalls)
	}

	mediaAlias := review.Content.MediaBytes
	captionAlias := review.Content.Caption
	review.Zero()
	if review.Content.MediaBytes != nil || review.Content.Caption != nil ||
		!allZero(mediaAlias) || !allZero(captionAlias) {
		t.Fatal("OwnerReview.Zero() did not nil and overwrite retained plaintext")
	}
}

func TestAuthorizedOwnerMutationsCarryAuditReasonAndDeterministicRetention(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	service, _ := newTestService(t, store, validServiceOptions())
	whisperID := uuid.New()
	ctx := context.Background()

	if err := service.OwnerDelete(ctx, 9001, whisperID); err != nil {
		t.Fatalf("OwnerDelete() error = %v", err)
	}
	if len(store.ownerDeletes) != 1 {
		t.Fatalf("owner deletes = %d, want 1", len(store.ownerDeletes))
	}
	deleted := store.ownerDeletes[0]
	if deleted.OwnerTelegramUserID != 9001 || deleted.WhisperID != whisperID ||
		deleted.Reason != "owner_command" || deleted.Now != serviceTestNow {
		t.Fatalf("owner delete params = %#v", deleted)
	}

	retention := 48 * time.Hour
	if err := service.OwnerSetRetention(ctx, 9001, whisperID, retention); err != nil {
		t.Fatalf("OwnerSetRetention() error = %v", err)
	}
	if len(store.ownerRetentions) != 1 {
		t.Fatalf("owner retention updates = %d, want 1", len(store.ownerRetentions))
	}
	updated := store.ownerRetentions[0]
	if updated.OwnerTelegramUserID != 9001 || updated.WhisperID != whisperID ||
		updated.Reason != "owner_command" || updated.Now != serviceTestNow ||
		updated.RetainUntil != serviceTestNow.Add(retention) {
		t.Fatalf("owner retention params = %#v", updated)
	}

	if err := service.OwnerSetRetention(ctx, 9001, whisperID, 0); !errors.Is(err, repository.ErrInvalidInput) {
		t.Fatalf("OwnerSetRetention(0) error = %v, want ErrInvalidInput", err)
	}
	if len(store.ownerRetentions) != 1 {
		t.Fatal("invalid owner retention reached repository")
	}
}
