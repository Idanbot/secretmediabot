package secretcrypto

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestKeyringRoundTrip(t *testing.T) {
	t.Parallel()

	key := bytes.Repeat([]byte{0x42}, KeySize)
	keyring, err := NewKeyring("2026-08", map[string][]byte{"2026-08": key})
	if err != nil {
		t.Fatalf("NewKeyring() error = %v", err)
	}

	plaintext := []byte("secret voice bytes")
	aad := []byte("blob:019123")
	encrypted, err := keyring.Encrypt(plaintext, aad)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	if bytes.Contains(encrypted.Ciphertext, plaintext) {
		t.Fatal("ciphertext contains plaintext")
	}

	decrypted, err := keyring.Decrypt(encrypted.KeyID, encrypted.Nonce, encrypted.Ciphertext, aad)
	if err != nil {
		t.Fatalf("Decrypt() error = %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("Decrypt() = %q, want %q", decrypted, plaintext)
	}
}

func TestAssociatedDataSeparatesRowsAndPurposes(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("0198b020-c67b-7c93-89a4-194c10fa1dc1")
	media, err := AssociatedData(PurposeMedia, id)
	if err != nil {
		t.Fatalf("AssociatedData() error = %v", err)
	}
	text, err := AssociatedData(PurposeText, id)
	if err != nil {
		t.Fatalf("AssociatedData() error = %v", err)
	}
	if bytes.Equal(media, text) {
		t.Fatal("different purposes produced the same associated data")
	}
	scoped, err := AssociatedData(PurposeMedia, id, uuid.MustParse("0198b021-15f2-72b3-ae8b-922be90bc8af"))
	if err != nil {
		t.Fatalf("AssociatedData(scoped) error = %v", err)
	}
	if bytes.Equal(media, scoped) {
		t.Fatal("scope did not change associated data")
	}
	if _, err := AssociatedData(PurposeMedia, uuid.Nil); err == nil {
		t.Fatal("AssociatedData() accepted a nil UUID")
	}
}

func TestKeyringRejectsWrongAADAndUnknownKey(t *testing.T) {
	t.Parallel()

	keyring, err := NewKeyring("active", map[string][]byte{"active": make([]byte, KeySize)})
	if err != nil {
		t.Fatalf("NewKeyring() error = %v", err)
	}
	encrypted, err := keyring.Encrypt([]byte("secret"), []byte("correct-row"))
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	if _, err := keyring.Decrypt(encrypted.KeyID, encrypted.Nonce, encrypted.Ciphertext, []byte("wrong-row")); err == nil {
		t.Fatal("Decrypt() with wrong associated data unexpectedly succeeded")
	}
	if _, err := keyring.Decrypt("missing", encrypted.Nonce, encrypted.Ciphertext, nil); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Decrypt() error = %v, want ErrUnknownKey", err)
	}
}

func TestNewKeyringValidatesKeys(t *testing.T) {
	t.Parallel()

	if _, err := NewKeyring("short", map[string][]byte{"short": {1, 2, 3}}); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("NewKeyring() error = %v, want ErrInvalidKey", err)
	}
}

func TestKeyringMultiKeyRotationRoundTrip(t *testing.T) {
	t.Parallel()

	oldKey := bytes.Repeat([]byte{0x11}, KeySize)
	newKey := bytes.Repeat([]byte{0x22}, KeySize)

	oldRing, err := NewKeyring("v1", map[string][]byte{"v1": oldKey})
	if err != nil {
		t.Fatalf("NewKeyring(v1) error = %v", err)
	}

	plaintext := []byte("secret payload from yesterday")
	aad := []byte("blob:019123")
	encryptedOld, err := oldRing.Encrypt(plaintext, aad)
	if err != nil {
		t.Fatalf("oldRing.Encrypt() error = %v", err)
	}

	// Rotated keyring has v2 active, with v1 preserved for previous decryption
	rotatedRing, err := NewKeyring("v2", map[string][]byte{
		"v2": newKey,
		"v1": oldKey,
	})
	if err != nil {
		t.Fatalf("NewKeyring(v2) error = %v", err)
	}

	// Decrypt old ciphertext using the rotated keyring
	decryptedOld, err := rotatedRing.Decrypt(encryptedOld.KeyID, encryptedOld.Nonce, encryptedOld.Ciphertext, aad)
	if err != nil {
		t.Fatalf("rotatedRing.Decrypt(v1) error = %v", err)
	}
	if !bytes.Equal(decryptedOld, plaintext) {
		t.Fatalf("decrypted = %q, want %q", decryptedOld, plaintext)
	}

	// Encrypt new ciphertext using the rotated keyring (should use v2)
	newPlaintext := []byte("secret payload from today")
	encryptedNew, err := rotatedRing.Encrypt(newPlaintext, aad)
	if err != nil {
		t.Fatalf("rotatedRing.Encrypt() error = %v", err)
	}
	if encryptedNew.KeyID != "v2" {
		t.Fatalf("encryptedNew.KeyID = %q, want v2", encryptedNew.KeyID)
	}
	decryptedNew, err := rotatedRing.Decrypt(encryptedNew.KeyID, encryptedNew.Nonce, encryptedNew.Ciphertext, aad)
	if err != nil {
		t.Fatalf("rotatedRing.Decrypt(v2) error = %v", err)
	}
	if !bytes.Equal(decryptedNew, newPlaintext) {
		t.Fatalf("decrypted = %q, want %q", decryptedNew, newPlaintext)
	}
}
