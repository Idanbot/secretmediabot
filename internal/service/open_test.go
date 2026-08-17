package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/secretcrypto"
	"github.com/idan/secretmediabot/internal/token"
)

func TestReserveOpenRejectsNonCanonicalTokensBeforeRepositoryAccess(t *testing.T) {
	t.Parallel()

	generated, err := token.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	tests := []string{
		"",
		generated.Raw,
		"x:" + generated.Raw,
		"w:short",
		generated.Data + "=",
		" " + generated.Data,
	}
	for _, callbackData := range tests {
		callbackData := callbackData
		t.Run(fmt.Sprintf("length_%d", len(callbackData)), func(t *testing.T) {
			t.Parallel()
			store := newMemoryStore()
			service, _ := newTestService(t, store, validServiceOptions())
			_, err := service.ReserveOpen(context.Background(), callbackData, 1001, "query")
			if !errors.Is(err, ErrInvalidOpenToken) {
				t.Fatalf("ReserveOpen(%q) error = %v, want ErrInvalidOpenToken", callbackData, err)
			}
			if store.reservedCount() != 0 {
				t.Fatal("invalid callback token reached repository reservation")
			}
		})
	}
}

func TestReserveOpenMapsRecipientLifecycleErrors(t *testing.T) {
	t.Parallel()

	generated, err := token.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	tests := []struct {
		name       string
		repository error
		want       error
	}{
		{name: "not found", repository: repository.ErrNotFound, want: ErrWhisperNotFound},
		{name: "wrong recipient", repository: repository.ErrUnauthorized, want: ErrWrongRecipient},
		{name: "expired", repository: repository.ErrExpired, want: ErrWhisperExpired},
		{name: "already opened", repository: repository.ErrAlreadyOpened, want: ErrWhisperAlreadyOpened},
		{name: "not active", repository: repository.ErrNotActive, want: ErrWhisperUnavailable},
		{name: "reservation conflict", repository: repository.ErrConflict, want: ErrWhisperUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := newMemoryStore()
			store.reserveErr = fmt.Errorf("repository boundary: %w", test.repository)
			service, _ := newTestService(t, store, validServiceOptions())
			_, err := service.ReserveOpen(context.Background(), generated.Data, 2001, "callback-query")
			if !errors.Is(err, test.want) {
				t.Fatalf("ReserveOpen() error = %v, want %v", err, test.want)
			}
			if store.reservedCount() != 1 {
				t.Fatal("valid callback token did not reach repository exactly once")
			}
			params := store.reserved[0]
			if !bytes.Equal(params.OpenTokenHash, generated.Hash[:]) || params.TelegramUserID != 2001 ||
				params.CallbackQueryID != "callback-query" || params.Now != serviceTestNow ||
				params.LeaseUntil != serviceTestNow.Add(validServiceOptions().OpenLease) {
				t.Fatalf("reservation params = %#v", params)
			}
		})
	}
}

func TestReserveOpenDecryptsTextAndZeroClearsDelivery(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	service, keyring := newTestService(t, store, validServiceOptions())
	generated, err := token.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	whisperID := uuid.New()
	stored := storedPayload(t, keyring, secretcrypto.PurposeText, whisperID, []byte("recipient plaintext"), "text/plain")
	store.reservation = repository.OpenReservation{
		Whisper: domain.Whisper{ID: whisperID, RecipientID: 3001},
		EventID: 88,
		Content: repository.DeliveryContent{Kind: domain.PayloadText, Text: &stored},
	}

	delivery, err := service.ReserveOpen(context.Background(), generated.Data, 3001, "callback-text")
	if err != nil {
		t.Fatalf("ReserveOpen() error = %v", err)
	}
	if delivery.EventID != 88 || delivery.CallbackQueryID != "callback-text" ||
		delivery.Content.Kind != domain.PayloadText || string(delivery.Content.Text) != "recipient plaintext" {
		t.Fatalf("open delivery = %#v", delivery)
	}
	plaintextAlias := delivery.Content.Text
	delivery.Content.Zero()
	if delivery.Content.Text != nil || !allZero(plaintextAlias) {
		t.Fatal("PlaintextContent.Zero() did not clear and zero recipient plaintext")
	}
}

func TestReserveOpenFailsReservedEventWhenCiphertextIsCorrupt(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	service, keyring := newTestService(t, store, validServiceOptions())
	generated, err := token.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	whisperID := uuid.New()
	stored := storedPayload(t, keyring, secretcrypto.PurposeText, whisperID, []byte("secret"), "text/plain")
	stored.CiphertextSHA256[0] ^= 0xff
	store.reservation = repository.OpenReservation{
		Whisper: domain.Whisper{ID: whisperID},
		EventID: 99,
		Content: repository.DeliveryContent{Kind: domain.PayloadText, Text: &stored},
	}

	_, err = service.ReserveOpen(context.Background(), generated.Data, 4001, "callback-corrupt")
	if !errors.Is(err, ErrCorruptCiphertext) {
		t.Fatalf("ReserveOpen() error = %v, want ErrCorruptCiphertext", err)
	}
	if len(store.failed) != 1 || store.failed[0].WhisperID != whisperID ||
		store.failed[0].EventID != 99 || store.failed[0].ErrorCode != "decrypt_text_failed" {
		t.Fatalf("failed reservation record = %#v", store.failed)
	}
}

func storedPayload(
	t *testing.T,
	keyring *secretcrypto.Keyring,
	purpose secretcrypto.RecordPurpose,
	whisperID uuid.UUID,
	plaintext []byte,
	contentType string,
) repository.StoredEncryptedPayload {
	t.Helper()
	id := uuid.New()
	aad, err := secretcrypto.AssociatedData(purpose, id, whisperID)
	if err != nil {
		t.Fatalf("AssociatedData(%s) error = %v", purpose, err)
	}
	encrypted, err := keyring.Encrypt(plaintext, aad)
	if err != nil {
		t.Fatalf("Encrypt(%s) error = %v", purpose, err)
	}
	return repository.StoredEncryptedPayload{
		ID: id, EncryptionAlgorithm: "AES-256-GCM", EncryptionKeyID: encrypted.KeyID,
		Nonce: append([]byte(nil), encrypted.Nonce...), Ciphertext: append([]byte(nil), encrypted.Ciphertext...),
		CiphertextSHA256: append([]byte(nil), encrypted.CiphertextSHA256[:]...),
		ContentType:      contentType, PlaintextSize: int64(len(plaintext)),
		RetainUntil: serviceTestNow.Add(30 * 24 * time.Hour),
	}
}
