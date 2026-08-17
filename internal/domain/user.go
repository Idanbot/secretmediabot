package domain

import (
	"strconv"
	"strings"
	"time"
)

// User is a Telegram user observed by the bot. TelegramUserID is the stable
// identity; Username is mutable and must only be used as a lookup hint.
type User struct {
	TelegramUserID        int64
	Username              string
	FirstName             string
	LastName              string
	LanguageCode          string
	IsBot                 bool
	HasStartedPrivateChat bool
	FirstSeenAt           time.Time
	LastSeenAt            time.Time
	UpdatedAt             time.Time
}

// DisplayName returns the most useful non-secret label available for a user.
func (u User) DisplayName() string {
	name := strings.TrimSpace(strings.TrimSpace(u.FirstName) + " " + strings.TrimSpace(u.LastName))
	if name != "" {
		return name
	}

	if username := strings.TrimLeft(strings.TrimSpace(u.Username), "@"); username != "" {
		return "@" + username
	}

	if u.TelegramUserID != 0 {
		return strconv.FormatInt(u.TelegramUserID, 10)
	}

	return "Unknown user"
}

// NormalizedUsername returns a username suitable for case-insensitive cache
// lookup. It deliberately does not make a username an identity.
func (u User) NormalizedUsername() string {
	return strings.ToLower(strings.TrimLeft(strings.TrimSpace(u.Username), "@"))
}
