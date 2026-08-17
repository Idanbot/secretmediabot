package command

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		text      string
		want      Command
		wantOK    bool
		wantError error
	}{
		{name: "plain text", text: "hello", wantOK: false},
		{name: "simple", text: "/help", want: Command{Name: "help"}, wantOK: true},
		{name: "args", text: "/whisper @Alice_1", want: Command{Name: "whisper", Args: "@Alice_1"}, wantOK: true},
		{name: "whitespace args", text: "/whisper\n\t@Alice_1", want: Command{Name: "whisper", Args: "@Alice_1"}, wantOK: true},
		{name: "addressed", text: "/HELP@SecretBot", want: Command{Name: "help"}, wantOK: true},
		{name: "other bot", text: "/help@OtherBot", wantError: ErrOtherBot},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, ok, err := Parse(test.text, "secretbot")
			if !errors.Is(err, test.wantError) {
				t.Fatalf("Parse() error = %v, want %v", err, test.wantError)
			}
			if got != test.want || ok != test.wantOK {
				t.Fatalf("Parse() = (%+v, %v), want (%+v, %v)", got, ok, test.want, test.wantOK)
			}
		})
	}
}

func TestParseTarget(t *testing.T) {
	t.Parallel()

	username, err := ParseTarget("@Alice_1")
	if err != nil || username.Kind != TargetUsername || username.Username != "alice_1" {
		t.Fatalf("ParseTarget(username) = (%+v, %v)", username, err)
	}
	id, err := ParseTarget("99887766")
	if err != nil || id.Kind != TargetUserID || id.UserID != 99887766 {
		t.Fatalf("ParseTarget(ID) = (%+v, %v)", id, err)
	}
	for _, invalid := range []string{"", "123oops", "-1", "@a", "@invalid-name"} {
		if _, err := ParseTarget(invalid); !errors.Is(err, ErrInvalidTarget) {
			t.Errorf("ParseTarget(%q) error = %v, want ErrInvalidTarget", invalid, err)
		}
	}
}
