package token_test

import (
	"testing"

	"github.com/idan/secretmediabot/internal/token"
)

func FuzzParseCallbackData(f *testing.F) {
	for i := 0; i < 5; i++ {
		tok, err := token.Generate()
		if err == nil {
			f.Add(tok.Data)
		}
	}
	f.Add("")
	f.Add("short")
	f.Add("invalid_base64_chars!@#$%^&*()")
	f.Add("w:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	f.Add("\x00\x01\x02\x03\x04\x05")

	f.Fuzz(func(t *testing.T, raw string) {
		parsed, err := token.ParseCallbackData(raw)
		if err == nil && len(parsed) == 0 {
			t.Fatalf("ParseCallbackData succeeded but produced empty raw token for %q", raw)
		}
	})
}
