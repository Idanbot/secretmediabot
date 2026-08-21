// Package telegram implements the small Telegram Bot API surface used by the
// application. It intentionally does not depend on a third-party Bot API SDK
// so newly introduced API fields can be supported without waiting for a
// wrapper release.
package telegram

import "time"

// EphemeralCallbackWindow is Telegram's eligibility window for sending an
// ephemeral response to the exact client which triggered a callback query.
// Callers must still enforce their own shorter context deadline; the client
// does not retry ephemeral sends.
const EphemeralCallbackWindow = 15 * time.Second

type User struct {
	ID                    int64  `json:"id"`
	IsBot                 bool   `json:"is_bot"`
	FirstName             string `json:"first_name"`
	LastName              string `json:"last_name,omitempty"`
	Username              string `json:"username,omitempty"`
	LanguageCode          string `json:"language_code,omitempty"`
	SupportsGuestQueries  bool   `json:"supports_guest_queries,omitempty"`
	SupportsInlineQueries bool   `json:"supports_inline_queries,omitempty"`
}

type Chat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title,omitempty"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	IsForum   bool   `json:"is_forum,omitempty"`
}

type MessageEntity struct {
	Type          string `json:"type"`
	Offset        int    `json:"offset"`
	Length        int    `json:"length"`
	URL           string `json:"url,omitempty"`
	User          *User  `json:"user,omitempty"`
	Language      string `json:"language,omitempty"`
	CustomEmojiID string `json:"custom_emoji_id,omitempty"`
}

type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int64  `json:"file_size,omitempty"`
}

type Voice struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Duration     int    `json:"duration"`
	MIMEType     string `json:"mime_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

type Video struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Duration     int    `json:"duration"`
	FileName     string `json:"file_name,omitempty"`
	MIMEType     string `json:"mime_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

type Audio struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Duration     int    `json:"duration"`
	Performer    string `json:"performer,omitempty"`
	Title        string `json:"title,omitempty"`
	FileName     string `json:"file_name,omitempty"`
	MIMEType     string `json:"mime_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

type Document struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name,omitempty"`
	MIMEType     string `json:"mime_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

// Message contains the fields used by update handling and media extraction.
// Telegram sets MessageID to zero for ephemeral messages and supplies
// EphemeralMessageID instead.
type Message struct {
	MessageID           int64           `json:"message_id"`
	MessageThreadID     int64           `json:"message_thread_id,omitempty"`
	From                *User           `json:"from,omitempty"`
	SenderChat          *Chat           `json:"sender_chat,omitempty"`
	ReceiverUser        *User           `json:"receiver_user,omitempty"`
	EphemeralMessageID  int64           `json:"ephemeral_message_id,omitempty"`
	GuestQueryID        string          `json:"guest_query_id,omitempty"`
	GuestBotCallerUser  *User           `json:"guest_bot_caller_user,omitempty"`
	GuestBotCallerChat  *Chat           `json:"guest_bot_caller_chat,omitempty"`
	Date                int64           `json:"date"`
	Chat                Chat            `json:"chat"`
	ReplyToMessage      *Message        `json:"reply_to_message,omitempty"`
	Text                string          `json:"text,omitempty"`
	Entities            []MessageEntity `json:"entities,omitempty"`
	Caption             string          `json:"caption,omitempty"`
	CaptionEntities     []MessageEntity `json:"caption_entities,omitempty"`
	Photo               []PhotoSize     `json:"photo,omitempty"`
	Voice               *Voice          `json:"voice,omitempty"`
	Video               *Video          `json:"video,omitempty"`
	Audio               *Audio          `json:"audio,omitempty"`
	Document            *Document       `json:"document,omitempty"`
	MediaGroupID        string          `json:"media_group_id,omitempty"`
	IsTopicMessage      bool            `json:"is_topic_message,omitempty"`
	HasProtectedContent bool            `json:"has_protected_content,omitempty"`
}

type CallbackQuery struct {
	ID              string   `json:"id"`
	From            User     `json:"from"`
	Message         *Message `json:"message,omitempty"`
	InlineMessageID string   `json:"inline_message_id,omitempty"`
	ChatInstance    string   `json:"chat_instance"`
	Data            string   `json:"data,omitempty"`
}

type InlineQuery struct {
	ID       string `json:"id"`
	From     User   `json:"from"`
	ChatType string `json:"chat_type,omitempty"`
	Query    string `json:"query"`
	Offset   string `json:"offset"`
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	EditedMessage *Message       `json:"edited_message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
	GuestMessage  *Message       `json:"guest_message,omitempty"`
	InlineQuery   *InlineQuery   `json:"inline_query,omitempty"`
}

type File struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
}

// ChatMember represents the common fields shared by Telegram's ChatMember
// variants. The status value determines which optional permissions apply.
type ChatMember struct {
	Status             string `json:"status"`
	User               User   `json:"user"`
	UntilDate          int64  `json:"until_date,omitempty"`
	CanManageChat      bool   `json:"can_manage_chat,omitempty"`
	CanDeleteMessages  bool   `json:"can_delete_messages,omitempty"`
	CanRestrictMembers bool   `json:"can_restrict_members,omitempty"`
	IsMember           bool   `json:"is_member,omitempty"`
}

type ResponseParameters struct {
	MigrateToChatID int64 `json:"migrate_to_chat_id,omitempty"`
	RetryAfter      int   `json:"retry_after,omitempty"`
}

type InlineKeyboardButton struct {
	Text         string `json:"text"`
	URL          string `json:"url,omitempty"`
	CallbackData string `json:"callback_data,omitempty"`
}

type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type InputTextMessageContent struct {
	MessageText string `json:"message_text"`
}

type InlineQueryResultArticle struct {
	Type                string                  `json:"type"`
	ID                  string                  `json:"id"`
	Title               string                  `json:"title"`
	Description         string                  `json:"description,omitempty"`
	InputMessageContent InputTextMessageContent `json:"input_message_content"`
	ReplyMarkup         *InlineKeyboardMarkup   `json:"reply_markup,omitempty"`
}

type ReplyParameters struct {
	MessageID          *int64 `json:"message_id,omitempty"`
	EphemeralMessageID int64  `json:"ephemeral_message_id,omitempty"`
}

type GetUpdatesRequest struct {
	Offset         int64
	Limit          int
	Timeout        time.Duration
	AllowedUpdates []string
}

type SendMessageRequest struct {
	ChatID              int64                 `json:"chat_id"`
	MessageThreadID     *int64                `json:"message_thread_id,omitempty"`
	ReceiverUserID      int64                 `json:"receiver_user_id,omitempty"`
	CallbackQueryID     string                `json:"callback_query_id,omitempty"`
	Text                string                `json:"text"`
	ParseMode           string                `json:"parse_mode,omitempty"`
	Entities            []MessageEntity       `json:"entities,omitempty"`
	DisableNotification bool                  `json:"disable_notification,omitempty"`
	ProtectContent      bool                  `json:"protect_content,omitempty"`
	ReplyParameters     *ReplyParameters      `json:"reply_parameters,omitempty"`
	ReplyMarkup         *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

type EditMessageTextRequest struct {
	ChatID      int64                 `json:"chat_id"`
	MessageID   int64                 `json:"message_id"`
	Text        string                `json:"text"`
	ParseMode   string                `json:"parse_mode,omitempty"`
	ReplyMarkup *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

type SendEphemeralTextRequest struct {
	ChatID          int64
	MessageThreadID *int64
	ReceiverUserID  int64
	CallbackQueryID string
	Text            string
	ProtectContent  bool
	ReplyMarkup     *InlineKeyboardMarkup
}

type AnswerCallbackQueryRequest struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
	ShowAlert       bool   `json:"show_alert,omitempty"`
	URL             string `json:"url,omitempty"`
	CacheTime       int    `json:"cache_time,omitempty"`
}

type GetChatMemberRequest struct {
	ChatID int64 `json:"chat_id"`
	UserID int64 `json:"user_id"`
}

type GetFileRequest struct {
	FileID string `json:"file_id"`
}

type SetWebhookRequest struct {
	URL                string   `json:"url"`
	SecretToken        string   `json:"secret_token,omitempty"`
	AllowedUpdates     []string `json:"allowed_updates,omitempty"`
	DropPendingUpdates bool     `json:"drop_pending_updates,omitempty"`
	MaxConnections     int      `json:"max_connections,omitempty"`
}

type DeleteWebhookRequest struct {
	DropPendingUpdates bool `json:"drop_pending_updates,omitempty"`
}

type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
	IsEphemeral bool   `json:"is_ephemeral,omitempty"`
}

type SetMyCommandsRequest struct {
	Commands []BotCommand `json:"commands"`
}

type DeleteEphemeralMessageRequest struct {
	ChatID             int64 `json:"chat_id"`
	ReceiverUserID     int64 `json:"receiver_user_id"`
	EphemeralMessageID int64 `json:"ephemeral_message_id"`
}

type DeleteMessageRequest struct {
	ChatID    int64 `json:"chat_id"`
	MessageID int64 `json:"message_id"`
}

type AnswerGuestQueryRequest struct {
	GuestQueryID string                   `json:"guest_query_id"`
	Result       InlineQueryResultArticle `json:"result"`
}

type AnswerInlineQueryRequest struct {
	InlineQueryID string                     `json:"inline_query_id"`
	Results       []InlineQueryResultArticle `json:"results"`
	CacheTime     int                        `json:"cache_time,omitempty"`
	IsPersonal    bool                       `json:"is_personal,omitempty"`
}

type SentGuestMessage struct {
	InlineMessageID string `json:"inline_message_id"`
}
