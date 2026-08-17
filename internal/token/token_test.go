package token

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestGenerateProducesParseableUniqueTokens(t *testing.T) {
	t.Parallel()
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		generated, err := Generate()
		if err != nil {
			t.Fatalf("Generate() error = %v", err)
		}
		if len(generated.Data) > MaxCallbackDataLength {
			t.Fatalf("callback length = %d", len(generated.Data))
		}
		raw, err := ParseCallbackData(generated.Data)
		if err != nil {
			t.Fatalf("ParseCallbackData() error = %v", err)
		}
		if raw != generated.Raw {
			t.Fatalf("raw = %q, want %q", raw, generated.Raw)
		}
		if generated.Hash != sha256.Sum256([]byte(raw)) {
			t.Fatal("generated hash does not match token")
		}
		if _, duplicate := seen[raw]; duplicate {
			t.Fatal("duplicate random token")
		}
		seen[raw] = struct{}{}
	}
}

func TestGenerateUsesAllReaderBytes(t *testing.T) {
	t.Parallel()
	input := bytes.Repeat([]byte{0xa5}, RawTokenBytes)
	generated, err := generate(bytes.NewReader(input))
	if err != nil {
		t.Fatalf("generate() error = %v", err)
	}
	want := base64.RawURLEncoding.EncodeToString(input)
	if generated.Raw != want {
		t.Fatalf("raw = %q, want %q", generated.Raw, want)
	}
}

func TestCallbackTokenFormattingIsRedacted(t *testing.T) {
	t.Parallel()
	generated, err := generate(bytes.NewReader(bytes.Repeat([]byte{0xa5}, RawTokenBytes)))
	if err != nil {
		t.Fatal(err)
	}
	for _, formatted := range []string{fmt.Sprintf("%v", generated), fmt.Sprintf("%#v", generated)} {
		if formatted != "[REDACTED]" || strings.Contains(formatted, generated.Raw) {
			t.Fatalf("formatted token was not redacted: %q", formatted)
		}
	}
}

func TestGeneratePropagatesEntropyFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("entropy unavailable")
	_, err := generate(errorReader{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestParseCallbackDataRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	validRaw := base64.RawURLEncoding.EncodeToString(make([]byte, RawTokenBytes))
	nonCanonical := validRaw[:len(validRaw)-1] + "B" // non-zero trailing pad bits
	tests := []struct {
		name string
		data string
		want error
	}{
		{name: "empty", data: "", want: ErrInvalidPrefix},
		{name: "wrong prefix", data: "x:" + validRaw, want: ErrInvalidPrefix},
		{name: "empty token", data: CallbackPrefix, want: ErrInvalidTokenLength},
		{name: "too short", data: CallbackPrefix + validRaw[:len(validRaw)-1], want: ErrInvalidTokenLength},
		{name: "padding", data: CallbackPrefix + validRaw + "=", want: ErrInvalidTokenLength},
		{name: "standard alphabet", data: CallbackPrefix + "+" + validRaw[1:], want: ErrInvalidTokenEncoding},
		{name: "whitespace", data: CallbackPrefix + "\n" + validRaw[1:], want: ErrInvalidTokenEncoding},
		{name: "non canonical", data: CallbackPrefix + nonCanonical, want: ErrInvalidTokenEncoding},
		{name: "oversized", data: CallbackPrefix + strings.Repeat("A", MaxCallbackDataLength), want: ErrCallbackDataTooLong},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseCallbackData(testCase.data)
			if !errors.Is(err, testCase.want) {
				t.Fatalf("error = %v, want %v", err, testCase.want)
			}
		})
	}
}

func TestParseAndHash(t *testing.T) {
	t.Parallel()
	raw := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, RawTokenBytes))
	got, err := ParseAndHash(CallbackPrefix + raw)
	if err != nil {
		t.Fatalf("ParseAndHash() error = %v", err)
	}
	if want := Hash(raw); got != want {
		t.Fatalf("hash = %x, want %x", got, want)
	}
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

var _ io.Reader = errorReader{}
