// Package command parses the small, explicit command surface accepted by the
// bot. It does not inspect arbitrary message content.
package command

import (
	"errors"
	"strconv"
	"strings"
	"unicode"
)

var (
	ErrInvalidTarget = errors.New("target must be a positive Telegram user ID or @username")
	ErrOtherBot      = errors.New("command is addressed to another bot")
)

type Command struct {
	Name string
	Args string
}

// Parse returns false for ordinary text. Telegram command suffixes are matched
// case-insensitively so /help@ThisBot is accepted but commands for another bot
// are ignored.
func Parse(text, botUsername string) (Command, bool, error) {
	text = strings.TrimSpace(text)
	if text == "" || text[0] != '/' {
		return Command{}, false, nil
	}

	first, rest := text, ""
	if split := strings.IndexFunc(text, unicode.IsSpace); split >= 0 {
		first, rest = text[:split], strings.TrimSpace(text[split:])
	}
	first = strings.TrimPrefix(first, "/")
	name, addressedTo, hasAddress := strings.Cut(first, "@")
	if name == "" {
		return Command{}, false, nil
	}
	if hasAddress && !strings.EqualFold(addressedTo, strings.TrimPrefix(botUsername, "@")) {
		return Command{}, false, ErrOtherBot
	}

	return Command{Name: strings.ToLower(name), Args: rest}, true, nil
}

type TargetKind string

const (
	TargetUserID   TargetKind = "user_id"
	TargetUsername TargetKind = "username"
)

type Target struct {
	Kind     TargetKind
	UserID   int64
	Username string
}

func ParseTarget(value string) (Target, error) {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "[]<>\"'()")
	value = strings.TrimPrefix(value, "id:")
	value = strings.TrimPrefix(value, "ID:")
	value = strings.TrimSpace(value)
	if value == "" {
		return Target{}, ErrInvalidTarget
	}
	if strings.HasPrefix(value, "@") {
		username := strings.TrimPrefix(value, "@")
		if !validUsername(username) {
			return Target{}, ErrInvalidTarget
		}
		return Target{Kind: TargetUsername, Username: strings.ToLower(username)}, nil
	}

	id, err := strconv.ParseInt(value, 10, 64)
	if err == nil && id > 0 {
		return Target{Kind: TargetUserID, UserID: id}, nil
	}

	if validUsername(value) {
		return Target{Kind: TargetUsername, Username: strings.ToLower(value)}, nil
	}

	return Target{}, ErrInvalidTarget
}

func validUsername(value string) bool {
	// Telegram usernames are 5-32 ASCII letters, digits, or underscores,
	// starting with a letter.
	if len(value) < 5 || len(value) > 32 {
		return false
	}
	first := value[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z')) {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			continue
		}
		return false
	}
	return true
}
