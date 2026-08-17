// Package token creates and validates opaque Telegram callback tokens.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"strings"
)

const (
	CallbackPrefix        = "w:"
	RawTokenBytes         = 32
	EncodedTokenLength    = (RawTokenBytes*8 + 5) / 6
	MaxCallbackDataLength = 64
)

var (
	ErrCallbackDataTooLong  = errors.New("callback data exceeds Telegram's size limit")
	ErrInvalidPrefix        = errors.New("invalid callback token prefix")
	ErrInvalidTokenLength   = errors.New("invalid callback token length")
	ErrInvalidTokenEncoding = errors.New("invalid callback token encoding")
)

// CallbackToken contains the value sent to Telegram and the digest persisted
// in PostgreSQL. Raw and Data are secrets and must not be logged or stored.
type CallbackToken struct {
	Data string            `json:"-"`
	Raw  string            `json:"-"`
	Hash [sha256.Size]byte `json:"-"`
}

func (CallbackToken) String() string   { return "[REDACTED]" }
func (CallbackToken) GoString() string { return "[REDACTED]" }

// Generate creates a 256-bit token using the operating system CSPRNG.
func Generate() (CallbackToken, error) {
	return generate(rand.Reader)
}

func generate(random io.Reader) (CallbackToken, error) {
	bytes := make([]byte, RawTokenBytes)
	if _, err := io.ReadFull(random, bytes); err != nil {
		return CallbackToken{}, err
	}

	raw := base64.RawURLEncoding.EncodeToString(bytes)
	return CallbackToken{
		Data: CallbackPrefix + raw,
		Raw:  raw,
		Hash: Hash(raw),
	}, nil
}

// ParseCallbackData accepts only the canonical w:<base64url> representation
// of exactly 32 bytes. In particular, padding, alternate alphabets, ignored
// whitespace and non-zero trailing pad bits are rejected.
func ParseCallbackData(data string) (string, error) {
	if len(data) > MaxCallbackDataLength {
		return "", ErrCallbackDataTooLong
	}
	if !strings.HasPrefix(data, CallbackPrefix) {
		return "", ErrInvalidPrefix
	}

	raw := strings.TrimPrefix(data, CallbackPrefix)
	if len(raw) != EncodedTokenLength {
		return "", ErrInvalidTokenLength
	}
	for i := 0; i < len(raw); i++ {
		char := raw[i]
		if !((char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_') {
			return "", ErrInvalidTokenEncoding
		}
	}

	decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil {
		return "", ErrInvalidTokenEncoding
	}
	if len(decoded) != RawTokenBytes {
		return "", ErrInvalidTokenLength
	}
	if base64.RawURLEncoding.EncodeToString(decoded) != raw {
		return "", ErrInvalidTokenEncoding
	}

	return raw, nil
}

// Hash returns the non-reversible value stored in PostgreSQL. The digest is
// computed over the canonical base64url token, matching callback parsing.
func Hash(raw string) [sha256.Size]byte {
	return sha256.Sum256([]byte(raw))
}

func ParseAndHash(data string) ([sha256.Size]byte, error) {
	raw, err := ParseCallbackData(data)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return Hash(raw), nil
}
