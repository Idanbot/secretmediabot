package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/command"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/telegram"
)

func (h *Handler) handleOwnerCommand(
	ctx context.Context,
	message telegram.Message,
	owner domain.User,
	parsed command.Command,
) error {
	if !h.service.IsOwner(owner.TelegramUserID) {
		return h.sendReply(ctx, message, "This command is owner-only.", nil)
	}
	switch parsed.Name {
	case "owner_menu":
		return h.ownerMenu(ctx, message, owner.TelegramUserID)
	case "owner_ephemeral":
		return h.ownerEphemeral(ctx, message, parsed.Args)
	case "owner_list", "owner_sender", "owner_media", "owner_last":
		return h.ownerList(ctx, message, owner.TelegramUserID, strings.TrimPrefix(parsed.Name, "owner_"), parsed.Args)
	case "owner_open", "owner_review":
		return h.ownerOpen(ctx, message, owner.TelegramUserID, parsed.Args)
	case "owner_delete":
		return h.ownerDelete(ctx, message, owner.TelegramUserID, parsed.Args)
	case "owner_retain", "owner_set_retention":
		return h.ownerRetain(ctx, message, owner.TelegramUserID, parsed.Args)
	default:
		return nil
	}
}

func (h *Handler) ownerMenu(ctx context.Context, message telegram.Message, ownerID int64) error {
	ephemeral := h.service.GetEphemeralDeleteAfter()
	ephemeralText := "⚪ Disabled (secrets persist until global retention expires)"
	if ephemeral > 0 {
		ephemeralText = fmt.Sprintf("🔴 Enabled (auto-deletes %s after open)", ephemeral)
	}

	var buildSection strings.Builder
	buildSection.WriteString("🚀 Build & Deployment:")

	version := h.buildInfo.Version
	if version == "" {
		version = "dev"
	}
	fmt.Fprintf(&buildSection, "\n• Version: %s", version)

	if h.buildInfo.CIRunNumber != "" && h.buildInfo.CIRunNumber != "local" {
		fmt.Fprintf(&buildSection, "\n• CI Run: #%s", h.buildInfo.CIRunNumber)
	} else {
		buildSection.WriteString("\n• Environment: Local / Development")
	}

	commit := h.buildInfo.Commit
	if len(commit) > 7 {
		commit = commit[:7]
	}
	if commit != "" && commit != "unknown" {
		fmt.Fprintf(&buildSection, "\n• Commit: %s", commit)
	}

	if h.buildInfo.CommitMessage != "" && h.buildInfo.CommitMessage != "unknown" {
		fmt.Fprintf(&buildSection, "\n• Message: %s", h.buildInfo.CommitMessage)
	}

	if h.buildInfo.BuildTime != "" && h.buildInfo.BuildTime != "unknown" {
		fmt.Fprintf(&buildSection, "\n• Built: %s", h.buildInfo.BuildTime)
	}

	text := fmt.Sprintf(`🛡️ Secret Media Bot — Operator Menu

%s

⏱️ Self-Destruction on Open:
• Status: %s
• Quick Controls:
  /owner_ephemeral off — Turn off self-destruction
  /owner_ephemeral 30s — Auto-delete 30s after open
  /owner_ephemeral 1m — Auto-delete 1m after open
  /owner_ephemeral 5m — Auto-delete 5m after open
  /owner_ephemeral <duration> — Set custom delay (e.g. 10m)

📋 Auditing & Whisper Management:
• /owner_list — Browse retained whispers (tap an item to expand)
• /owner_sender <id|@username> — Browse one sender
• /owner_media <image|video|recording> — Browse a media category
• /owner_last [image|video|recording] — Show the latest matching whisper
• /owner_open <whisper-uuid> — Send decrypted content privately
• /owner_delete <whisper-uuid> — Hard-delete whisper and payload
• /owner_retain <whisper-uuid> <duration> — Adjust retention window`, buildSection.String(), ephemeralText)

	return h.sendReply(ctx, message, text, ownerMenuMarkup())
}

func (h *Handler) ownerEphemeral(ctx context.Context, message telegram.Message, args string) error {
	arg := strings.TrimSpace(strings.ToLower(args))
	if arg == "" {
		ephemeral := h.service.GetEphemeralDeleteAfter()
		if ephemeral <= 0 {
			return h.sendReply(ctx, message, "Self-destruction on open is currently DISABLED.", nil)
		}
		return h.sendReply(ctx, message, fmt.Sprintf("Self-destruction on open is currently set to %s.", ephemeral), nil)
	}

	if arg == "off" || arg == "disable" || arg == "disabled" || arg == "0" || arg == "0s" {
		h.service.SetEphemeralDeleteAfter(0)
		return h.sendReply(ctx, message, "Self-destruction on open has been DISABLED.", nil)
	}

	dur, err := time.ParseDuration(arg)
	if err != nil || dur <= 0 {
		return h.sendReply(ctx, message, "Invalid duration. Use 'off' or a positive duration like '30s', '1m', '5m'.", nil)
	}

	h.service.SetEphemeralDeleteAfter(dur)
	return h.sendReply(ctx, message, fmt.Sprintf("Self-destruction on open is now set to %s.", dur), nil)
}

func (h *Handler) ownerOpen(ctx context.Context, message telegram.Message, ownerID int64, args string) error {
	id, ok := parseWhisperID(args)
	if !ok {
		return h.sendReply(ctx, message, "Usage: /owner_open <whisper-uuid>", nil)
	}
	review, err := h.service.OwnerReview(ctx, ownerID, id)
	if err != nil {
		if text, expected := userMessage(err); expected {
			return h.sendReply(ctx, message, text, nil)
		}
		return err
	}
	defer review.Zero()
	switch review.Content.Kind {
	case domain.PayloadText:
		requestCtx, cancel := context.WithTimeout(ctx, h.requestTimeout)
		_, err = h.telegram.SendMessage(requestCtx, telegram.SendMessageRequest{
			ChatID: message.Chat.ID, Text: string(review.Content.Text), ProtectContent: true,
		})
		cancel()
	case domain.PayloadMedia:
		if review.Content.Media == nil {
			return errors.New("owner media review has no media metadata")
		}
		requestCtx, cancel := context.WithTimeout(ctx, h.mediaDownloadTimeout)
		_, err = h.telegram.SendPrivateMedia(requestCtx, telegram.SendPrivateMediaRequest{
			ChatID: message.Chat.ID, Type: review.Content.Media.Type,
			Data: review.Content.MediaBytes, ContentType: review.Content.Media.ContentType,
			Caption: string(review.Content.Caption), ProtectContent: true,
		})
		cancel()
	default:
		return errors.New("owner review has unsupported payload kind")
	}
	return err
}

func (h *Handler) ownerDelete(ctx context.Context, message telegram.Message, ownerID int64, args string) error {
	id, ok := parseWhisperID(args)
	if !ok {
		return h.sendReply(ctx, message, "Usage: /owner_delete <whisper-uuid>", nil)
	}
	if err := h.service.OwnerDelete(ctx, ownerID, id); err != nil {
		if text, expected := userMessage(err); expected {
			return h.sendReply(ctx, message, text, nil)
		}
		return err
	}
	if err := h.sendReply(ctx, message, "Whisper and encrypted payload deleted from live PostgreSQL. Telegram or backup copies may remain.", nil); err != nil {
		h.logger.WarnContext(ctx, "owner deletion acknowledgement failed")
	}
	return nil
}

func (h *Handler) ownerRetain(ctx context.Context, message telegram.Message, ownerID int64, args string) error {
	fields := strings.Fields(args)
	if len(fields) != 2 {
		return h.sendReply(ctx, message, "Usage: /owner_retain <whisper-uuid> <duration>, for example 720h", nil)
	}
	id, err := uuid.Parse(fields[0])
	if err != nil {
		return h.sendReply(ctx, message, "The whisper ID must be a UUID.", nil)
	}
	duration, err := time.ParseDuration(fields[1])
	if err != nil || duration <= 0 {
		return h.sendReply(ctx, message, "Retention must be a positive Go duration, for example 720h.", nil)
	}
	if err := h.service.OwnerSetRetention(ctx, ownerID, id, duration); err != nil {
		if text, expected := userMessage(err); expected {
			return h.sendReply(ctx, message, text, nil)
		}
		return err
	}
	if err := h.sendReply(ctx, message, "Retention deadline updated for metadata and encrypted content.", nil); err != nil {
		h.logger.WarnContext(ctx, "owner retention acknowledgement failed")
	}
	return nil
}

func parseWhisperID(value string) (uuid.UUID, bool) {
	fields := strings.Fields(value)
	if len(fields) != 1 {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(fields[0])
	return id, err == nil && id != uuid.Nil
}
