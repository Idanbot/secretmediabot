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
	"github.com/idan/secretmediabot/internal/service"
	"github.com/idan/secretmediabot/internal/telegram"
)

const (
	ownerCallbackPrefix = "ol:"
	ownerPageSize       = 5
	ownerMaxPageSize    = 20
	ownerDefaultRetain  = 30 * 24 * time.Hour
)

type ownerListQuery struct {
	Limit          int
	Offset         int
	SenderID       *int64
	SenderUsername string
	MediaFilter    string
	MediaTypes     []domain.MediaType
	LastOnly       bool
}

func (h *Handler) ownerList(ctx context.Context, message telegram.Message, ownerID int64, mode, args string) error {
	query, err := parseOwnerListQuery(mode, args)
	if err != nil {
		return h.sendReply(ctx, message, ownerUsage(mode), nil)
	}
	return h.ownerListQuery(ctx, message, ownerID, query, nil)
}

func parseOwnerListQuery(mode, args string) (ownerListQuery, error) {
	query := ownerListQuery{Limit: ownerPageSize}
	fields := strings.Fields(args)
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "list"
	}

	switch mode {
	case "sender":
		if len(fields) != 1 {
			return ownerListQuery{}, errors.New("sender filter requires one target")
		}
		return applyOwnerSenderFilter(query, fields[0])
	case "media":
		if len(fields) != 1 {
			return ownerListQuery{}, errors.New("media filter requires one type")
		}
		return applyOwnerMediaFilter(query, fields[0])
	case "last":
		if len(fields) > 1 {
			return ownerListQuery{}, errors.New("last filter accepts at most one type")
		}
		query.Limit = 1
		query.LastOnly = true
		if len(fields) == 1 {
			return applyOwnerMediaFilter(query, fields[0])
		}
		return query, nil
	case "list":
		if len(fields) == 0 {
			return query, nil
		}
		switch strings.ToLower(fields[0]) {
		case "sender":
			if len(fields) != 2 {
				return ownerListQuery{}, errors.New("sender filter requires one target")
			}
			return applyOwnerSenderFilter(query, fields[1])
		case "media":
			if len(fields) != 2 {
				return ownerListQuery{}, errors.New("media filter requires one type")
			}
			return applyOwnerMediaFilter(query, fields[1])
		case "last":
			if len(fields) > 2 {
				return ownerListQuery{}, errors.New("last filter accepts at most one type")
			}
			query.Limit = 1
			query.LastOnly = true
			if len(fields) == 2 {
				return applyOwnerMediaFilter(query, fields[1])
			}
			return query, nil
		}
		if len(fields) > 2 {
			return ownerListQuery{}, errors.New("too many pagination arguments")
		}
		limit, err := strconv.Atoi(fields[0])
		if err != nil || limit < 1 || limit > ownerMaxPageSize {
			return ownerListQuery{}, errors.New("invalid page size")
		}
		query.Limit = limit
		if len(fields) == 2 {
			query.Offset, err = strconv.Atoi(fields[1])
			if err != nil || query.Offset < 0 {
				return ownerListQuery{}, errors.New("invalid page offset")
			}
		}
		return query, nil
	default:
		return ownerListQuery{}, errors.New("unknown owner list mode")
	}
}

func applyOwnerSenderFilter(query ownerListQuery, value string) (ownerListQuery, error) {
	target, err := command.ParseTarget(value)
	if err != nil {
		return ownerListQuery{}, err
	}
	if target.Kind == command.TargetUserID {
		query.SenderID = &target.UserID
		return query, nil
	}
	query.SenderUsername = target.Username
	return query, nil
}

func applyOwnerMediaFilter(query ownerListQuery, value string) (ownerListQuery, error) {
	filter, mediaTypes, ok := parseOwnerMediaFilter(value)
	if !ok {
		return ownerListQuery{}, errors.New("invalid media type")
	}
	query.MediaFilter = filter
	query.MediaTypes = mediaTypes
	return query, nil
}

func parseOwnerMediaFilter(value string) (string, []domain.MediaType, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "image", "images", "photo", "photos":
		return "image", []domain.MediaType{domain.MediaPhoto}, true
	case "video", "videos":
		return "video", []domain.MediaType{domain.MediaVideo}, true
	case "recording", "recordings":
		return "recording", []domain.MediaType{domain.MediaVoice, domain.MediaAudio}, true
	case "voice", "voice_note", "voice-note":
		return "voice", []domain.MediaType{domain.MediaVoice}, true
	case "audio", "audio_file", "audio-file":
		return "audio", []domain.MediaType{domain.MediaAudio}, true
	case "document", "documents", "file", "files":
		return "document", []domain.MediaType{domain.MediaDocument}, true
	default:
		return "", nil, false
	}
}

func ownerUsage(mode string) string {
	switch mode {
	case "sender":
		return "Usage: /owner_sender <telegram-id|@username>"
	case "media":
		return "Usage: /owner_media <image|video|recording|voice|audio|document>"
	case "last":
		return "Usage: /owner_last [image|video|recording|voice|audio|document]"
	default:
		return "Usage: /owner_list [page-size 1-20] [offset >=0]\nOr: /owner_list sender <id|@username>\nOr: /owner_list media <image|video|recording>\nOr: /owner_list last [media-type]"
	}
}

func (h *Handler) ownerListQuery(
	ctx context.Context,
	message telegram.Message,
	ownerID int64,
	query ownerListQuery,
	callbackMessage *telegram.Message,
) error {
	details, err := h.service.OwnerListDetails(ctx, ownerID, service.OwnerListOptions{
		Limit:          query.Limit,
		Offset:         query.Offset,
		SenderID:       query.SenderID,
		SenderUsername: query.SenderUsername,
		MediaTypes:     query.MediaTypes,
	})
	if err != nil {
		return err
	}

	displayQuery := query
	if query.SenderID == nil && query.SenderUsername != "" && len(details) > 0 {
		resolvedID := details[0].Whisper.SenderID
		query.SenderID = &resolvedID
	}

	text := renderOwnerList(displayQuery, details)
	markup := ownerListMarkup(query, details)
	if callbackMessage != nil {
		if err := h.editOwnerMessage(ctx, *callbackMessage, text, markup); err != nil {
			return err
		}
		return nil
	}
	return h.sendReply(ctx, message, text, markup)
}

func renderOwnerList(query ownerListQuery, details []domain.OwnerWhisper) string {
	if len(details) == 0 {
		if query.MediaFilter != "" || query.SenderID != nil || query.SenderUsername != "" {
			return fmt.Sprintf("📋 No retained whispers match this %s filter.", ownerFilterLabel(query))
		}
		return "📋 No retained whispers."
	}

	page := (query.Offset / query.Limit) + 1
	var output strings.Builder
	if query.LastOnly {
		fmt.Fprintf(&output, "🔎 Latest %s whisper\n", ownerFilterLabel(query))
	} else {
		fmt.Fprintf(&output, "📋 Retained whispers · page %d\n", page)
	}
	fmt.Fprintf(&output, "Filter: %s\nTap an item to expand its safe metadata.", ownerFilterLabel(query))
	for index, detail := range details {
		whisper := detail.Whisper
		fmt.Fprintf(&output, "\n\n%d. %s · %s → %s\n   %s · %s",
			query.Offset+index+1,
			ownerMediaLabel(whisper),
			ownerUserLabel(detail.Sender),
			ownerUserLabel(detail.Recipient),
			formatWhisperStatus(whisper.Status, whisper.PublishState),
			whisper.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"))
	}
	return output.String()
}

func ownerListMarkup(query ownerListQuery, details []domain.OwnerWhisper) *telegram.InlineKeyboardMarkup {
	rows := make([][]telegram.InlineKeyboardButton, 0, len(details)+3)
	state := encodeOwnerState(query)
	for index, detail := range details {
		whisper := detail.Whisper
		rows = append(rows, []telegram.InlineKeyboardButton{{
			Text:         fmt.Sprintf("%d · %s %s → %s", query.Offset+index+1, ownerMediaIcon(whisper), ownerShortUserLabel(detail.Sender), ownerShortUserLabel(detail.Recipient)),
			CallbackData: ownerCallbackItem(whisper.ID, state),
		}})
	}

	if !query.LastOnly {
		navigation := make([]telegram.InlineKeyboardButton, 0, 2)
		if query.Offset > 0 {
			previous := query
			previous.Offset -= query.Limit
			if previous.Offset < 0 {
				previous.Offset = 0
			}
			navigation = append(navigation, telegram.InlineKeyboardButton{Text: "◀ Previous", CallbackData: ownerCallbackPage(encodeOwnerState(previous))})
		}
		if len(details) == query.Limit {
			next := query
			next.Offset += query.Limit
			navigation = append(navigation, telegram.InlineKeyboardButton{Text: "Next ▶", CallbackData: ownerCallbackPage(encodeOwnerState(next))})
		}
		if len(navigation) > 0 {
			rows = append(rows, navigation)
		}
	}

	rows = append(rows,
		[]telegram.InlineKeyboardButton{
			{Text: "All", CallbackData: ownerCallbackFilter("all")},
			{Text: "🖼 Image", CallbackData: ownerCallbackFilter("image")},
		},
		[]telegram.InlineKeyboardButton{
			{Text: "🎥 Video", CallbackData: ownerCallbackFilter("video")},
			{Text: "🎙 Recording", CallbackData: ownerCallbackFilter("recording")},
		},
	)
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func ownerMenuMarkup() *telegram.InlineKeyboardMarkup {
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
		{{Text: "📋 Browse whispers", CallbackData: ownerCallbackFilter("all")}},
		{{Text: "🖼 Images", CallbackData: ownerCallbackFilter("image")}, {Text: "🎥 Videos", CallbackData: ownerCallbackFilter("video")}},
		{{Text: "🎙 Recordings", CallbackData: ownerCallbackFilter("recording")}},
	}}
}

func ownerFilterLabel(query ownerListQuery) string {
	switch {
	case query.SenderID != nil:
		return fmt.Sprintf("sender %d", *query.SenderID)
	case query.SenderUsername != "":
		return "sender @" + strings.TrimPrefix(query.SenderUsername, "@")
	case query.MediaFilter != "":
		return query.MediaFilter
	default:
		return "all whispers"
	}
}

func ownerMediaLabel(whisper domain.Whisper) string {
	if whisper.Content.Kind != domain.PayloadMedia || whisper.Content.Media == nil {
		return "📝 text"
	}
	return ownerMediaIcon(whisper) + " " + ownerMediaTypeLabel(whisper.Content.Media.Type)
}

func ownerMediaIcon(whisper domain.Whisper) string {
	if whisper.Content.Kind != domain.PayloadMedia || whisper.Content.Media == nil {
		return "📝"
	}
	switch whisper.Content.Media.Type {
	case domain.MediaPhoto:
		return "🖼"
	case domain.MediaVideo:
		return "🎥"
	case domain.MediaVoice, domain.MediaAudio:
		return "🎙"
	case domain.MediaDocument:
		return "📄"
	default:
		return "📦"
	}
}

func ownerMediaTypeLabel(mediaType domain.MediaType) string {
	switch mediaType {
	case domain.MediaPhoto:
		return "image"
	case domain.MediaVideo:
		return "video"
	case domain.MediaVoice:
		return "voice recording"
	case domain.MediaAudio:
		return "audio recording"
	case domain.MediaDocument:
		return "document"
	default:
		return string(mediaType)
	}
}

func ownerUserLabel(user domain.User) string {
	label := user.DisplayName()
	if username := strings.TrimPrefix(strings.TrimSpace(user.Username), "@"); username != "" && !strings.EqualFold(label, "@"+username) {
		label += " (@" + username + ")"
	}
	if user.TelegramUserID > 0 {
		return fmt.Sprintf("%s [%d]", label, user.TelegramUserID)
	}
	return label
}

func ownerShortUserLabel(user domain.User) string {
	label := user.DisplayName()
	if len([]rune(label)) > 18 {
		label = string([]rune(label)[:17]) + "…"
	}
	return label
}

func formatWhisperStatus(status domain.WhisperStatus, pub domain.PublishState) string {
	switch status {
	case domain.WhisperActive:
		if pub == domain.PublishPublished {
			return "🟢 active · published"
		}
		return "🟢 active"
	case domain.WhisperOpening:
		return "🟡 opening"
	case domain.WhisperOpened:
		return "📬 opened"
	case domain.WhisperExpired:
		return "⌛ expired"
	case domain.WhisperRevoked:
		return "🚫 revoked"
	default:
		return string(status)
	}
}

func encodeOwnerState(query ownerListQuery) string {
	limit := strconv.FormatInt(int64(query.Limit), 36)
	offset := strconv.FormatInt(int64(query.Offset), 36)
	switch {
	case query.SenderID != nil:
		return "s" + strconv.FormatInt(*query.SenderID, 36) + "." + limit + "." + offset
	case query.MediaFilter != "":
		return "m" + ownerMediaFilterCode(query.MediaFilter) + limit + "." + offset
	default:
		return "a" + limit + "." + offset
	}
}

func parseOwnerState(value string) (ownerListQuery, error) {
	if len(value) < 2 {
		return ownerListQuery{}, errors.New("invalid owner page state")
	}
	query := ownerListQuery{Limit: ownerPageSize}
	var offsetText string
	switch value[0] {
	case 'a':
		parts := strings.Split(value[1:], ".")
		if len(parts) == 2 {
			limit, err := strconv.ParseInt(parts[0], 36, 64)
			if err != nil || limit < 1 || limit > ownerMaxPageSize {
				return ownerListQuery{}, errors.New("invalid owner page size")
			}
			query.Limit = int(limit)
			offsetText = parts[1]
		} else if len(parts) == 1 {
			// Accept callbacks emitted by older bot instances.
			offsetText = parts[0]
		} else {
			return ownerListQuery{}, errors.New("invalid owner page state")
		}
	case 'm':
		if len(value) < 3 {
			return ownerListQuery{}, errors.New("invalid owner media state")
		}
		filter, mediaTypes, ok := parseOwnerMediaFilter(ownerMediaFilterName(value[1]))
		if !ok {
			return ownerListQuery{}, errors.New("invalid owner media state")
		}
		query.MediaFilter, query.MediaTypes = filter, mediaTypes
		parts := strings.Split(value[2:], ".")
		if len(parts) == 2 {
			limit, err := strconv.ParseInt(parts[0], 36, 64)
			if err != nil || limit < 1 || limit > ownerMaxPageSize {
				return ownerListQuery{}, errors.New("invalid owner page size")
			}
			query.Limit = int(limit)
			offsetText = parts[1]
		} else if len(parts) == 1 {
			offsetText = parts[0]
		} else {
			return ownerListQuery{}, errors.New("invalid owner media state")
		}
	case 's':
		parts := strings.Split(value[1:], ".")
		if len(parts) != 2 && len(parts) != 3 {
			return ownerListQuery{}, errors.New("invalid owner sender state")
		}
		senderID, err := strconv.ParseInt(parts[0], 36, 64)
		if err != nil || senderID <= 0 {
			return ownerListQuery{}, errors.New("invalid owner sender state")
		}
		query.SenderID = &senderID
		if len(parts) == 3 {
			limit, err := strconv.ParseInt(parts[1], 36, 64)
			if err != nil || limit < 1 || limit > ownerMaxPageSize {
				return ownerListQuery{}, errors.New("invalid owner page size")
			}
			query.Limit = int(limit)
			offsetText = parts[2]
		} else {
			offsetText = parts[1]
		}
	default:
		return ownerListQuery{}, errors.New("unknown owner page state")
	}
	offset, err := strconv.ParseInt(offsetText, 36, 64)
	if err != nil || offset < 0 || offset > int64(^uint(0)>>1) {
		return ownerListQuery{}, errors.New("invalid owner page offset")
	}
	query.Offset = int(offset)
	return query, nil
}

func ownerMediaFilterCode(filter string) string {
	switch filter {
	case "image":
		return "i"
	case "video":
		return "v"
	case "recording":
		return "r"
	case "voice":
		return "w"
	case "audio":
		return "a"
	case "document":
		return "d"
	default:
		return "x"
	}
}

func ownerMediaFilterName(code byte) string {
	switch code {
	case 'i':
		return "image"
	case 'v':
		return "video"
	case 'r':
		return "recording"
	case 'w':
		return "voice"
	case 'a':
		return "audio"
	case 'd':
		return "document"
	default:
		return ""
	}
}

func ownerCallbackFilter(filter string) string {
	return ownerCallbackPrefix + "f:" + map[string]string{
		"all": "a", "image": "i", "video": "v", "recording": "r",
	}[filter]
}

func ownerCallbackPage(state string) string {
	return ownerCallbackPrefix + "p:" + state
}

func ownerCallbackItem(id uuid.UUID, state string) string {
	return ownerCallbackPrefix + "i:" + compactOwnerUUID(id) + ":" + state
}

func compactOwnerUUID(id uuid.UUID) string {
	return strings.ReplaceAll(id.String(), "-", "")
}

func parseCompactOwnerUUID(value string) (uuid.UUID, error) {
	if len(value) != 32 {
		return uuid.Nil, errors.New("invalid owner whisper ID")
	}
	value = value[:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:]
	return uuid.Parse(value)
}

func (h *Handler) handleOwnerCallback(ctx context.Context, callback telegram.CallbackQuery) error {
	if !h.service.IsOwner(callback.From.ID) {
		return h.answerCallback(ctx, callback.ID, "This owner control is not available to you.", true)
	}
	if callback.Message == nil || callback.Message.Chat.ID == 0 || callback.Message.MessageID <= 0 {
		return h.answerCallback(ctx, callback.ID, "This owner view has expired. Run /owner_list again.", true)
	}

	parts := strings.Split(callback.Data, ":")
	if len(parts) < 3 || parts[0] != "ol" {
		return h.answerCallback(ctx, callback.ID, "Unknown owner control.", true)
	}
	callbackMessage := *callback.Message
	switch parts[1] {
	case "f":
		query := ownerListQuery{Limit: ownerPageSize}
		if len(parts[2]) != 1 {
			return h.answerCallback(ctx, callback.ID, "Unknown owner filter.", true)
		}
		if parts[2] != "a" {
			filter, mediaTypes, ok := parseOwnerMediaFilter(ownerMediaFilterName(parts[2][0]))
			if !ok {
				return h.answerCallback(ctx, callback.ID, "Unknown owner filter.", true)
			}
			query.MediaFilter, query.MediaTypes = filter, mediaTypes
		}
		return h.ownerListCallback(ctx, callback, callbackMessage, query)
	case "p":
		query, err := parseOwnerState(parts[2])
		if err != nil {
			return h.answerCallback(ctx, callback.ID, "This owner page has expired. Run /owner_list again.", true)
		}
		return h.ownerListCallback(ctx, callback, callbackMessage, query)
	case "i":
		if len(parts) != 4 {
			return h.answerCallback(ctx, callback.ID, "Invalid owner item.", true)
		}
		id, err := parseCompactOwnerUUID(parts[2])
		if err != nil {
			return h.answerCallback(ctx, callback.ID, "Invalid owner whisper ID.", true)
		}
		return h.ownerDetailCallback(ctx, callback, callbackMessage, id, parts[3])
	case "o", "dd", "r":
		if len(parts) != 3 {
			return h.answerCallback(ctx, callback.ID, "Invalid owner action.", true)
		}
		id, err := parseCompactOwnerUUID(parts[2])
		if err != nil {
			return h.answerCallback(ctx, callback.ID, "Invalid owner whisper ID.", true)
		}
		switch parts[1] {
		case "o":
			if err := h.ownerOpen(ctx, callbackMessage, callback.From.ID, id.String()); err != nil {
				return h.ownerCallbackError(ctx, callback.ID, err)
			}
			return h.answerCallback(ctx, callback.ID, "Content sent privately.", false)
		case "dd":
			if err := h.service.OwnerDelete(ctx, callback.From.ID, id); err != nil {
				return h.ownerCallbackError(ctx, callback.ID, err)
			}
			if err := h.editOwnerMessage(ctx, callbackMessage, "🗑 Whisper deleted from live PostgreSQL. Telegram or backup copies may remain.", ownerBackMarkup()); err != nil {
				return err
			}
			return h.answerCallback(ctx, callback.ID, "Whisper deleted.", false)
		case "r":
			if err := h.service.OwnerSetRetention(ctx, callback.From.ID, id, ownerDefaultRetain); err != nil {
				return h.ownerCallbackError(ctx, callback.ID, err)
			}
			return h.answerCallback(ctx, callback.ID, "Retention extended by 30 days.", false)
		}
	case "dc":
		if len(parts) != 4 {
			return h.answerCallback(ctx, callback.ID, "Invalid owner delete action.", true)
		}
		id, err := parseCompactOwnerUUID(parts[2])
		if err != nil {
			return h.answerCallback(ctx, callback.ID, "Invalid owner whisper ID.", true)
		}
		if err := h.editOwnerMessage(ctx, callbackMessage, "⚠️ Delete this whisper and its encrypted payloads?", ownerDeleteConfirmMarkup(id, parts[3])); err != nil {
			return err
		}
		return h.answerCallback(ctx, callback.ID, "Confirm deletion.", false)
	}
	return h.answerCallback(ctx, callback.ID, "Unknown owner control.", true)
}

func (h *Handler) ownerListCallback(ctx context.Context, callback telegram.CallbackQuery, message telegram.Message, query ownerListQuery) error {
	details, err := h.service.OwnerListDetails(ctx, callback.From.ID, service.OwnerListOptions{
		Limit: query.Limit, Offset: query.Offset, SenderID: query.SenderID,
		SenderUsername: query.SenderUsername, MediaTypes: query.MediaTypes,
	})
	if err != nil {
		return h.ownerCallbackError(ctx, callback.ID, err)
	}
	displayQuery := query
	if query.SenderID == nil && query.SenderUsername != "" && len(details) > 0 {
		resolvedID := details[0].Whisper.SenderID
		query.SenderID = &resolvedID
	}
	if err := h.editOwnerMessage(ctx, message, renderOwnerList(displayQuery, details), ownerListMarkup(query, details)); err != nil {
		return err
	}
	return h.answerCallback(ctx, callback.ID, "Owner list updated.", false)
}

func (h *Handler) ownerDetailCallback(ctx context.Context, callback telegram.CallbackQuery, message telegram.Message, id uuid.UUID, state string) error {
	detail, err := h.service.OwnerMetadata(ctx, callback.From.ID, id)
	if err != nil {
		return h.ownerCallbackError(ctx, callback.ID, err)
	}
	if _, err := parseOwnerState(state); err != nil {
		state = "a0"
	}
	if err := h.editOwnerMessage(ctx, message, renderOwnerDetail(detail), ownerDetailMarkup(id, state)); err != nil {
		return err
	}
	return h.answerCallback(ctx, callback.ID, "Expanded whisper metadata.", false)
}

func renderOwnerDetail(detail domain.OwnerWhisper) string {
	whisper := detail.Whisper
	var output strings.Builder
	fmt.Fprintf(&output, "📦 Whisper %s\n\n", whisper.ID)
	fmt.Fprintf(&output, "Media: %s\n", ownerMediaLabel(whisper))
	fmt.Fprintf(&output, "Sender: %s\n", ownerUserLabel(detail.Sender))
	fmt.Fprintf(&output, "Receiver: %s\n", ownerUserLabel(detail.Recipient))
	fmt.Fprintf(&output, "Status: %s\n", formatWhisperStatus(whisper.Status, whisper.PublishState))
	fmt.Fprintf(&output, "Created: %s", whisper.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
	if !whisper.ExpiresAt.IsZero() {
		fmt.Fprintf(&output, "\nExpires: %s", whisper.ExpiresAt.UTC().Format("2006-01-02 15:04:05 UTC"))
	}
	if whisper.MetadataRetainUntil != nil {
		fmt.Fprintf(&output, "\nMetadata retention: %s", whisper.MetadataRetainUntil.UTC().Format("2006-01-02 15:04:05 UTC"))
	}
	return output.String()
}

func ownerDetailMarkup(id uuid.UUID, state string) *telegram.InlineKeyboardMarkup {
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
		{{Text: "🔐 Open content privately", CallbackData: ownerCallbackPrefix + "o:" + compactOwnerUUID(id)}},
		{{Text: "🗑 Delete", CallbackData: ownerCallbackPrefix + "dc:" + compactOwnerUUID(id) + ":" + state}, {Text: "⏱ Retain 30 days", CallbackData: ownerCallbackPrefix + "r:" + compactOwnerUUID(id)}},
		{{Text: "◀ Back to list", CallbackData: ownerCallbackPage(state)}},
	}}
}

func ownerDeleteConfirmMarkup(id uuid.UUID, state string) *telegram.InlineKeyboardMarkup {
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
		{{Text: "✅ Delete permanently", CallbackData: ownerCallbackPrefix + "dd:" + compactOwnerUUID(id)}},
		{{Text: "Cancel", CallbackData: ownerCallbackPrefix + "i:" + compactOwnerUUID(id) + ":" + state}},
	}}
}

func ownerBackMarkup() *telegram.InlineKeyboardMarkup {
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{{
		{Text: "◀ Back to whispers", CallbackData: ownerCallbackFilter("all")},
	}}}
}

func (h *Handler) editOwnerMessage(ctx context.Context, message telegram.Message, text string, markup *telegram.InlineKeyboardMarkup) error {
	requestCtx, cancel := context.WithTimeout(ctx, h.requestTimeout)
	defer cancel()
	_, err := h.telegram.EditMessageText(requestCtx, telegram.EditMessageTextRequest{
		ChatID: message.Chat.ID, MessageID: message.MessageID, Text: text, ReplyMarkup: markup,
	})
	return err
}

func (h *Handler) ownerCallbackError(ctx context.Context, callbackID string, err error) error {
	text := "Owner action failed. Try again shortly."
	if userText, expected := userMessage(err); expected {
		text = userText
	}
	answerErr := h.answerCallback(ctx, callbackID, text, true)
	if answerErr != nil {
		return errors.Join(err, answerErr)
	}
	if _, expected := userMessage(err); expected {
		return nil
	}
	return err
}
