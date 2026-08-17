package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/secretcrypto"
	"github.com/idan/secretmediabot/internal/token"
)

func TestFinalizeTextEncryptsExactlyTextAndCallback(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	draft := ingestingDraft(t, 501, 502, -5001)
	recipient := domain.User{TelegramUserID: draft.RecipientID, Username: "recipient"}
	store.addMember(draft.SourceChatID, recipient)
	service, keyring := newTestService(t, store, validServiceOptions())
	plaintext := "highly sensitive text U0001f510"

	created, err := service.FinalizeText(context.Background(), draft, plaintext)
	if err != nil {
		t.Fatalf("FinalizeText() error = %v", err)
	}
	params, ok := store.lastFinalization()
	if !ok {
		t.Fatal("FinalizeText() did not call repository finalization")
	}
	if params.Text == nil || params.Media != nil || params.Caption != nil || params.CallbackToken == nil {
		t.Fatalf("text finalization cardinality = text:%t media:%t caption:%t callback:%t",
			params.Text != nil, params.Media != nil, params.Caption != nil, params.CallbackToken != nil)
	}
	if params.TelegramFileID != "" || params.TelegramFileUniqueID != "" {
		t.Fatal("text finalization persisted Telegram media handles")
	}
	if params.Whisper.Content.Kind != domain.PayloadText || params.Whisper.Content.TextBlobID == nil ||
		*params.Whisper.Content.TextBlobID != params.Text.ID {
		t.Fatalf("text content reference = %#v, payload ID = %s", params.Whisper.Content, params.Text.ID)
	}
	if params.Text.PlaintextSize != int64(len([]byte(plaintext))) || params.Text.ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("text metadata = size %d type %q", params.Text.PlaintextSize, params.Text.ContentType)
	}
	if bytes.Contains(params.Text.Payload.Ciphertext, []byte(plaintext)) {
		t.Fatal("text ciphertext contains plaintext")
	}
	if got := decryptInput(t, keyring, secretcrypto.PurposeText, params.Whisper.ID, *params.Text); !bytes.Equal(got, []byte(plaintext)) {
		t.Fatalf("decrypted text = %q, want %q", got, plaintext)
	}
	callbackPlaintext := decryptInput(t, keyring, secretcrypto.PurposeCallback, params.Whisper.ID, *params.CallbackToken)
	if string(callbackPlaintext) != created.CallbackData {
		t.Fatal("encrypted callback does not match returned callback data")
	}
	if _, err := token.ParseCallbackData(created.CallbackData); err != nil {
		t.Fatalf("returned callback data is invalid: %v", err)
	}
	if created.Recipient.TelegramUserID != recipient.TelegramUserID || created.Whisper.ID != params.Whisper.ID {
		t.Fatalf("created whisper result = %#v", created)
	}
	assertFinalizationPolicy(t, params, validServiceOptions())
}

func TestFinalizeMediaEncryptsMediaAndOptionalCaptionWithExactCardinality(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	draft := ingestingDraft(t, 601, 602, -6001)
	store.addMember(draft.SourceChatID, domain.User{TelegramUserID: draft.RecipientID, Username: "recipient"})
	options := validServiceOptions()
	options.MaxMediaBytes = 64
	service, keyring := newTestService(t, store, options)
	mediaPlaintext := []byte("voice-message-bytes")
	wantMedia := append([]byte(nil), mediaPlaintext...)
	caption := "private caption"
	telegramMedia := domain.MediaReference{
		Provider:    domain.MediaProviderTelegram,
		Type:        domain.MediaVoice,
		Ref:         "telegram-file-id",
		UniqueRef:   "telegram-unique-id",
		ContentType: "audio/ogg",
		SizeBytes:   int64(len(mediaPlaintext)),
	}

	_, err := service.FinalizeMedia(context.Background(), draft, telegramMedia, mediaPlaintext, caption)
	if err != nil {
		t.Fatalf("FinalizeMedia() error = %v", err)
	}
	if !allZero(mediaPlaintext) {
		t.Fatal("FinalizeMedia() did not zero the caller's plaintext media buffer")
	}
	params, ok := store.lastFinalization()
	if !ok {
		t.Fatal("FinalizeMedia() did not call repository finalization")
	}
	if params.Text != nil || params.Media == nil || params.Caption == nil || params.CallbackToken == nil {
		t.Fatalf("media finalization cardinality = text:%t media:%t caption:%t callback:%t",
			params.Text != nil, params.Media != nil, params.Caption != nil, params.CallbackToken != nil)
	}
	if params.TelegramFileID != telegramMedia.Ref || params.TelegramFileUniqueID != telegramMedia.UniqueRef {
		t.Fatalf("Telegram handles = %q/%q", params.TelegramFileID, params.TelegramFileUniqueID)
	}
	mediaReference := params.Whisper.Content.Media
	if params.Whisper.Content.Kind != domain.PayloadMedia || mediaReference == nil ||
		mediaReference.Provider != domain.MediaProviderPostgresBlob || mediaReference.BlobID == nil ||
		*mediaReference.BlobID != params.Media.ID || mediaReference.SizeBytes != int64(len(wantMedia)) {
		t.Fatalf("stored media reference = %#v", mediaReference)
	}
	if params.Whisper.Content.CaptionBlobID == nil || *params.Whisper.Content.CaptionBlobID != params.Caption.ID {
		t.Fatalf("caption reference = %v, payload ID = %s", params.Whisper.Content.CaptionBlobID, params.Caption.ID)
	}
	if bytes.Contains(params.Media.Payload.Ciphertext, wantMedia) || bytes.Contains(params.Caption.Payload.Ciphertext, []byte(caption)) {
		t.Fatal("encrypted media finalization contains plaintext")
	}
	if got := decryptInput(t, keyring, secretcrypto.PurposeMedia, params.Whisper.ID, *params.Media); !bytes.Equal(got, wantMedia) {
		t.Fatalf("decrypted media = %q, want %q", got, wantMedia)
	}
	if got := decryptInput(t, keyring, secretcrypto.PurposeCaption, params.Whisper.ID, *params.Caption); !bytes.Equal(got, []byte(caption)) {
		t.Fatalf("decrypted caption = %q, want %q", got, caption)
	}
	assertFinalizationPolicy(t, params, options)
}

func TestFinalizeMediaEnforcesActualAndDeclaredSizeBoundsAndZerosInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		media    domain.MediaReference
		bytes    []byte
		caption  string
		want     error
		finalize bool
	}{
		{
			name:  "actual bytes above limit",
			media: validTelegramMedia(5), bytes: []byte{1, 2, 3, 4, 5},
			want: ErrContentTooLarge,
		},
		{
			name:  "declared size above limit",
			media: validTelegramMedia(5), bytes: []byte{1, 2, 3, 4},
			want: ErrContentTooLarge,
		},
		{
			name:  "exact limit",
			media: validTelegramMedia(4), bytes: []byte{1, 2, 3, 4},
			finalize: true,
		},
		{
			name:  "empty media",
			media: validTelegramMedia(0), bytes: []byte{},
			want: ErrUnsupportedContent,
		},
		{
			name:  "wrong provider",
			media: domain.MediaReference{Provider: domain.MediaProviderPostgresBlob, Type: domain.MediaVoice, Ref: "file"},
			bytes: []byte{1}, want: ErrUnsupportedContent,
		},
		{
			name:  "caption above rune limit",
			media: validTelegramMedia(1), bytes: []byte{1}, caption: strings.Repeat("界", MaxCaptionRunes+1),
			want: ErrCaptionTooLong,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := newMemoryStore()
			draft := ingestingDraft(t, 701, 702, -7001)
			store.addMember(draft.SourceChatID, domain.User{TelegramUserID: draft.RecipientID})
			options := validServiceOptions()
			options.MaxMediaBytes = 4
			service, _ := newTestService(t, store, options)
			mediaBytes := append([]byte(nil), test.bytes...)

			_, err := service.FinalizeMedia(context.Background(), draft, test.media, mediaBytes, test.caption)
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("FinalizeMedia() error = %v, want %v", err, test.want)
			}
			if test.want == nil && err != nil {
				t.Fatalf("FinalizeMedia() error = %v", err)
			}
			if !allZero(mediaBytes) {
				t.Fatal("FinalizeMedia() did not zero input after validation/finalization")
			}
			_, finalized := store.lastFinalization()
			if finalized != test.finalize {
				t.Fatalf("repository finalization = %t, want %t", finalized, test.finalize)
			}
		})
	}
}

func TestFinalizeTextEnforcesWhitespaceAndRuneLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want error
	}{
		{name: "whitespace", text: " \n\t ", want: ErrUnsupportedContent},
		{name: "one rune too many", text: strings.Repeat("界", MaxSecretTextRunes+1), want: ErrTextTooLong},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := newMemoryStore()
			draft := ingestingDraft(t, 801, 802, -8001)
			service, _ := newTestService(t, store, validServiceOptions())
			_, err := service.FinalizeText(context.Background(), draft, test.text)
			if !errors.Is(err, test.want) {
				t.Fatalf("FinalizeText() error = %v, want %v", err, test.want)
			}
			if _, finalized := store.lastFinalization(); finalized {
				t.Fatal("invalid text reached repository finalization")
			}
		})
	}
}

func ingestingDraft(t *testing.T, senderID, recipientID, chatID int64) domain.Draft {
	t.Helper()
	composeHash := sha256.Sum256([]byte("compose-draft" + uuid.NewString()))
	draft, err := domain.NewDraft(domain.NewDraftParams{
		ComposeTokenHash: composeHash[:],
		SenderID:         senderID, RecipientID: recipientID, SourceChatID: chatID,
		CreatedAt: serviceTestNow, ExpiresAt: serviceTestNow.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("NewDraft() error = %v", err)
	}
	ingestStarted := serviceTestNow
	ingestLease := serviceTestNow.Add(2 * time.Minute)
	draft.State = domain.DraftIngestingMedia
	draft.IngestStartedAt = &ingestStarted
	draft.IngestLeaseUntil = &ingestLease
	if err := draft.Validate(); err != nil {
		t.Fatalf("ingesting draft fixture invalid: %v", err)
	}
	return draft
}

func validTelegramMedia(size int64) domain.MediaReference {
	return domain.MediaReference{
		Provider:    domain.MediaProviderTelegram,
		Type:        domain.MediaVoice,
		Ref:         "telegram-file-id",
		UniqueRef:   "telegram-unique-id",
		ContentType: "audio/ogg",
		SizeBytes:   size,
	}
}

func decryptInput(
	t *testing.T,
	keyring *secretcrypto.Keyring,
	purpose secretcrypto.RecordPurpose,
	whisperID uuid.UUID,
	input repository.EncryptedBlobInput,
) []byte {
	t.Helper()
	aad, err := secretcrypto.AssociatedData(purpose, input.ID, whisperID)
	if err != nil {
		t.Fatalf("AssociatedData(%s) error = %v", purpose, err)
	}
	plaintext, err := keyring.Decrypt(input.Payload.KeyID, input.Payload.Nonce, input.Payload.Ciphertext, aad)
	if err != nil {
		t.Fatalf("decrypt %s input: %v", purpose, err)
	}
	return plaintext
}

func assertFinalizationPolicy(t *testing.T, params repository.FinalizeDraftParams, options Options) {
	t.Helper()
	wantRetention := serviceTestNow.Add(options.ContentRetention)
	if params.Whisper.ContentRetainUntil == nil || params.Whisper.MetadataRetainUntil == nil ||
		!params.Whisper.ContentRetainUntil.Equal(wantRetention) || !params.Whisper.MetadataRetainUntil.Equal(wantRetention) {
		t.Fatalf("whisper retention = %v/%v, want %v", params.Whisper.ContentRetainUntil, params.Whisper.MetadataRetainUntil, wantRetention)
	}
	if params.Whisper.ExpiresAt != serviceTestNow.Add(options.WhisperTTL) ||
		params.Whisper.OneTime != options.DefaultOneTime || params.Whisper.ProtectContent != options.ProtectContent {
		t.Fatalf("whisper policy = expiry %v one_time %t protect %t", params.Whisper.ExpiresAt, params.Whisper.OneTime, params.Whisper.ProtectContent)
	}
	for _, input := range []*repository.EncryptedBlobInput{params.CallbackToken, params.Text, params.Media, params.Caption} {
		if input != nil && !input.RetainUntil.Equal(wantRetention) {
			t.Fatalf("encrypted input retention = %v, want %v", input.RetainUntil, wantRetention)
		}
	}
}

func allZero(value []byte) bool {
	for _, b := range value {
		if b != 0 {
			return false
		}
	}
	return true
}
