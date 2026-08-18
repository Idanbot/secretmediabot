package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/idan/secretmediabot/internal/command"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/service"
	"github.com/idan/secretmediabot/internal/telegram"
)

// inlineResultCacheSeconds lets Telegram cache inline results per user. The
// result ID is derived from the query, so identical queries reuse the same
// envelope instead of hammering the database on every keystroke.
const inlineResultCacheSeconds = 300

func (h *Handler) handleGuestMessage(ctx context.Context, message telegram.Message) error {
	if h.guest == nil || message.GuestQueryID == "" {
		return nil
	}
	caller := message.From
	if message.GuestBotCallerUser != nil {
		caller = message.GuestBotCallerUser
	}
	if caller == nil || caller.ID <= 0 || caller.IsBot {
		return h.answerGuestNotice(ctx, message.GuestQueryID, "Only human users can create locked secrets.")
	}
	chat := message.Chat
	if message.GuestBotCallerChat != nil {
		chat = *message.GuestBotCallerChat
	}
	if chat.Type != string(domain.ChatTypeGroup) && chat.Type != string(domain.ChatTypeSupergroup) {
		return h.answerGuestNotice(ctx, message.GuestQueryID, "Locked group secrets require a group or supergroup.")
	}
	target, err := parseGuestTarget(message.Text, h.botUsername)
	if err != nil {
		return h.answerGuestNotice(ctx, message.GuestQueryID, "Use @"+h.botUsername+" @username or @"+h.botUsername+" 123456789. Send the secret privately after that.")
	}
	sender := domainUser(*caller)
	sourceChat := domainChat(chat)
	session, err := h.guest.CreateGuestRequest(ctx, service.CreateGuestRequestParams{
		Sender: sender, Target: target, SourceChat: &sourceChat,
		SourceThreadID: optionalMessageID(message.MessageThreadID), SourceMessageID: optionalMessageID(message.MessageID),
		GuestQueryID: message.GuestQueryID,
	})
	if err != nil {
		if text, expected := userMessage(err); expected {
			return h.answerGuestNotice(ctx, message.GuestQueryID, text)
		}
		return h.answerGuestNotice(ctx, message.GuestQueryID, "I could not create that locked secret. Try again shortly.")
	}
	result := h.guestArticle(session, target, inlineResultID(h.botUsername, sender.TelegramUserID, target))
	requestCtx, cancel := context.WithTimeout(ctx, h.requestTimeout)
	sent, err := h.telegram.AnswerGuestQuery(requestCtx, telegram.AnswerGuestQueryRequest{
		GuestQueryID: message.GuestQueryID, Result: result,
	})
	cancel()
	if err != nil {
		return err
	}
	if err := h.guest.MarkGuestEnvelope(ctx, session.Parameter, sent.InlineMessageID); err != nil {
		h.logger.WarnContext(ctx, "guest envelope persistence failed")
	}
	return nil
}

func (h *Handler) handleInlineQuery(ctx context.Context, query telegram.InlineQuery) error {
	if h.guest == nil || query.ID == "" {
		return nil
	}
	target, err := command.ParseTarget(strings.TrimSpace(query.Query))
	if err != nil {
		return h.answerInlineNotice(ctx, query.ID, "Use @username or a numeric Telegram ID, then select the locked envelope.")
	}
	if query.From.ID <= 0 || query.From.IsBot {
		return h.answerInlineNotice(ctx, query.ID, "Only human users can create locked secrets.")
	}
	session, err := h.guest.CreateGuestRequest(ctx, service.CreateGuestRequestParams{
		Sender: domainUser(query.From), Target: target, InlineQueryID: query.ID,
	})
	if err != nil {
		if text, expected := userMessage(err); expected {
			return h.answerInlineNotice(ctx, query.ID, text)
		}
		return h.answerInlineNotice(ctx, query.ID, "I could not create that locked secret. Try again shortly.")
	}
	requestCtx, cancel := context.WithTimeout(ctx, h.requestTimeout)
	err = h.telegram.AnswerInlineQuery(requestCtx, telegram.AnswerInlineQueryRequest{
		InlineQueryID: query.ID, Results: []telegram.InlineQueryResultArticle{
			h.guestArticle(session, target, inlineResultID(h.botUsername, query.From.ID, target)),
		},
		CacheTime: inlineResultCacheSeconds, IsPersonal: true,
	})
	cancel()
	return err
}

// inlineResultID derives a stable inline result ID from the query text so
// Telegram's inline cache can reuse the same envelope across repeated queries.
func inlineResultID(botUsername string, senderID int64, target command.Target) string {
	targetText := target.Username
	if target.Kind == command.TargetUserID {
		targetText = fmt.Sprintf("id:%d", target.UserID)
	}
	digest := sha256.Sum256([]byte(botUsername + "|" + fmt.Sprintf("%d", senderID) + "|" + targetText))
	return hex.EncodeToString(digest[:16])
}

func (h *Handler) guestArticle(session service.GuestSession, target command.Target, resultID string) telegram.InlineQueryResultArticle {
	targetText := target.Username
	if target.Kind == command.TargetUserID {
		targetText = fmt.Sprintf("Telegram user %d", target.UserID)
	}
	description := "The secret is added privately and opened privately."
	if target.Kind == command.TargetUsername {
		// Telegram usernames are mutable: the first person holding this
		// username who opens the envelope privately receives the secret.
		description = "The secret is added privately and opened privately. Usernames can change; prefer a numeric ID for certainty."
	}
	link := composeURL(h.botUsername, session.Parameter)
	return telegram.InlineQueryResultArticle{
		Type: "article", ID: resultID, Title: "Locked secret",
		Description: description,
		InputMessageContent: telegram.InputTextMessageContent{
			MessageText: fmt.Sprintf("Locked secret for %s. The secret content is not posted in this group.", targetText),
		},
		ReplyMarkup: &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{{
			{Text: "Add or open privately", URL: link},
		}}},
	}
}

func (h *Handler) answerGuestNotice(ctx context.Context, queryID, text string) error {
	requestCtx, cancel := context.WithTimeout(ctx, h.requestTimeout)
	defer cancel()
	_, err := h.telegram.AnswerGuestQuery(requestCtx, telegram.AnswerGuestQueryRequest{
		GuestQueryID: queryID,
		Result: telegram.InlineQueryResultArticle{
			Type: "article", ID: "guest-error", Title: "Locked secret unavailable",
			InputMessageContent: telegram.InputTextMessageContent{MessageText: text},
		},
	})
	return err
}

func (h *Handler) answerInlineNotice(ctx context.Context, queryID, text string) error {
	requestCtx, cancel := context.WithTimeout(ctx, h.requestTimeout)
	defer cancel()
	return h.telegram.AnswerInlineQuery(requestCtx, telegram.AnswerInlineQueryRequest{
		InlineQueryID: queryID, CacheTime: 0, IsPersonal: true,
		Results: []telegram.InlineQueryResultArticle{{
			Type: "article", ID: "inline-error", Title: "Locked secret unavailable",
			InputMessageContent: telegram.InputTextMessageContent{MessageText: text},
		}},
	})
}

func parseGuestTarget(text, botUsername string) (command.Target, error) {
	fields := strings.Fields(text)
	if len(fields) != 2 || !strings.EqualFold(strings.TrimPrefix(fields[0], "@"), strings.TrimPrefix(botUsername, "@")) {
		return command.Target{}, errors.New("guest mention must contain exactly one target")
	}
	return command.ParseTarget(fields[1])
}
