// Package secretcrypto encrypts secret payloads before persistence.
package secretcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
)

const (
	KeySize   = 32
	NonceSize = 12
)

var (
	ErrUnknownKey = errors.New("unknown encryption key")
	ErrInvalidKey = errors.New("encryption keys must contain exactly 32 bytes")
)

type RecordPurpose string

const (
	PurposeMedia    RecordPurpose = "media"
	PurposeText     RecordPurpose = "text"
	PurposeCaption  RecordPurpose = "caption"
	PurposeCallback RecordPurpose = "callback"
)

// AssociatedData binds ciphertext to its immutable row and semantic purpose.
// It is not secret and is derived rather than persisted.
func AssociatedData(purpose RecordPurpose, id uuid.UUID, scope ...uuid.UUID) ([]byte, error) {
	if id == uuid.Nil {
		return nil, errors.New("associated-data record ID is required")
	}
	switch purpose {
	case PurposeMedia, PurposeText, PurposeCaption, PurposeCallback:
	default:
		return nil, fmt.Errorf("invalid associated-data purpose %q", purpose)
	}
	parts := []string{"secretsantabot", "v1", string(purpose), id.String()}
	for _, scopeID := range scope {
		if scopeID == uuid.Nil {
			return nil, errors.New("associated-data scope ID is required")
		}
		parts = append(parts, scopeID.String())
	}
	return []byte(strings.Join(parts, ":")), nil
}

// EncryptedPayload contains only values that are safe to persist together.
// Associated data is deliberately not included: callers derive it from the
// immutable blob ID, preventing ciphertext from being moved between rows.
type EncryptedPayload struct {
	KeyID            string
	Nonce            []byte
	Ciphertext       []byte
	CiphertextSHA256 [sha256.Size]byte
}

// Keyring supports decrypting records produced by an older key while using one
// explicitly selected key for all new records.
type Keyring struct {
	activeID string
	aeads    map[string]cipher.AEAD
	random   io.Reader
}

func NewKeyring(activeID string, keys map[string][]byte) (*Keyring, error) {
	if activeID == "" {
		return nil, errors.New("active encryption key ID is required")
	}
	if len(keys) == 0 {
		return nil, errors.New("at least one encryption key is required")
	}

	aeads := make(map[string]cipher.AEAD, len(keys))
	for id, key := range keys {
		if id == "" {
			return nil, errors.New("encryption key ID cannot be empty")
		}
		if len(key) != KeySize {
			return nil, fmt.Errorf("key %q: %w", id, ErrInvalidKey)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("create cipher for key %q: %w", id, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("create GCM for key %q: %w", id, err)
		}
		aeads[id] = aead
	}
	if _, ok := aeads[activeID]; !ok {
		return nil, fmt.Errorf("active key %q: %w", activeID, ErrUnknownKey)
	}

	return &Keyring{activeID: activeID, aeads: aeads, random: rand.Reader}, nil
}

func (k *Keyring) Encrypt(plaintext, associatedData []byte) (EncryptedPayload, error) {
	aead := k.aeads[k.activeID]
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(k.random, nonce); err != nil {
		return EncryptedPayload{}, fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := aead.Seal(nil, nonce, plaintext, associatedData)
	return EncryptedPayload{
		KeyID:            k.activeID,
		Nonce:            nonce,
		Ciphertext:       ciphertext,
		CiphertextSHA256: sha256.Sum256(ciphertext),
	}, nil
}

func (k *Keyring) Decrypt(keyID string, nonce, ciphertext, associatedData []byte) ([]byte, error) {
	aead, ok := k.aeads[keyID]
	if !ok {
		return nil, fmt.Errorf("key %q: %w", keyID, ErrUnknownKey)
	}
	if len(nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("invalid nonce length %d", len(nonce))
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, associatedData)
	if err != nil {
		return nil, fmt.Errorf("decrypt payload: %w", err)
	}
	return plaintext, nil
}

// Zero makes a best effort to shorten how long plaintext remains in memory.
// Go does not guarantee secure memory erasure, so this is defense in depth.
func Zero(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
