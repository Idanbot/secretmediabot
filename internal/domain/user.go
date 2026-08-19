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

// RecentTarget represents a target recipient recently used by a sender.
type RecentTarget struct {
	TargetUserID   int64
	TargetUsername string
	DisplayName    string
	LastUsedAt     time.Time
}

// TargetIdentifier returns the @username or numeric ID string.
func (r RecentTarget) TargetIdentifier() string {
	if r.TargetUsername != "" {
		if strings.HasPrefix(r.TargetUsername, "@") {
			return r.TargetUsername
		}
		return "@" + r.TargetUsername
	}
	if r.TargetUserID > 0 {
		return strconv.FormatInt(r.TargetUserID, 10)
	}
	return ""
}

// Label returns a user-friendly label like "JoeTheBoss (@joetheboss)" or "Alice".
func (r RecentTarget) Label() string {
	if r.DisplayName != "" {
		if r.TargetUsername != "" && !strings.EqualFold(r.DisplayName, "@"+r.TargetUsername) && !strings.EqualFold(r.DisplayName, r.TargetUsername) {
			return r.DisplayName + " (@" + strings.TrimPrefix(r.TargetUsername, "@") + ")"
		}
		return r.DisplayName
	}
	return r.TargetIdentifier()
}
