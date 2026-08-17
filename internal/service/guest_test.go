package service

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"
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
