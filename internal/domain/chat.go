package domain

import "time"

type ChatType string

const (
	ChatTypePrivate    ChatType = "private"
	ChatTypeGroup      ChatType = "group"
	ChatTypeSupergroup ChatType = "supergroup"
	ChatTypeChannel    ChatType = "channel"
)

func (t ChatType) IsValid() bool {
	switch t {
	case ChatTypePrivate, ChatTypeGroup, ChatTypeSupergroup, ChatTypeChannel:
		return true
	default:
		return false
	}
}

func (t ChatType) SupportsWhispers() bool {
	return t == ChatTypeGroup || t == ChatTypeSupergroup
}

type Chat struct {
	TelegramChatID int64
	Type           ChatType
	Title          string
	Username       string
	FirstSeenAt    time.Time
	LastSeenAt     time.Time
	UpdatedAt      time.Time
}

// ObservedChatMember scopes username and numeric-ID targeting to members the
// bot has actually observed in a specific chat.
type ObservedChatMember struct {
	ChatID      int64
	UserID      int64
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}
