package service

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/secretcrypto"
)

var ErrCorruptCiphertext = errors.New("stored ciphertext failed integrity validation")

func (s *Service) decryptStored(
	purpose secretcrypto.RecordPurpose,
	whisperID uuid.UUID,
	payload repository.StoredEncryptedPayload,
) ([]byte, error) {
	if payload.EncryptionAlgorithm != "AES-256-GCM" || payload.PlaintextSize < 0 {
		return nil, ErrCorruptCiphertext
	}
	digest := sha256.Sum256(payload.Ciphertext)
	if len(payload.CiphertextSHA256) != sha256.Size ||
		subtle.ConstantTimeCompare(digest[:], payload.CiphertextSHA256) != 1 {
		return nil, ErrCorruptCiphertext
	}
	aad, err := secretcrypto.AssociatedData(purpose, payload.ID, whisperID)
	if err != nil {
		return nil, err
	}
	plaintext, err := s.cipher.Decrypt(payload.EncryptionKeyID, payload.Nonce, payload.Ciphertext, aad)
	if err != nil {
		return nil, err
	}
	if int64(len(plaintext)) != payload.PlaintextSize {
		secretcrypto.Zero(plaintext)
		return nil, ErrCorruptCiphertext
	}
	return plaintext, nil
}
