package command_test

import (
	"testing"

	"github.com/idan/secretmediabot/internal/command"
)

func FuzzParse(f *testing.F) {
	seeds := []string{
		"/whisper",
		"/whisper @alice",
		"/whisper 123456789",
		"/whisper@secretmediabot",
		"/whisper@secretmediabot @bob",
		"/start compose_123456",
		"/cancel",
		"/help",
		"/privacy",
		"/owner_list 10",
		"",
		" ",
		"\x00\xff\xfe",
		"/unknown_command with arguments",
		"//@//\n\r\t",
	}
	for _, seed := range seeds {
		f.Add(seed, "secretmediabot")
	}

	f.Fuzz(func(t *testing.T, text, botUsername string) {
		cmd, matched, err := command.Parse(text, botUsername)
		_ = cmd.Name
		_ = cmd.Args
		_ = matched
		_ = err
	})
}

func FuzzParseTarget(f *testing.F) {
	seeds := []string{
		"@username",
		"@alice_bob_123",
		"123456789",
		"-100123456",
		"0",
		"invalid target with spaces",
		"@@invalid",
		"@999",
		"\x00\x01\x02",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		target, err := command.ParseTarget(raw)
		if err == nil {
			if target.Kind == command.TargetUserID && target.UserID <= 0 {
				t.Fatalf("ParseTarget produced non-positive user ID %d for input %q", target.UserID, raw)
			}
			if target.Kind == command.TargetUsername && target.Username == "" {
				t.Fatalf("ParseTarget produced empty username for input %q", raw)
			}
		}
	})
}
