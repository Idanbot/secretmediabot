package bot

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/idan/secretmediabot/internal/command"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/secretcrypto"
	"github.com/idan/secretmediabot/internal/service"
	"github.com/idan/secretmediabot/internal/telegram"
)

func (h *Handler) handleGroupMessage(
	ctx context.Context,
	message telegram.Message,
	sender domain.User,
	chat domain.Chat,
) error {
	parsed, isCommand, err := command.Parse(message.Text, h.botUsername)
	if errors.Is(err, command.ErrOtherBot) || !isCommand {
		return nil
	}
	if err != nil {
		return err
	}
	switch parsed.Name {
	case "whisper":
		return h.beginWhisper(ctx, message, sender, chat, parsed.Args)
	case "help":
		return h.sendReply(ctx, message, h.groupHelpText(), nil)
	case "privacy":
		return h.sendReply(ctx, message, privacyText, nil)
	default:
		return nil
	}
}

func (h *Handler) beginWhisper(
	ctx context.Context,
	message telegram.Message,
	sender domain.User,
	chat domain.Chat,
	args string,
) error {
	request := service.CreateDraftRequest{
		Sender: sender, Chat: chat,
		SourceThreadID:         optionalMessageID(message.MessageThreadID),
		SourceCommandMessageID: optionalMessageID(message.MessageID),
	}
	if message.ReplyToMessage != nil {
		if strings.TrimSpace(args) != "" || message.ReplyToMessage.From == nil {
			return h.sendReply(ctx, message, "Reply to one person's message without also supplying a target.", nil)
		}
		recipient := domainUser(*message.ReplyToMessage.From)
		request.ReplyRecipient = &recipient
		request.SourceReplyMessageID = optionalMessageID(message.ReplyToMessage.MessageID)
	} else {
		target, err := command.ParseTarget(args)
		if err != nil {
			return h.sendReply(ctx, message, "Use /whisper as a reply, or use /whisper @username or /whisper 123456789.", nil)
		}
		request.DirectTarget = &target
	}

	created, err := h.service.CreateDraft(ctx, request)
	if err != nil {
		if text, expected := userMessage(err); expected {
			return h.sendReply(ctx, message, text, nil)
		}
		return err
	}

	prompt := fmt.Sprintf(
		"Send one secret for %s now: either text, or one photo, voice note, video, audio file, or document (maximum %s). A media item may have a caption. Use /cancel to discard it.",
		created.Recipient.DisplayName(), h.mediaLimitText(),
	)
	promptCtx, cancelPrompt := context.WithTimeout(ctx, h.requestTimeout)
	_, promptErr := h.telegram.SendMessage(promptCtx, telegram.SendMessageRequest{
		ChatID: sender.TelegramUserID, Text: prompt, ProtectContent: true,
	})
	cancelPrompt()
	if promptErr == nil {
		if err := h.sendReply(ctx, message, "I opened the private composer. Check your chat with me.", nil); err != nil {
			h.logger.WarnContext(ctx, "group composer acknowledgement failed")
		}
		return nil
	}

	deepLink := composeURL(h.botUsername, created.ComposeParameter)
	markup := &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{{{
		Text: "Continue privately", URL: deepLink,
	}}}}
	if err := h.sendReply(ctx, message, "Open the private composer to send the secret.", markup); err != nil {
		cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_, _ = h.service.CancelLatestDraft(cancelCtx, sender.TelegramUserID)
		return err
	}
	return nil
}

func (h *Handler) handlePrivateMessage(ctx context.Context, message telegram.Message, sender domain.User) error {
	parsed, isCommand, err := command.Parse(message.Text, h.botUsername)
	if errors.Is(err, command.ErrOtherBot) {
		return nil
	}
	if err != nil {
		return err
	}
	if isCommand {
		switch parsed.Name {
		case "start":
			return h.handleStart(ctx, message, sender, parsed.Args)
		case "help":
			return h.sendReply(ctx, message, h.privateHelpText(), nil)
		case "privacy":
			return h.sendReply(ctx, message, privacyText, nil)
		case "cancel":
			return h.handleCancel(ctx, message, sender)
		case "owner_list", "owner_sender", "owner_media", "owner_last", "owner_open", "owner_review", "owner_delete", "owner_retain", "owner_set_retention", "owner_menu", "owner_ephemeral":
			return h.handleOwnerCommand(ctx, message, sender, parsed)
		default:
			return h.sendReply(ctx, message, "Unknown command. Use /help.", nil)
		}
	}
	if h.guest != nil {
		handled, guestErr := h.ingestGuestSecret(ctx, message, sender)
		if handled {
			return guestErr
		}
	}
	return h.ingestSecret(ctx, message, sender)
}

func (h *Handler) handleCancel(ctx context.Context, message telegram.Message, sender domain.User) error {
	cancelledGuest := 0
	if h.guest != nil {
		count, guestCancelErr := h.guest.CancelGuestRequest(ctx, sender.TelegramUserID)
		if guestCancelErr != nil && !errors.Is(guestCancelErr, service.ErrGuestNotFound) && !errors.Is(guestCancelErr, service.ErrGuestUnavailable) {
			if text, expected := userMessage(guestCancelErr); expected {
				return h.sendReply(ctx, message, text, nil)
			}
			return guestCancelErr
		}
		if guestCancelErr == nil {
			cancelledGuest = count
		}
	}
	cancelledDraft := false
	_, cancelErr := h.service.CancelLatestDraft(ctx, sender.TelegramUserID)
	switch {
	case cancelErr == nil:
		cancelledDraft = true
	case errors.Is(cancelErr, service.ErrDraftNotFound):
		// nothing to report; the guest result (if any) decides the message
	default:
		if text, expected := userMessage(cancelErr); expected {
			return h.sendReply(ctx, message, text, nil)
		}
		return cancelErr
	}
	switch {
	case cancelledDraft:
		if err := h.sendReply(ctx, message, draftCancelText(cancelledGuest), nil); err != nil {
			// The cancellation already committed; a failed acknowledgement
			// must not be retried as if the mutation itself failed.
			h.logger.WarnContext(ctx, "draft cancellation acknowledgement failed")
		}
		return nil
	case cancelledGuest > 0:
		return h.sendReply(ctx, message, "Locked secret cancelled.", nil)
	default:
		return h.sendReply(ctx, message, "No active draft was found. Start with /whisper in a shared group.", nil)
	}
}

func draftCancelText(cancelledGuest int) string {
	if cancelledGuest > 0 {
		return "Draft and locked secret cancelled."
	}
	return "Draft cancelled."
}

func (h *Handler) handleStart(
	ctx context.Context,
	message telegram.Message,
	sender domain.User,
	parameter string,
) error {
	if strings.TrimSpace(parameter) == "" {
		return h.sendReply(ctx, message, h.welcomeText(), nil)
	}
	if strings.HasPrefix(strings.TrimSpace(parameter), service.GuestPrefix) {
		return h.handleGuestStart(ctx, message, sender, parameter)
	}
	resumed, err := h.service.ResumeDraft(ctx, sender, parameter)
	if err != nil {
		if text, expected := userMessage(err); expected {
			return h.sendReply(ctx, message, text, nil)
		}
		return err
	}
	return h.sendReply(ctx, message, fmt.Sprintf(
		"Composer ready for %s. Send text or one supported media item up to %s. Use /cancel to stop.",
		resumed.Recipient.DisplayName(), h.mediaLimitText(),
	), nil)
}

func (h *Handler) handleGuestStart(ctx context.Context, message telegram.Message, sender domain.User, parameter string) error {
	session, err := h.guest.BeginGuestSession(ctx, parameter, sender)
	if err != nil {
		if text, expected := userMessage(err); expected {
			return h.sendReply(ctx, message, text, nil)
		}
		return err
	}
	if session.Role == service.GuestRoleSender {
		switch session.Request.State {
		case repository.GuestStateAwaitingSecret, repository.GuestStateIngestingSecret:
			targetName := "the recipient"
			if session.Request.TargetUsername != "" {
				targetName = "@" + session.Request.TargetUsername
			} else if session.Request.TargetUserID != nil {
				targetName = fmt.Sprintf("user %d", *session.Request.TargetUserID)
			}
			return h.sendReply(ctx, message, fmt.Sprintf(
				"Composer ready for %s. Send secret text or one photo, voice note, video, audio file, or document here (up to %s). Use /cancel to stop.",
				targetName, h.mediaLimitText(),
			), nil)
		case repository.GuestStateReady:
			return h.sendReply(ctx, message, "You are the sender of this secret. The secret is ready and waiting for the recipient to open it from the envelope.", nil)
		case repository.GuestStateOpening:
			return h.sendReply(ctx, message, "A delivery attempt is in progress. If it fails, the envelope becomes openable again shortly.", nil)
		case repository.GuestStateOpened:
			return h.sendReply(ctx, message, "The secret was already opened.", nil)
		default:
			return h.sendReply(ctx, message, "This locked secret is no longer available.", nil)
		}
	}
	switch session.Request.State {
	case repository.GuestStateReady:
		return h.deliverGuestSecret(ctx, message, sender, session.Parameter)
	case repository.GuestStateOpening:
		return h.sendReply(ctx, message, "A delivery attempt is already in progress. Try again in a few seconds.", nil)
	case repository.GuestStateOpened:
		return h.sendReply(ctx, message, "This locked secret was already opened.", nil)
	case repository.GuestStateAwaitingSecret, repository.GuestStateIngestingSecret:
		return h.sendReply(ctx, message, "The secret is not ready yet. The sender has not added the secret yet. Press Open privately again after it is ready.", nil)
	default:
		return h.sendReply(ctx, message, "This locked secret is no longer available.", nil)
	}
}

func (h *Handler) ingestGuestSecret(ctx context.Context, message telegram.Message, sender domain.User) (bool, error) {
	// An explicit /whisper draft always wins the private composer: otherwise a
	// pending guest request would silently starve the draft until it expired.
	if hasDraft, draftErr := h.service.HasActiveDraft(ctx, sender.TelegramUserID); draftErr == nil && hasDraft {
		return false, nil
	}
	claim, err := h.guest.ClaimGuestIngestForSender(ctx, sender.TelegramUserID)
	if errors.Is(err, service.ErrGuestNotFound) || errors.Is(err, service.ErrGuestUnavailable) {
		return false, nil
	}
	if err != nil {
		if text, expected := userMessage(err); expected {
			return true, h.sendReply(ctx, message, text, nil)
		}
		return true, err
	}
	if message.MediaGroupID != "" {
		return true, h.sendReply(ctx, message, "Albums are not supported. Send exactly one media item.", nil)
	}
	media, mediaErr := telegram.ExtractMedia(message)
	hasMedia := mediaErr == nil
	if mediaErr != nil && !errors.Is(mediaErr, telegram.ErrNoSupportedMedia) {
		return true, h.sendReply(ctx, message, "Send exactly one supported media item, not multiple attachments.", nil)
	}
	if !hasMedia && strings.TrimSpace(message.Text) == "" {
		return true, h.sendReply(ctx, message, "Send secret text or one photo, voice note, video, audio file, or document.", nil)
	}
	if hasMedia && media.SizeBytes > h.maxMediaBytes {
		return true, h.sendReply(ctx, message, "That media item exceeds the "+h.mediaLimitText()+" limit.", nil)
	}
	release := true
	defer func() {
		if release {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			_ = h.guest.ReleaseGuestIngest(releaseCtx, claim)
		}
	}()
	var finalizeErr error
	if !hasMedia {
		_, finalizeErr = h.guest.FinalizeGuestText(ctx, claim, message.Text)
	} else {
		metadataCtx, cancelMetadata := context.WithTimeout(ctx, h.requestTimeout)
		file, fileErr := h.telegram.GetFile(metadataCtx, telegram.GetFileRequest{FileID: media.Ref})
		cancelMetadata()
		if fileErr != nil {
			if errors.Is(fileErr, telegram.ErrFileTooLarge) {
				return true, h.sendReply(ctx, message, "That media item exceeds the "+h.mediaLimitText()+" limit.", nil)
			}
			return true, fileErr
		}
		if file.FileSize > 0 {
			media.SizeBytes = file.FileSize
		}
		downloadCtx, cancelDownload := context.WithTimeout(ctx, h.mediaDownloadTimeout)
		bytes, downloadErr := h.telegram.DownloadFile(downloadCtx, file.FilePath, file.FileSize)
		cancelDownload()
		if downloadErr != nil {
			if errors.Is(downloadErr, telegram.ErrFileTooLarge) {
				return true, h.sendReply(ctx, message, "That media item exceeds the "+h.mediaLimitText()+" limit.", nil)
			}
			return true, downloadErr
		}
		finalizeErr = func() error {
			defer secretcrypto.Zero(bytes)
			_, err := h.guest.FinalizeGuestMedia(ctx, claim, media, bytes, message.Caption)
			return err
		}()
	}
	if finalizeErr != nil {
		if text, expected := userMessage(finalizeErr); expected {
			return true, h.sendReply(ctx, message, text, nil)
		}
		return true, finalizeErr
	}
	release = false
	return true, h.sendReply(ctx, message, "Secret stored privately. The target can open it from the group envelope.", nil)
}

func (h *Handler) deliverGuestSecret(ctx context.Context, message telegram.Message, sender domain.User, parameter string) error {
	delivery, err := h.guest.ReserveGuestOpen(ctx, parameter, sender)
	if err != nil {
		if text, expected := userMessage(err); expected {
			return h.sendReply(ctx, message, text, nil)
		}
		return err
	}
	defer delivery.Content.Zero()
	var sent telegram.Message
	requestCtx, cancel := context.WithTimeout(ctx, h.requestTimeout)
	if delivery.Content.Kind == domain.PayloadText {
		sent, err = h.telegram.SendMessage(requestCtx, telegram.SendMessageRequest{
			ChatID: sender.TelegramUserID, Text: string(delivery.Content.Text), ProtectContent: true,
		})
	} else if delivery.Content.Media != nil {
		sent, err = h.telegram.SendPrivateMediaByFileID(requestCtx, telegram.SendPrivateMediaByFileIDRequest{
			ChatID: sender.TelegramUserID, Type: delivery.Content.Media.Type,
			FileID: delivery.Content.Media.TelegramFileID, Caption: string(delivery.Content.Caption), ProtectContent: true,
		})
		if err != nil && telegram.IsPermanent(err) {
			data, mediaType, contentType, fallbackErr := h.guest.GuestMediaFallback(requestCtx, delivery.Request.ID)
			if fallbackErr != nil {
				err = errors.Join(err, fallbackErr)
			} else if int64(len(data)) != delivery.Content.Media.PlaintextSize {
				err = errors.New("retained guest media size mismatch")
			} else {
				sent, err = h.telegram.SendPrivateMedia(requestCtx, telegram.SendPrivateMediaRequest{
					ChatID: sender.TelegramUserID, Type: mediaType, Data: data,
					ContentType: contentType, Caption: string(delivery.Content.Caption), ProtectContent: true,
				})
			}
			secretcrypto.Zero(data)
		}
	} else {
		err = errors.New("guest delivery has no supported content")
	}
	cancel()
	if err != nil {
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		finishErr := h.guest.FailGuestOpen(finishCtx, delivery)
		finishCancel()
		if finishErr != nil {
			return errors.Join(err, finishErr)
		}
		return h.sendReply(ctx, message, "Delivery failed. Press Open privately again to retry.", nil)
	}
	if err := h.guest.CompleteGuestOpen(ctx, delivery, sent.MessageID); err != nil {
		return err
	}
	acknowledgement := "Secret delivered privately. It will remain under the configured retention policy."
	if deleteAfter := h.service.GetEphemeralDeleteAfter(); deleteAfter > 0 {
		acknowledgement = fmt.Sprintf("Secret delivered privately. It will be deleted after %s.", deleteAfter)
	}
	return h.sendReply(ctx, message, acknowledgement, nil)
}

func (h *Handler) ingestSecret(ctx context.Context, message telegram.Message, sender domain.User) error {
	if message.MediaGroupID != "" {
		return h.sendReply(ctx, message, "Albums are not supported. Send exactly one media item.", nil)
	}

	media, mediaErr := telegram.ExtractMedia(message)
	hasMedia := mediaErr == nil
	if mediaErr != nil && !errors.Is(mediaErr, telegram.ErrNoSupportedMedia) {
		return h.sendReply(ctx, message, "Send exactly one supported media item, not multiple attachments.", nil)
	}
	if !hasMedia && strings.TrimSpace(message.Text) == "" {
		return h.sendReply(ctx, message, "Send secret text or one photo, voice note, video, audio file, or document.", nil)
	}
	if hasMedia && media.SizeBytes > h.maxMediaBytes {
		return h.sendReply(ctx, message, "That media item exceeds the "+h.mediaLimitText()+" limit.", nil)
	}

	draft, err := h.service.ClaimIngest(ctx, sender.TelegramUserID)
	if err != nil {
		if text, expected := userMessage(err); expected {
			return h.sendReply(ctx, message, text, nil)
		}
		return err
	}
	release := true
	defer func() {
		if release {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			_ = h.service.ReleaseIngest(releaseCtx, draft)
		}
	}()

	var created service.CreatedWhisper
	if !hasMedia {
		created, err = h.service.FinalizeText(ctx, draft, message.Text)
	} else {
		metadataCtx, cancelMetadata := context.WithTimeout(ctx, h.requestTimeout)
		file, fileErr := h.telegram.GetFile(metadataCtx, telegram.GetFileRequest{FileID: media.Ref})
		cancelMetadata()
		if fileErr != nil {
			if errors.Is(fileErr, telegram.ErrFileTooLarge) {
				return h.sendReply(ctx, message, "That media item exceeds the "+h.mediaLimitText()+" limit.", nil)
			}
			return fileErr
		}
		if file.FileSize > 0 {
			media.SizeBytes = file.FileSize
		}
		downloadCtx, cancelDownload := context.WithTimeout(ctx, h.mediaDownloadTimeout)
		bytes, downloadErr := h.telegram.DownloadFile(downloadCtx, file.FilePath, file.FileSize)
		cancelDownload()
		if downloadErr != nil {
			if errors.Is(downloadErr, telegram.ErrFileTooLarge) {
				return h.sendReply(ctx, message, "That media item exceeds the "+h.mediaLimitText()+" limit.", nil)
			}
			return downloadErr
		}
		defer secretcrypto.Zero(bytes)
		created, err = h.service.FinalizeMedia(ctx, draft, media, bytes, message.Caption)
	}
	if err != nil {
		if text, expected := userMessage(err); expected {
			return h.sendReply(ctx, message, text, nil)
		}
		return err
	}
	release = false

	publication, err := h.service.ClaimPublication(ctx, created.Whisper.ID)
	if err == nil {
		err = h.Publish(ctx, publication)
	}
	if err != nil {
		if service.IsNoPublication(err) {
			// The envelope can never be published from this state (for example
			// the whisper expired between finalize and publish); be honest
			// instead of promising a delivery that will not happen.
			return h.sendReply(ctx, message, "Secret stored securely, but the group envelope could not be queued. It will not be delivered.", nil)
		}
		h.logger.WarnContext(ctx, "envelope publication queued for retry")
		if replyErr := h.sendReply(ctx, message, "Secret stored securely. The group envelope will be posted shortly.", nil); replyErr != nil {
			h.logger.WarnContext(ctx, "queued publication acknowledgement failed")
		}
		return nil
	}
	if err := h.sendReply(ctx, message, "Secret stored and the group envelope was posted.", nil); err != nil {
		h.logger.WarnContext(ctx, "published secret acknowledgement failed")
	}
	return nil
}

func (h *Handler) sendReply(
	ctx context.Context,
	message telegram.Message,
	text string,
	markup *telegram.InlineKeyboardMarkup,
) error {
	requestCtx, cancel := context.WithTimeout(ctx, h.requestTimeout)
	defer cancel()
	_, err := h.telegram.SendMessage(requestCtx, telegram.SendMessageRequest{
		ChatID: message.Chat.ID, MessageThreadID: optionalMessageID(message.MessageThreadID),
		Text: text, ReplyParameters: &telegram.ReplyParameters{MessageID: optionalMessageID(message.MessageID)},
		ReplyMarkup: markup,
	})
	return err
}

func composeURL(botUsername, parameter string) string {
	value := url.URL{Scheme: "https", Host: "t.me", Path: "/" + botUsername}
	query := value.Query()
	query.Set("start", parameter)
	value.RawQuery = query.Encode()
	return value.String()
}

func (h *Handler) mediaLimitText() string {
	const mebibyte = int64(1024 * 1024)
	if h.maxMediaBytes%mebibyte == 0 {
		return fmt.Sprintf("%d MiB", h.maxMediaBytes/mebibyte)
	}
	return fmt.Sprintf("%d bytes", h.maxMediaBytes)
}

func userMessage(err error) (string, bool) {
	switch {
	case errors.Is(err, service.ErrTargetRequired):
		return "Choose one recipient by replying to them, @username, or numeric Telegram ID.", true
	case errors.Is(err, service.ErrTargetNotObserved):
		return "I have not observed that user in this group. Reply to one of their messages or use an observed numeric ID.", true
	case errors.Is(err, service.ErrAmbiguousTarget):
		return "That cached username is ambiguous. Reply to the recipient or use their numeric ID.", true
	case errors.Is(err, service.ErrTargetIsBot):
		return "Bots cannot receive whispers.", true
	case errors.Is(err, service.ErrTargetIsSender):
		return "Choose someone other than yourself.", true
	case errors.Is(err, service.ErrChatNotAllowed):
		return "Whispers are not enabled in this group.", true
	case errors.Is(err, service.ErrTooManyDrafts):
		return "Finish or /cancel your active draft before starting another.", true
	case errors.Is(err, service.ErrRateLimited):
		return "You have reached the hourly whisper limit. Try again later.", true
	case errors.Is(err, service.ErrDraftNotFound):
		return "No active draft was found. Start with /whisper in a shared group.", true
	case errors.Is(err, service.ErrDraftExpired):
		return "That draft expired. Start a new /whisper in the group.", true
	case errors.Is(err, service.ErrDraftBusy):
		return "That draft is already being processed. Please wait a moment.", true
	case errors.Is(err, service.ErrUnsupportedContent):
		return "Send secret text or one supported media item.", true
	case errors.Is(err, service.ErrContentTooLarge):
		return "That media item exceeds the configured media size limit.", true
	case errors.Is(err, service.ErrTextTooLong):
		return "Secret text is too long (maximum 4096 characters).", true
	case errors.Is(err, service.ErrCaptionTooLong):
		return "The caption is too long (maximum 1024 characters).", true
	case errors.Is(err, service.ErrInvalidOpenToken), errors.Is(err, service.ErrWhisperNotFound):
		return "That whisper link is invalid or no longer available.", true
	case errors.Is(err, service.ErrGuestNotFound), errors.Is(err, service.ErrGuestExpired):
		return "That locked secret link is invalid or expired.", true
	case errors.Is(err, service.ErrGuestWrongRecipient):
		return "This locked secret is for another Telegram user.", true
	case errors.Is(err, service.ErrGuestAlreadyOpened):
		return "This locked secret was already opened.", true
	case errors.Is(err, service.ErrGuestSecretNotReady):
		return "The secret is not ready yet. Try again after the sender adds it.", true
	case errors.Is(err, service.ErrGuestActiveLimit):
		return "You already have an active locked secret. Finish it or use /cancel before creating another.", true
	case errors.Is(err, service.ErrGuestRateLimit):
		return "You have reached the hourly locked-secret limit. Try again later.", true
	case errors.Is(err, service.ErrGuestOpeningInProgress):
		return "A delivery attempt is already in progress. Try again in a few seconds.", true
	default:
		return "", false
	}
}

func (h *Handler) welcomeText() string {
	bot := "@" + h.botUsername
	return fmt.Sprintf("Welcome to Secret Whisper Bot 🔒\n\n"+
		"Two ways to send locked secrets:\n\n"+
		"1️⃣ Instant Text Whisper (In any chat):\n"+
		"Type %s @recipient your secret message\n"+
		"Tap the popup to post the locked whisper. The recipient can unlock it immediately.\n\n"+
		"2️⃣ Media/DM Whisper (Photos, Videos, Voice, Text):\n"+
		"• Inline: Type %s @recipient, post envelope, then tap to add secret privately in DM.\n"+
		"• Group: Reply to a user with /whisper, then send your secret here.\n\n"+
		"Use /privacy to review our encryption and privacy model.", bot, bot)
}

func (h *Handler) privateHelpText() string {
	bot := "@" + h.botUsername
	return fmt.Sprintf("Secret Whisper Bot Help 🔒\n\n"+
		"1️⃣ Instant Text Whisper (Any chat):\n"+
		"Type in any chat: %s @recipient your secret message\n"+
		"Tap the result to post. Recipient taps to unlock.\n\n"+
		"2️⃣ Media / DM Whisper:\n"+
		"Type %s @recipient in any chat or use /whisper in a shared group.\n"+
		"Send your secret photo, voice note, video, or document here (up to %s).\n"+
		"Use /cancel to discard an active composer draft and /privacy for security details.", bot, bot, h.mediaLimitText())
}

func (h *Handler) groupHelpText() string {
	bot := "@" + h.botUsername
	return fmt.Sprintf("How to whisper in this group 🔒\n\n"+
		"1. Reply to someone with /whisper, or use /whisper @username or /whisper 123456789.\n"+
		"2. The bot will open a private composer with you in DM to collect the secret safely.\n"+
		"3. Or type inline: %s @recipient your secret message to send an instant locked whisper.", bot)
}

const privacyText = "Privacy: secret payloads are encrypted in PostgreSQL and ordinary group members receive only an empty envelope. Content is retained for 30 days by default. One-time means one successful Telegram delivery, not proof that it was read. Ephemeral deletion and protect-content are best effort; screenshots, devices, backups, and Telegram copies may remain."
