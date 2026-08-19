package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
	case "owner_list":
		return h.ownerList(ctx, message, owner.TelegramUserID, parsed.Args)
	case "owner_open":
		return h.ownerOpen(ctx, message, owner.TelegramUserID, parsed.Args)
	case "owner_delete":
		return h.ownerDelete(ctx, message, owner.TelegramUserID, parsed.Args)
	case "owner_retain":
		return h.ownerRetain(ctx, message, owner.TelegramUserID, parsed.Args)
	default:
		return nil
	}
}

func (h *Handler) ownerMenu(ctx context.Context, message telegram.Message, ownerID int64) error {
	ephemeral := h.service.GetEphemeralDeleteAfter()
	ephemeralText := "Disabled (secrets persist until global retention expires)"
	if ephemeral > 0 {
		ephemeralText = fmt.Sprintf("Enabled (auto-deletes %s after open)", ephemeral)
	}

	text := fmt.Sprintf(`🛡️ Secret Media Bot — Operator Menu

⏱️ Self-Destruction on Open: %s
• /owner_ephemeral off — Turn self-destruction off
• /owner_ephemeral 30s — Auto-delete 30s after open
• /owner_ephemeral 1m — Auto-delete 1m after open
• /owner_ephemeral 5m — Auto-delete 5m after open
• /owner_ephemeral <duration> — Custom duration (e.g. 10m)

📋 Auditing & Whisper Management:
• /owner_list [limit] [offset] — List recent whispers (metadata only)
• /owner_open <whisper-uuid> — Review decrypted whisper payload
• /owner_delete <whisper-uuid> — Hard-delete whisper and payload
• /owner_retain <whisper-uuid> <duration> — Adjust retention window`, ephemeralText)

	return h.sendReply(ctx, message, text, nil)
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

func (h *Handler) ownerList(ctx context.Context, message telegram.Message, ownerID int64, args string) error {
	limit := 20
	offset := 0
	fields := strings.Fields(args)
	if len(fields) > 2 {
		return h.sendReply(ctx, message, "Usage: /owner_list [limit 1-50] [offset >=0]", nil)
	}
	if len(fields) >= 1 {
		parsed, err := strconv.Atoi(fields[0])
		if err != nil || parsed < 1 || parsed > 50 {
			return h.sendReply(ctx, message, "Usage: /owner_list [limit 1-50] [offset >=0]", nil)
		}
		limit = parsed
	}
	if len(fields) == 2 {
		parsed, err := strconv.Atoi(fields[1])
		if err != nil || parsed < 0 {
			return h.sendReply(ctx, message, "Usage: /owner_list [limit 1-50] [offset >=0]", nil)
		}
		offset = parsed
	}
	whispers, err := h.service.OwnerList(ctx, ownerID, limit, offset)
	if err != nil {
		return err
	}
	if len(whispers) == 0 {
		return h.sendReply(ctx, message, "No retained whispers.", nil)
	}
	var output strings.Builder
	output.WriteString("Retained whispers (metadata only):\n")
	for _, whisper := range whispers {
		fmt.Fprintf(&output, "\n%s | %s/%s | %s | sender %d → recipient %d | created %s",
			whisper.ID, whisper.Status, whisper.PublishState, whisper.Content.Kind,
			whisper.SenderID, whisper.RecipientID, whisper.CreatedAt.UTC().Format(time.RFC3339),
		)
	}
	if len(whispers) == limit {
		fmt.Fprintf(&output, "\n\nNext page: /owner_list %d %d", limit, offset+limit)
	}
	return h.sendLongReply(ctx, message, output.String())
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
