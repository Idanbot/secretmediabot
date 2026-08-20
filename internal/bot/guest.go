package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/idan/secretmediabot/internal/command"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/service"
	"github.com/idan/secretmediabot/internal/telegram"
)

// inlineResultCacheSeconds lets Telegram cache inline results per user. The
// result ID is derived from the query, so identical queries reuse the same
// envelope instead of hammering the database on every keystroke.
const inlineResultCacheSeconds = 300

const (
	inlineResultVariantInstant = "instant:"
	inlineResultVariantMedia   = "media:"
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
		if text, expected := userMessage(err); expected {
			return h.answerGuestNotice(ctx, message.GuestQueryID, text)
		}
		return h.answerGuestNotice(ctx, message.GuestQueryID, "I could not create that locked secret. Try again shortly.")
	}
	result := h.guestArticle(session, target, inlineResultID(h.botUsername, sender.TelegramUserID, target, inlineResultVariantMedia))
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
	if query.From.ID <= 0 || query.From.IsBot {
		return h.answerInlineNotice(ctx, query.ID, "⚠️ Unauthorized", "Only human users can create locked secrets.")
	}

	raw := strings.TrimSpace(query.Query)
	if raw == "" {
		return h.answerInlineHelp(ctx, query.From.ID, query.ID)
	}

	target, secretText, err := parseInlineQuery(raw)
	if err != nil {
		// Target not specified as first word. Check if user typed @bot [secret] to send to a recent target!
		recents, _ := h.guest.GetRecentTargets(ctx, query.From.ID, 3)
		if len(recents) > 0 {
			var articles []telegram.InlineQueryResultArticle
			var previews []service.GuestSession
			for _, recent := range recents {
				recentTarget := targetFromRecent(recent)
				session, err := h.guest.CreateGuestInlineSecret(ctx, service.CreateGuestInlineParams{
					Sender: domainUser(query.From), Target: recentTarget, Text: raw,
					InlineQueryID: recentInlineQueryID(query.ID, recentTarget),
				})
				if err == nil {
					previews = append(previews, session)
					articles = append(articles, h.guestInlineArticleWithLabel(session, recentTarget, recent.Label(), raw,
						inlineResultID(h.botUsername, query.From.ID, recentTarget, inlineResultVariantInstant+raw)))
				}
			}
			if len(articles) > 0 {
				return h.answerInlineResults(ctx, query.ID, articles, previews, query.From.ID)
			}
		}
		return h.answerInlineHelp(ctx, query.From.ID, query.ID)
	}

	var articles []telegram.InlineQueryResultArticle
	var instantPreview service.GuestSession

	if secretText != "" {
		// Flow 1: Instant Text Secret
		session, err := h.guest.CreateGuestInlineSecret(ctx, service.CreateGuestInlineParams{
			Sender: domainUser(query.From), Target: target, Text: secretText, InlineQueryID: query.ID,
		})
		if err != nil {
			title, desc := inlineNoticeFromError(err)
			return h.answerInlineNotice(ctx, query.ID, title, desc)
		}
		instantPreview = session
		articles = []telegram.InlineQueryResultArticle{
			h.guestInlineArticle(session, target, secretText,
				inlineResultID(h.botUsername, query.From.ID, target, inlineResultVariantInstant+secretText)),
		}

		// Flow 2: Media option as secondary
		mediaSession, err := h.guest.CreateGuestRequest(ctx, service.CreateGuestRequestParams{
			Sender: domainUser(query.From), Target: target, InlineQueryID: query.ID + "-media",
		})
		if err == nil {
			articles = append(articles, h.guestArticle(mediaSession, target,
				inlineResultID(h.botUsername, query.From.ID, target, inlineResultVariantMedia)))
		}
	} else {
		// Flow 2: Media / DM Secret (target specified without secret text)
		session, err := h.guest.CreateGuestRequest(ctx, service.CreateGuestRequestParams{
			Sender: domainUser(query.From), Target: target, InlineQueryID: query.ID,
		})
		if err != nil {
			title, desc := inlineNoticeFromError(err)
			return h.answerInlineNotice(ctx, query.ID, title, desc)
		}
		articles = []telegram.InlineQueryResultArticle{
			h.guestArticle(session, target, inlineResultID(h.botUsername, query.From.ID, target, inlineResultVariantMedia)),
			h.inlineTypeSecretHint(target),
		}
	}

	var previews []service.GuestSession
	if secretText != "" {
		// Only ready text previews are safe to cancel after a permanent answer
		// failure. The secondary media request may be reused by another query.
		previews = append(previews, instantPreview)
	}
	return h.answerInlineResults(ctx, query.ID, articles, previews, query.From.ID)
}

func (h *Handler) answerInlineResults(
	ctx context.Context,
	queryID string,
	results []telegram.InlineQueryResultArticle,
	previews []service.GuestSession,
	senderID int64,
) error {
	requestCtx, cancel := context.WithTimeout(ctx, h.requestTimeout)
	err := h.telegram.AnswerInlineQuery(requestCtx, telegram.AnswerInlineQueryRequest{
		InlineQueryID: queryID, Results: results,
		CacheTime: inlineResultCacheSeconds, IsPersonal: true,
	})
	cancel()
	if err == nil || !telegram.IsPermanent(err) || len(previews) == 0 {
		return err
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cleanupCancel()
	for _, preview := range previews {
		if cancelErr := h.guest.CancelGuestRequestByID(cleanupCtx, preview.Request.ID, senderID); cancelErr != nil &&
			!errors.Is(cancelErr, service.ErrGuestNotFound) {
			h.logger.WarnContext(ctx, "inline preview cancellation failed")
		}
	}
	return err
}

func (h *Handler) inlineTypeSecretHint(target command.Target) telegram.InlineQueryResultArticle {
	targetText := target.Username
	if target.Kind == command.TargetUserID {
		targetText = fmt.Sprintf("%d", target.UserID)
	} else if !strings.HasPrefix(targetText, "@") {
		targetText = "@" + targetText
	}
	bot := "@" + h.botUsername
	return telegram.InlineQueryResultArticle{
		Type: "article", ID: "hint-" + targetText,
		Title:       fmt.Sprintf("💬 Or type secret message after %s", targetText),
		Description: fmt.Sprintf("%s %s <your secret message>", bot, targetText),
		InputMessageContent: telegram.InputTextMessageContent{
			MessageText: fmt.Sprintf("To send an instant secret whisper, type:\n%s %s your secret message", bot, targetText),
		},
	}
}

func targetFromRecent(r domain.RecentTarget) command.Target {
	if r.TargetUserID > 0 {
		return command.Target{
			Kind:   command.TargetUserID,
			UserID: r.TargetUserID,
		}
	}
	if r.TargetUsername != "" {
		return command.Target{
			Kind:     command.TargetUsername,
			Username: strings.TrimPrefix(r.TargetUsername, "@"),
		}
	}
	return command.Target{
		Kind:   command.TargetUserID,
		UserID: r.TargetUserID,
	}
}

func parseInlineQuery(query string) (command.Target, string, error) {
	query = strings.TrimSpace(query)
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return command.Target{}, "", errors.New("empty query")
	}
	target, err := command.ParseTarget(fields[0])
	if err != nil {
		return command.Target{}, "", err
	}
	var secretText string
	if len(fields) > 1 {
		secretText = cleanSecretText(query[len(fields[0]):])
	}
	return target, secretText, nil
}

func cleanSecretText(s string) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) >= 2 {
		first := runes[0]
		last := runes[len(runes)-1]
		if (first == '"' && last == '"') ||
			(first == '\'' && last == '\'') ||
			(first == '`' && last == '`') ||
			(first == '“' && last == '”') ||
			(first == '‘' && last == '’') {
			s = strings.TrimSpace(string(runes[1 : len(runes)-1]))
		}
	}
	return s
}

// inlineResultID derives a stable inline result ID from the target and result
// variant. Variant prefixes keep a secret whose text happens to be "media"
// distinct from the secondary media result.
func inlineResultID(botUsername string, senderID int64, target command.Target, variant string) string {
	targetText := target.Username
	if target.Kind == command.TargetUserID {
		targetText = fmt.Sprintf("id:%d", target.UserID)
	}
	digest := sha256.Sum256([]byte(botUsername + "|" + fmt.Sprintf("%d", senderID) + "|" + targetText + "|" + variant))
	return hex.EncodeToString(digest[:16])
}

func recentInlineQueryID(queryID string, target command.Target) string {
	return queryID + "-recent-" + inlineTargetKey(target)
}

func inlineTargetKey(target command.Target) string {
	if target.Kind == command.TargetUserID {
		return fmt.Sprintf("user-id-%d", target.UserID)
	}
	return "username-" + strings.ToLower(strings.TrimPrefix(target.Username, "@"))
}

func (h *Handler) guestInlineArticle(session service.GuestSession, target command.Target, text string, resultID string) telegram.InlineQueryResultArticle {
	targetText := target.Username
	if target.Kind == command.TargetUserID {
		targetText = fmt.Sprintf("Telegram user %d", target.UserID)
	} else if !strings.HasPrefix(targetText, "@") {
		targetText = "@" + targetText
	}
	preview := text
	if runes := []rune(preview); len(runes) > 40 {
		preview = string(runes[:40]) + "..."
	}
	link := composeURL(h.botUsername, session.Parameter)
	return telegram.InlineQueryResultArticle{
		Type: "article", ID: resultID, Title: fmt.Sprintf("🔒 Send instant secret to %s", targetText),
		Description: fmt.Sprintf("Secret: %q (tap to send locked whisper)", preview),
		InputMessageContent: telegram.InputTextMessageContent{
			MessageText: fmt.Sprintf("🔒 Secret whisper for %s.\nOnly they can unlock and view this secret.", targetText),
		},
		ReplyMarkup: &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{{
			{Text: "🔓 Open Secret", URL: link},
		}}},
	}
}

func (h *Handler) guestInlineArticleWithLabel(session service.GuestSession, target command.Target, label, text, resultID string) telegram.InlineQueryResultArticle {
	preview := text
	if runes := []rune(preview); len(runes) > 40 {
		preview = string(runes[:40]) + "..."
	}
	link := composeURL(h.botUsername, session.Parameter)
	return telegram.InlineQueryResultArticle{
		Type: "article", ID: resultID, Title: fmt.Sprintf("🔒 Send to %s", label),
		Description: fmt.Sprintf("Secret: %q (tap to send locked whisper)", preview),
		InputMessageContent: telegram.InputTextMessageContent{
			MessageText: fmt.Sprintf("🔒 Secret whisper for %s.\nOnly they can unlock and view this secret.", label),
		},
		ReplyMarkup: &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{{
			{Text: "🔓 Open Secret", URL: link},
		}}},
	}
}

func (h *Handler) guestArticle(session service.GuestSession, target command.Target, resultID string) telegram.InlineQueryResultArticle {
	targetText := target.Username
	if target.Kind == command.TargetUserID {
		targetText = fmt.Sprintf("Telegram user %d", target.UserID)
	} else if !strings.HasPrefix(targetText, "@") {
		targetText = "@" + targetText
	}
	description := "Tap to post envelope, then sender taps button to add secret in DM."
	link := composeURL(h.botUsername, session.Parameter)
	return telegram.InlineQueryResultArticle{
		Type: "article", ID: resultID, Title: fmt.Sprintf("🖼️ / 📁 Send Media Whisper to %s (Photo, Video, File)", targetText),
		Description: description,
		InputMessageContent: telegram.InputTextMessageContent{
			MessageText: fmt.Sprintf("🔒 Locked secret envelope for %s.\nSender: click the button below to add your secret privately in DM.\nRecipient: click to open once added.", targetText),
		},
		ReplyMarkup: &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{{
			{Text: "➕ Add or open privately", URL: link},
		}}},
	}
}

func (h *Handler) answerInlineHelp(ctx context.Context, senderID int64, queryID string) error {
	requestCtx, cancel := context.WithTimeout(ctx, h.requestTimeout)
	defer cancel()
	bot := "@" + h.botUsername
	var articles []telegram.InlineQueryResultArticle

	if senderID > 0 && h.guest != nil {
		recents, _ := h.guest.GetRecentTargets(ctx, senderID, 3)
		for _, r := range recents {
			targetID := r.TargetIdentifier()
			articles = append(articles, telegram.InlineQueryResultArticle{
				Type:        "article",
				ID:          "recent-" + targetID,
				Title:       fmt.Sprintf("👤 Send to %s", r.Label()),
				Description: fmt.Sprintf("Tap to whisper to %s", r.Label()),
				InputMessageContent: telegram.InputTextMessageContent{
					MessageText: fmt.Sprintf("To send a secret whisper to %s:\n%s %s your secret message", r.Label(), bot, targetID),
				},
			})
		}
	}

	articles = append(articles,
		telegram.InlineQueryResultArticle{
			Type:        "article",
			ID:          "inline-help-instant",
			Title:       "1️⃣ Instant Text Whisper (1 Step)",
			Description: bot + " @recipient <your secret message>",
			InputMessageContent: telegram.InputTextMessageContent{
				MessageText: "To send an instant secret whisper:\n" + bot + " @recipient your secret message",
			},
		},
		telegram.InlineQueryResultArticle{
			Type:        "article",
			ID:          "inline-help-media",
			Title:       "2️⃣ Media/DM Whisper (2 Steps)",
			Description: bot + " @recipient (tap to post envelope, then add photo/media in DM)",
			InputMessageContent: telegram.InputTextMessageContent{
				MessageText: "To send a media whisper:\n" + bot + " @recipient",
			},
		},
	)

	return h.telegram.AnswerInlineQuery(requestCtx, telegram.AnswerInlineQueryRequest{
		InlineQueryID: queryID, CacheTime: 0, IsPersonal: true,
		Results: articles,
	})
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

func (h *Handler) answerInlineNotice(ctx context.Context, queryID, title, text string) error {
	requestCtx, cancel := context.WithTimeout(ctx, h.requestTimeout)
	defer cancel()
	return h.telegram.AnswerInlineQuery(requestCtx, telegram.AnswerInlineQueryRequest{
		InlineQueryID: queryID, CacheTime: 0, IsPersonal: true,
		Results: []telegram.InlineQueryResultArticle{{
			Type: "article", ID: "inline-notice", Title: title,
			Description:         text,
			InputMessageContent: telegram.InputTextMessageContent{MessageText: text},
		}},
	})
}

func inlineNoticeFromError(err error) (string, string) {
	switch {
	case errors.Is(err, service.ErrTargetIsSender):
		return "⚠️ Cannot send secret to yourself", "Target another user's @username or numeric Telegram ID."
	case errors.Is(err, service.ErrGuestActiveLimit):
		return "⚠️ Active secret limit reached", "You have open whispers waiting. Use /cancel in private chat before creating more."
	case errors.Is(err, service.ErrGuestRateLimit):
		return "⚠️ Hourly limit reached", "You have reached the hourly whisper limit. Try again in a few minutes."
	case errors.Is(err, service.ErrTextTooLong):
		return "⚠️ Secret text is too long", "Secret text is limited to 4096 characters."
	case errors.Is(err, service.ErrTargetRequired), errors.Is(err, command.ErrInvalidTarget):
		return "🔒 Send Secret Whisper", "Type: @username [secret message] or 123456789 [secret message]"
	default:
		if text, expected := userMessage(err); expected {
			return "⚠️ " + text, text
		}
		return "🔒 Send Secret Whisper", "Type: @username [secret message] or 123456789 [secret message]"
	}
}

func parseGuestTarget(text, botUsername string) (command.Target, error) {
	fields := strings.Fields(text)
	if len(fields) != 2 || !strings.EqualFold(strings.TrimPrefix(fields[0], "@"), strings.TrimPrefix(botUsername, "@")) {
		return command.Target{}, errors.New("guest mention must contain exactly one target")
	}
	return command.ParseTarget(fields[1])
}
