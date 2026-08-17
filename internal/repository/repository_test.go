package repository

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/secretcrypto"
)

func TestWhisperMetadataProjectionExcludesSensitivePayloads(t *testing.T) {
	t.Parallel()
	query := strings.ToLower(whisperMetadataSelect + whisperMetadataJoins)
	for _, forbidden := range []string{"ciphertext", "telegram_file_id", "telegram_file_unique_id", "nonce", "encryption_key_id"} {
		if strings.Contains(query, forbidden) {
			t.Errorf("metadata projection contains sensitive column %q", forbidden)
		}
	}
	for _, required := range []string{"media_blob_id", "text_blob_id", "caption_blob_id", "plaintext_size_bytes"} {
		if !strings.Contains(query, required) {
			t.Errorf("metadata projection is missing safe reference %q", required)
		}
	}
}

func TestNormalizeUsername(t *testing.T) {
	t.Parallel()
	if got := normalizeUsername("  @CaSe_Sensitive "); got != "case_sensitive" {
		t.Fatalf("normalizeUsername() = %q, want case_sensitive", got)
	}
	if got := normalizeUsername(" @@@ "); got != "" {
		t.Fatalf("normalizeUsername() = %q, want empty", got)
	}
}

func TestNormalizeOwnerListPage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		limit      int
		offset     int
		wantLimit  int
		wantOffset int
		wantError  bool
	}{
		{name: "default limit", wantLimit: 50},
		{name: "requested page", limit: 25, offset: 75, wantLimit: 25, wantOffset: 75},
		{name: "bounded limit", limit: 101, wantLimit: 100},
		{name: "negative offset", limit: 20, offset: -1, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limit, offset, err := normalizeOwnerListPage(test.limit, test.offset)
			if (err != nil) != test.wantError {
				t.Fatalf("normalizeOwnerListPage() error = %v, wantError %t", err, test.wantError)
			}
			if !test.wantError && (limit != test.wantLimit || offset != test.wantOffset) {
				t.Fatalf("normalizeOwnerListPage() = %d/%d, want %d/%d", limit, offset, test.wantLimit, test.wantOffset)
			}
		})
	}
}

func TestSafeErrorCodeIsBoundedAndNonempty(t *testing.T) {
	t.Parallel()
	if got := safeErrorCode("   "); got != "unspecified" {
		t.Fatalf("safeErrorCode(empty) = %q", got)
	}
	got := safeErrorCode(strings.Repeat("🙂", 200))
	if runes := []rune(got); len(runes) != 128 {
		t.Fatalf("safeErrorCode() rune count = %d, want 128", len(runes))
	}
}

func TestValidateEncryptedInput(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	valid := EncryptedBlobInput{
		ID: uuid.New(),
		Payload: secretcrypto.EncryptedPayload{
			KeyID:            "v1",
			Nonce:            make([]byte, 12),
			Ciphertext:       make([]byte, 17),
			CiphertextSHA256: [32]byte{1},
		},
		PlaintextSize: 1,
		RetainUntil:   now.Add(time.Hour),
	}
	if err := validateEncryptedInput(valid, now, true); err != nil {
		t.Fatalf("validateEncryptedInput(valid) = %v", err)
	}

	invalid := valid
	invalid.Payload.Nonce = make([]byte, 11)
	if err := validateEncryptedInput(invalid, now, true); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid nonce error = %v, want ErrInvalidInput", err)
	}
	invalid = valid
	invalid.PlaintextSize = 0
	if err := validateEncryptedInput(invalid, now, true); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty text error = %v, want ErrInvalidInput", err)
	}
}

func TestEncryptedRowsReturnDefensiveCopies(t *testing.T) {
	t.Parallel()
	row := mediaBlobRow{
		ID:                uuid.New(),
		Nonce:             []byte{1, 2, 3},
		Ciphertext:        []byte{4, 5, 6},
		CiphertextSHA256:  []byte{7, 8, 9},
		RetentionDeleteAt: time.Now().UTC(),
	}
	stored := row.toStored()
	stored.Nonce[0] = 99
	stored.Ciphertext[0] = 99
	stored.CiphertextSHA256[0] = 99
	if row.Nonce[0] != 1 || row.Ciphertext[0] != 4 || row.CiphertextSHA256[0] != 7 {
		t.Fatal("stored payload aliases database scan buffers")
	}
}

func TestUninitializedStoreFailsClosed(t *testing.T) {
	t.Parallel()
	store := NewStore(nil)
	if _, err := store.CountActiveDrafts(context.Background(), 1, time.Now()); err == nil {
		t.Fatal("uninitialized store returned no error")
	}
}

func TestRowMappersPreserveDomainFields(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	user := userRow{
		TelegramUserID: 10, Username: "Target", FirstName: "First", LastName: "Last",
		LanguageCode: "he", IsBot: false, HasStartedPrivateChat: true,
		FirstSeenAt: now, LastSeenAt: now.Add(time.Minute), UpdatedAt: now.Add(2 * time.Minute),
	}.toDomain()
	if user.LanguageCode != "he" || !user.UpdatedAt.Equal(now.Add(2*time.Minute)) || !user.HasStartedPrivateChat {
		t.Fatalf("user mapper lost fields: %#v", user)
	}
	chat := chatRow{
		TelegramChatID: -100, ChatType: string(domain.ChatTypeSupergroup), Title: "Title", Username: "group_name",
		FirstSeenAt: now, LastSeenAt: now.Add(time.Minute), UpdatedAt: now.Add(2 * time.Minute),
	}.toDomain()
	if chat.Username != "group_name" || !chat.UpdatedAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("chat mapper lost fields: %#v", chat)
	}

	draftID := uuid.New()
	commandID := int64(33)
	draft := draftRow{
		ID: draftID, ComposeTokenHash: make([]byte, 32), SenderID: 10, RecipientID: 20, SourceChatID: -100,
		SourceCommandMessageID: &commandID, State: string(domain.DraftAwaitingMedia),
		CreatedAt: now, UpdatedAt: now.Add(time.Minute), ExpiresAt: now.Add(10 * time.Minute),
	}.toDomain()
	if err := draft.Validate(); err != nil {
		t.Fatalf("mapped draft is invalid: %v (%#v)", err, draft)
	}
	if draft.SourceCommandMessageID == nil || *draft.SourceCommandMessageID != commandID || !draft.UpdatedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("draft mapper lost fields: %#v", draft)
	}

	textID := uuid.New()
	openLease := now.Add(time.Minute)
	publishLease := now.Add(2 * time.Minute)
	callbackID := "callback"
	lastError := "retry"
	projection := whisperProjection{
		WhisperRow: whisperRow{
			ID: uuid.New(), DraftID: draftID, OpenTokenHash: make([]byte, 32), SenderID: 10, RecipientID: 20,
			SourceChatID: -100, PayloadKind: string(domain.PayloadText), OneTime: true, ProtectContent: true,
			Status: string(domain.WhisperOpening), PublishState: string(domain.PublishPublishing),
			PublishAttemptCount: 2, NextPublishAttemptAt: now, PublishLeaseUntil: &publishLease,
			LastPublishError: &lastError, OpeningCallbackQueryID: &callbackID,
			OpeningReservedAt: &now, OpeningLeaseUntil: &openLease,
			CreatedAt: now, UpdatedAt: now.Add(time.Second), ExpiresAt: now.Add(24 * time.Hour),
			RetentionDeleteAt: now.Add(30 * 24 * time.Hour),
		},
		TextBlobID:      &textID,
		TextRetainUntil: timePointer(now.Add(30 * 24 * time.Hour)),
	}
	whisper, err := projection.toDomain(nil)
	if err != nil {
		t.Fatalf("map whisper: %v", err)
	}
	if err := whisper.Validate(); err != nil {
		t.Fatalf("mapped whisper is invalid: %v (%#v)", err, whisper)
	}
	if whisper.DraftID != draftID || whisper.PublishAttemptCount != 2 || whisper.OpeningCallbackQueryID != callbackID ||
		whisper.LastPublishError != lastError || !whisper.UpdatedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("whisper mapper lost fields: %#v", whisper)
	}

	mediaID := uuid.New()
	provider := string(domain.MediaProviderPostgresBlob)
	mediaType := string(domain.MediaVoice)
	contentType := "audio/ogg"
	mediaSize := int64(1234)
	mediaProjection := whisperProjection{
		WhisperRow: whisperRow{
			ID: uuid.New(), DraftID: uuid.New(), OpenTokenHash: make([]byte, 32), SenderID: 10, RecipientID: 20,
			SourceChatID: -100, PayloadKind: string(domain.PayloadMedia), MediaProvider: &provider, MediaType: &mediaType,
			OneTime: true, ProtectContent: true, Status: string(domain.WhisperActive), PublishState: string(domain.PublishPending),
			NextPublishAttemptAt: now, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
			RetentionDeleteAt: now.Add(30 * 24 * time.Hour),
		},
		MediaBlobID:             &mediaID,
		MediaContentType:        &contentType,
		MediaPlaintextSizeBytes: &mediaSize,
		MediaRetainUntil:        timePointer(now.Add(30 * 24 * time.Hour)),
	}
	mediaWhisper, err := mediaProjection.toDomain(nil)
	if err != nil {
		t.Fatalf("map media whisper: %v", err)
	}
	if err := mediaWhisper.Validate(); err != nil {
		t.Fatalf("mapped media whisper is invalid: %v (%#v)", err, mediaWhisper)
	}
	if mediaWhisper.Content.Media == nil || mediaWhisper.Content.Media.Ref != mediaID.String() {
		t.Fatalf("media mapper did not use privacy-safe postgres blob reference: %#v", mediaWhisper.Content.Media)
	}
}

func TestOwnerRetrieveAuditDistinguishesTextAndMedia(t *testing.T) {
	t.Parallel()
	if got := ownerRetrieveAction(domain.PayloadText); got != domain.OwnerAuditRetrieveContent {
		t.Fatalf("text audit action = %q", got)
	}
	if got := ownerRetrieveAction(domain.PayloadMedia); got != domain.OwnerAuditRetrieveMedia {
		t.Fatalf("media audit action = %q", got)
	}
}
