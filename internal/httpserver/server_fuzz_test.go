package httpserver

import (
	"testing"
)

func FuzzConstantTimeEqual(f *testing.F) {
	seeds := [][2]string{
		{"secret_token_12345", "secret_token_12345"},
		{"secret_token_12345", "different_token"},
		{"", ""},
		{"a", "a"},
		{"a", "b"},
		{"long_secret_string_1234567890", "long_secret_string_1234567890"},
		{"prefix_match", "prefix"},
	}
	for _, seed := range seeds {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, a, b string) {
		got := constantTimeEqual(a, b)
		want := a != "" && b != "" && a == b
		if got != want {
			t.Fatalf("constantTimeEqual(%q, %q) = %v, want %v", a, b, got, want)
		}
	})
}
