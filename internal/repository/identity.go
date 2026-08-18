package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/idan/secretmediabot/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Store) ObserveMembership(ctx context.Context, params ObserveMembershipParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	seenAt := nowOr(params.SeenAt)
	if params.User.TelegramUserID <= 0 || params.Chat.TelegramChatID == 0 || !params.Chat.Type.IsValid() {
		return fmt.Errorf("%w: invalid observed user or chat", ErrInvalidInput)
	}

	return translateError(db.Transaction(func(tx *gorm.DB) error {
		if err := upsertUser(tx, params.User, seenAt); err != nil {
			return err
		}
		if err := upsertChat(tx, params.Chat, seenAt); err != nil {
			return err
		}
		return upsertChatMember(tx, params.Chat.TelegramChatID, params.User.TelegramUserID, seenAt)
	}))
}

func (s *Store) ObserveUser(ctx context.Context, user domain.User, seenAt time.Time) (domain.User, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return domain.User{}, err
	}
	if user.TelegramUserID <= 0 {
		return domain.User{}, fmt.Errorf("%w: Telegram user ID must be positive", ErrInvalidInput)
	}
	seenAt = nowOr(seenAt)
	if err := upsertUser(db, user, seenAt); err != nil {
		return domain.User{}, translateError(err)
	}
	var row userRow
	if err := db.Where("telegram_user_id = ?", user.TelegramUserID).Take(&row).Error; err != nil {
		return domain.User{}, translateError(err)
	}
	return row.toDomain(), nil
}

func (s *Store) ObserveChat(ctx context.Context, chat domain.Chat, seenAt time.Time) (domain.Chat, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return domain.Chat{}, err
	}
	if chat.TelegramChatID == 0 || !chat.Type.IsValid() {
		return domain.Chat{}, fmt.Errorf("%w: invalid Telegram chat", ErrInvalidInput)
	}
	seenAt = nowOr(seenAt)
	if err := upsertChat(db, chat, seenAt); err != nil {
		return domain.Chat{}, translateError(err)
	}
	var row chatRow
	if err := db.Where("telegram_chat_id = ?", chat.TelegramChatID).Take(&row).Error; err != nil {
		return domain.Chat{}, translateError(err)
	}
	return row.toDomain(), nil
}

func (s *Store) ObserveChatMember(ctx context.Context, params ObserveChatMemberParams) error {
	db, err := s.withContext(ctx)
	if err != nil {
		return err
	}
	if params.ChatID == 0 || params.UserID <= 0 {
		return fmt.Errorf("%w: invalid chat member identity", ErrInvalidInput)
	}
	return translateError(upsertChatMember(db, params.ChatID, params.UserID, nowOr(params.SeenAt)))
}

func (s *Store) FindObservedUserByID(ctx context.Context, chatID, userID int64) (domain.User, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return domain.User{}, err
	}
	if chatID == 0 || userID <= 0 {
		return domain.User{}, fmt.Errorf("%w: invalid observed user lookup", ErrInvalidInput)
	}
	var row userRow
	err = db.Table("users AS u").
		Select("u.*").
		Joins("JOIN observed_chat_members o ON o.user_id = u.telegram_user_id").
		Where("o.chat_id = ? AND u.telegram_user_id = ?", chatID, userID).
		Take(&row).Error
	if err != nil {
		return domain.User{}, translateError(err)
	}
	return row.toDomain(), nil
}

func (s *Store) FindObservedUserByUsername(ctx context.Context, chatID int64, username string) (domain.User, error) {
	db, err := s.withContext(ctx)
	if err != nil {
		return domain.User{}, err
	}
	normalized := normalizeUsername(username)
	if chatID == 0 || normalized == "" {
		return domain.User{}, fmt.Errorf("%w: invalid observed username lookup", ErrInvalidInput)
	}

	var rows []userRow
	err = db.Table("users AS u").
		Select("u.*").
		Joins("JOIN observed_chat_members o ON o.user_id = u.telegram_user_id").
		Where("o.chat_id = ? AND u.username_normalized = ? AND u.is_bot = FALSE", chatID, normalized).
		Order("o.last_seen_at DESC, u.telegram_user_id ASC").
		Limit(2).
		Find(&rows).Error
	if err != nil {
		return domain.User{}, translateError(err)
	}
	switch len(rows) {
	case 0:
		return domain.User{}, ErrNotFound
	case 1:
		return rows[0].toDomain(), nil
	default:
		return domain.User{}, ErrAmbiguousRecipient
	}
}

func upsertUser(db *gorm.DB, user domain.User, seenAt time.Time) error {
	row := userRow{
		TelegramUserID:        user.TelegramUserID,
		Username:              user.Username,
		FirstName:             user.FirstName,
		LastName:              user.LastName,
		IsBot:                 user.IsBot,
		LanguageCode:          user.LanguageCode,
		HasStartedPrivateChat: user.HasStartedPrivateChat,
		FirstSeenAt:           seenAt,
		LastSeenAt:            seenAt,
		UpdatedAt:             seenAt,
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "telegram_user_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			// Keep the stored profile when the incoming observation is a stub
			// (for example a numeric-only guest target): overwriting with empty
			// values would silently destroy observed usernames and names.
			"username":                 clause.Expr{SQL: "COALESCE(NULLIF(EXCLUDED.username, ''), users.username)"},
			"first_name":               clause.Expr{SQL: "COALESCE(NULLIF(EXCLUDED.first_name, ''), users.first_name)"},
			"last_name":                clause.Expr{SQL: "COALESCE(NULLIF(EXCLUDED.last_name, ''), users.last_name)"},
			"is_bot":                   clause.Expr{SQL: "users.is_bot OR EXCLUDED.is_bot"},
			"language_code":            clause.Expr{SQL: "COALESCE(NULLIF(EXCLUDED.language_code, ''), users.language_code)"},
			"has_started_private_chat": clause.Expr{SQL: "users.has_started_private_chat OR EXCLUDED.has_started_private_chat"},
			"last_seen_at":             clause.Expr{SQL: "GREATEST(users.last_seen_at, EXCLUDED.last_seen_at)"},
			"updated_at":               clause.Expr{SQL: "GREATEST(users.updated_at, EXCLUDED.updated_at)"},
		}),
	}).Create(&row).Error
}

func upsertChat(db *gorm.DB, chat domain.Chat, seenAt time.Time) error {
	row := chatRow{
		TelegramChatID: chat.TelegramChatID,
		ChatType:       string(chat.Type),
		Title:          chat.Title,
		Username:       chat.Username,
		FirstSeenAt:    seenAt,
		LastSeenAt:     seenAt,
		UpdatedAt:      seenAt,
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "telegram_chat_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"chat_type":    clause.Expr{SQL: "EXCLUDED.chat_type"},
			"title":        clause.Expr{SQL: "EXCLUDED.title"},
			"username":     clause.Expr{SQL: "EXCLUDED.username"},
			"last_seen_at": clause.Expr{SQL: "GREATEST(chats.last_seen_at, EXCLUDED.last_seen_at)"},
			"updated_at":   clause.Expr{SQL: "GREATEST(chats.updated_at, EXCLUDED.updated_at)"},
		}),
	}).Create(&row).Error
}

func upsertChatMember(db *gorm.DB, chatID, userID int64, seenAt time.Time) error {
	row := observedChatMemberRow{
		ChatID:      chatID,
		UserID:      userID,
		FirstSeenAt: seenAt,
		LastSeenAt:  seenAt,
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "chat_id"}, {Name: "user_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"last_seen_at": clause.Expr{SQL: "GREATEST(observed_chat_members.last_seen_at, EXCLUDED.last_seen_at)"},
		}),
	}).Create(&row).Error
}
