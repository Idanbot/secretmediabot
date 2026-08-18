package service

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/command"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/secretcrypto"
)

func TestGuestPayloadEncryptsAndDecryptsWithRequestBoundAAD(t *testing.T) {
	keyring := newServiceTestKeyring(t)
	service := &Service{cipher: keyring}
	requestID := uuid.New()
	plaintext := []byte("guest secret")
	payload, err := service.encryptGuestPayload(secretcrypto.PurposeText, requestID, plaintext, "text/plain; charset=utf-8", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("encryptGuestPayload() error = %v", err)
	}
	stored := repository.StoredEncryptedPayload{
		ID: payload.ID, EncryptionAlgorithm: payload.EncryptionAlgorithm, EncryptionKeyID: payload.EncryptionKeyID,
		Nonce: payload.Nonce, Ciphertext: payload.Ciphertext, CiphertextSHA256: payload.CiphertextSHA256,
		ContentType: payload.ContentType, PlaintextSize: payload.PlaintextSize, RetainUntil: payload.RetainUntil,
	}
	got, err := service.decryptGuestStored(secretcrypto.PurposeText, requestID, stored)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("decryptGuestStored() = %q, %v", got, err)
	}
	if _, err := service.decryptGuestStored(secretcrypto.PurposeText, uuid.New(), stored); err == nil {
		t.Fatal("decryptGuestStored() accepted a payload under a different request ID")
	}
	secretcrypto.Zero(got)
}

func TestGuestPlaintextContentZeroClearsMemory(t *testing.T) {
	t.Parallel()

	content := GuestPlaintextContent{
		Text:    []byte("sensitive text"),
		Caption: []byte("sensitive caption"),
	}
	content.Zero()
	if content.Text != nil || content.Caption != nil {
		t.Fatal("content.Zero() did not clear text and caption pointers")
	}
}

func TestServiceStructsRedactCallbackTokens(t *testing.T) {
	t.Parallel()

	pub := Publication{
		CallbackData: "raw_callback_secret_token_12345",
	}
	if str := pub.String(); bytes.Contains([]byte(str), []byte("raw_callback_secret_token")) || !bytes.Contains([]byte(str), []byte("[REDACTED]")) {
		t.Fatalf("Publication.String() failed to redact token: %s", str)
	}

	whisper := CreatedWhisper{
		CallbackData: "raw_callback_secret_token_67890",
	}
	if str := whisper.String(); bytes.Contains([]byte(str), []byte("raw_callback_secret_token")) || !bytes.Contains([]byte(str), []byte("[REDACTED]")) {
		t.Fatalf("CreatedWhisper.String() failed to redact token: %s", str)
	}
}

type fakeGuestStore struct {
	GuestStore
}

func (f *fakeGuestStore) CreateGuestRequest(ctx context.Context, params repository.GuestCreateParams) (repository.GuestRequest, error) {
	return params.Request, nil
}

func TestCreateGuestRequestRejectsSelfTargeting(t *testing.T) {
	t.Parallel()

	svc := &Service{
		guestStore: &fakeGuestStore{},
		options: Options{
			GuestModeEnabled: true,
		},
		now: time.Now,
	}

	_, err := svc.CreateGuestRequest(context.Background(), CreateGuestRequestParams{
		Sender: domain.User{
			TelegramUserID: 12345,
			Username:       "Alice",
		},
		Target: command.Target{
			Kind:     command.TargetUsername,
			Username: "@alice",
		},
		GuestQueryID: "q1",
	})
	if !errors.Is(err, ErrTargetIsSender) {
		t.Fatalf("expected ErrTargetIsSender, got %v", err)
	}
}
