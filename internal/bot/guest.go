package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/idan/secretmediabot/internal/command"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/service"
	"github.com/idan/secretmediabot/internal/telegram"
)

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
		return h.answerGuestNotice(ctx, message.GuestQueryID, "I could not create that locked secret. Try again shortly.")
	}
	result := h.guestArticle(session, target)
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
		return h.answerInlineNotice(ctx, query.ID, "I could not create that locked secret. Try again shortly.")
	}
	requestCtx, cancel := context.WithTimeout(ctx, h.requestTimeout)
	err = h.telegram.AnswerInlineQuery(requestCtx, telegram.AnswerInlineQueryRequest{
		InlineQueryID: query.ID, Results: []telegram.InlineQueryResultArticle{h.guestArticle(session, target)},
		CacheTime: 0, IsPersonal: true,
	})
	cancel()
	return err
}

func (h *Handler) guestArticle(session service.GuestSession, target command.Target) telegram.InlineQueryResultArticle {
	targetText := target.Username
	if target.Kind == command.TargetUserID {
		targetText = fmt.Sprintf("Telegram user %d", target.UserID)
	}
	link := composeURL(h.botUsername, session.Parameter)
	return telegram.InlineQueryResultArticle{
		Type: "article", ID: session.Request.ID.String(), Title: "Locked secret",
		Description: "The secret is added privately and opened privately.",
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
