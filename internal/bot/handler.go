// Package bot translates Telegram updates into application use cases. It keeps
// transport parsing and user-facing copy out of the domain and repository.
package bot

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/service"
	"github.com/idan/secretmediabot/internal/telegram"
)

type useCases interface {
	ObserveMembership(context.Context, domain.User, domain.Chat) error
	RegisterPrivateUser(context.Context, domain.User) (domain.User, error)
	CreateDraft(context.Context, service.CreateDraftRequest) (service.CreateDraftResult, error)
	ResumeDraft(context.Context, domain.User, string) (service.ResumeDraftResult, error)
	CancelLatestDraft(context.Context, int64) (domain.Draft, error)
	ClaimIngest(context.Context, int64) (domain.Draft, error)
	ReleaseIngest(context.Context, domain.Draft) error
	FinalizeText(context.Context, domain.Draft, string) (service.CreatedWhisper, error)
	FinalizeMedia(context.Context, domain.Draft, domain.MediaReference, []byte, string) (service.CreatedWhisper, error)
	ClaimPublication(context.Context, uuid.UUID) (service.Publication, error)
	ClaimNextPublication(context.Context) (service.Publication, error)
	CompletePublication(context.Context, service.Publication, int64) error
	FailPublication(context.Context, service.Publication, string, time.Duration) error
	ReserveOpen(context.Context, string, int64, string) (service.OpenDelivery, error)
	CompleteOpen(context.Context, service.OpenDelivery, int64) error
	FailOpen(context.Context, service.OpenDelivery, string) error
	WhisperMediaFallback(context.Context, uuid.UUID) ([]byte, domain.MediaType, string, error)
	HasActiveDraft(context.Context, int64) (bool, error)
	IsOwner(int64) bool
	OwnerList(context.Context, int64, int, int) ([]domain.Whisper, error)
	OwnerReview(context.Context, int64, uuid.UUID) (service.OwnerReview, error)
	OwnerDelete(context.Context, int64, uuid.UUID) error
	OwnerSetRetention(context.Context, int64, uuid.UUID, time.Duration) error
	GetEphemeralDeleteAfter() time.Duration
	SetEphemeralDeleteAfter(time.Duration)
}

type guestUseCases interface {
	CreateGuestRequest(context.Context, service.CreateGuestRequestParams) (service.GuestSession, error)
	CreateGuestInlineSecret(context.Context, service.CreateGuestInlineParams) (service.GuestSession, error)
	MarkGuestEnvelope(context.Context, string, string) error
	CancelGuestRequest(context.Context, int64) (int, error)
	BeginGuestSession(context.Context, string, domain.User) (service.GuestSession, error)
	ClaimGuestIngestForSender(context.Context, int64) (service.GuestIngestClaim, error)
	ReleaseGuestIngest(context.Context, service.GuestIngestClaim) error
	FinalizeGuestText(context.Context, service.GuestIngestClaim, string) (repository.GuestRequest, error)
	FinalizeGuestMedia(context.Context, service.GuestIngestClaim, domain.MediaReference, []byte, string) (repository.GuestRequest, error)
	ReserveGuestOpen(context.Context, string, domain.User) (service.GuestDelivery, error)
	CompleteGuestOpen(context.Context, service.GuestDelivery, int64) error
	FailGuestOpen(context.Context, service.GuestDelivery) error
	GuestMediaFallback(context.Context, uuid.UUID) ([]byte, domain.MediaType, string, error)
	GetRecentTargets(context.Context, int64, int) ([]domain.RecentTarget, error)
}

type telegramAPI interface {
	SendMessage(context.Context, telegram.SendMessageRequest) (telegram.Message, error)
	AnswerCallbackQuery(context.Context, telegram.AnswerCallbackQueryRequest) error
	AnswerGuestQuery(context.Context, telegram.AnswerGuestQueryRequest) (telegram.SentGuestMessage, error)
	AnswerInlineQuery(context.Context, telegram.AnswerInlineQueryRequest) error
	GetFile(context.Context, telegram.GetFileRequest) (telegram.File, error)
	DownloadFile(context.Context, string, int64) ([]byte, error)
	SendEphemeralText(context.Context, telegram.SendEphemeralTextRequest) (int64, error)
	SendEphemeralMedia(context.Context, telegram.SendEphemeralMediaRequest) (int64, error)
	SendEphemeralMediaUpload(context.Context, telegram.SendEphemeralMediaUploadRequest) (int64, error)
	SendPrivateMediaByFileID(context.Context, telegram.SendPrivateMediaByFileIDRequest) (telegram.Message, error)
	SendPrivateMedia(context.Context, telegram.SendPrivateMediaRequest) (telegram.Message, error)
	DeleteMessage(context.Context, telegram.DeleteMessageRequest) error
}

type BuildInfo struct {
	Version       string
	Commit        string
	CommitMessage string
	BuildTime     string
	CIRunNumber   string
}

type Config struct {
	Service              *service.Service
	Telegram             *telegram.Client
	BotUsername          string
	MaxMediaBytes        int64
	MediaDownloadTimeout time.Duration
	RequestTimeout       time.Duration
	Logger               *slog.Logger
	BuildInfo            BuildInfo
}

type Handler struct {
	service              useCases
	guest                guestUseCases
	telegram             telegramAPI
	botUsername          string
	maxMediaBytes        int64
	mediaDownloadTimeout time.Duration
	requestTimeout       time.Duration
	logger               *slog.Logger
	buildInfo            BuildInfo
}

func New(cfg Config) (*Handler, error) {
	if cfg.Service == nil || cfg.Telegram == nil || strings.TrimSpace(cfg.BotUsername) == "" ||
		cfg.MaxMediaBytes <= 0 || cfg.MediaDownloadTimeout <= 0 {
		return nil, errors.New("bot handler requires service, Telegram client, username, and positive media limits")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	requestTimeout := cfg.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = 15 * time.Second
	}
	return &Handler{
		service: cfg.Service, guest: cfg.Service, telegram: cfg.Telegram,
		botUsername:   strings.TrimPrefix(strings.TrimSpace(cfg.BotUsername), "@"),
		maxMediaBytes: cfg.MaxMediaBytes, mediaDownloadTimeout: cfg.MediaDownloadTimeout,
		requestTimeout: requestTimeout, logger: logger,
		buildInfo: cfg.BuildInfo,
	}, nil
}

func (h *Handler) HandleUpdate(ctx context.Context, update telegram.Update) error {
	if update.GuestMessage != nil {
		return h.handleGuestMessage(ctx, *update.GuestMessage)
	}
	if update.InlineQuery != nil {
		return h.handleInlineQuery(ctx, *update.InlineQuery)
	}
	if update.CallbackQuery != nil {
		return h.handleCallback(ctx, *update.CallbackQuery)
	}
	if update.Message != nil {
		return h.handleMessage(ctx, *update.Message)
	}
	return nil
}

func (h *Handler) handleMessage(ctx context.Context, message telegram.Message) error {
	if message.From == nil || message.From.ID <= 0 {
		return nil
	}
	sender := domainUser(*message.From)
	chat := domainChat(message.Chat)
	switch chat.Type {
	case domain.ChatTypeGroup, domain.ChatTypeSupergroup:
		if err := h.service.ObserveMembership(ctx, sender, chat); err != nil {
			if errors.Is(err, service.ErrChatNotAllowed) {
				return nil
			}
			return err
		}
		return h.handleGroupMessage(ctx, message, sender, chat)
	case domain.ChatTypePrivate:
		if _, err := h.service.RegisterPrivateUser(ctx, sender); err != nil {
			return err
		}
		return h.handlePrivateMessage(ctx, message, sender)
	default:
		return nil
	}
}

func domainUser(user telegram.User) domain.User {
	return domain.User{
		TelegramUserID: user.ID, Username: user.Username, FirstName: user.FirstName,
		LastName: user.LastName, LanguageCode: user.LanguageCode, IsBot: user.IsBot,
	}
}

func domainChat(chat telegram.Chat) domain.Chat {
	return domain.Chat{
		TelegramChatID: chat.ID, Type: domain.ChatType(chat.Type),
		Title: chat.Title, Username: chat.Username,
	}
}

func optionalMessageID(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	copy := value
	return &copy
}

func (h *Handler) NotifyExpiredDraft(ctx context.Context, senderID int64) error {
	if senderID <= 0 {
		return nil
	}
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.requestTimeout)
	defer cancel()
	_, err := h.telegram.SendMessage(sendCtx, telegram.SendMessageRequest{
		ChatID: senderID,
		Text:   "Your whisper draft has expired because no secret was sent within the time limit.",
	})
	return err
}

func (h *Handler) NotifyExpiredGuestRequest(ctx context.Context, senderID int64) error {
	if senderID <= 0 {
		return nil
	}
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.requestTimeout)
	defer cancel()
	_, err := h.telegram.SendMessage(sendCtx, telegram.SendMessageRequest{
		ChatID: senderID,
		Text:   "Your locked secret request has expired because no secret was provided in time.",
	})
	return err
}
