package service

import (
	"context"
	"errors"
	"time"

	"github.com/idan/secretmediabot/internal/command"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/token"
)

type CreateDraftRequest struct {
	Sender                 domain.User
	Chat                   domain.Chat
	ReplyRecipient         *domain.User
	DirectTarget           *command.Target
	SourceThreadID         *int64
	SourceReplyMessageID   *int64
	SourceCommandMessageID *int64
}

type CreateDraftResult struct {
	Draft            domain.Draft
	Recipient        domain.User
	ComposeParameter string
}

// ObserveMembership records a Telegram identity only in a configured group or
// supergroup. Usernames remain mutable lookup hints; callers must continue to
// authorize with the numeric Telegram user ID.
func (s *Service) ObserveMembership(ctx context.Context, user domain.User, chat domain.Chat) error {
	if user.TelegramUserID <= 0 || chat.TelegramChatID == 0 || !chat.Type.SupportsWhispers() {
		return ErrInvalidMembership
	}
	if !s.chatAllowed(chat.TelegramChatID) {
		return ErrChatNotAllowed
	}
	return s.store.ObserveMembership(ctx, repository.ObserveMembershipParams{
		User: user, Chat: chat, SeenAt: s.now(),
	})
}

func (s *Service) RegisterPrivateUser(ctx context.Context, user domain.User) (domain.User, error) {
	if user.TelegramUserID <= 0 || user.IsBot {
		return domain.User{}, ErrTargetRequired
	}
	user.HasStartedPrivateChat = true
	return s.store.ObserveUser(ctx, user, s.now())
}

func (s *Service) CreateDraft(ctx context.Context, req CreateDraftRequest) (CreateDraftResult, error) {
	now := s.now()
	if req.Sender.TelegramUserID <= 0 || req.Sender.IsBot || !req.Chat.Type.SupportsWhispers() {
		return CreateDraftResult{}, ErrTargetRequired
	}
	if !s.chatAllowed(req.Chat.TelegramChatID) {
		return CreateDraftResult{}, ErrChatNotAllowed
	}
	if err := s.ObserveMembership(ctx, req.Sender, req.Chat); err != nil {
		return CreateDraftResult{}, err
	}

	recipient, err := s.resolveRecipient(ctx, req, now)
	if err != nil {
		return CreateDraftResult{}, err
	}
	if recipient.IsBot {
		return CreateDraftResult{}, ErrTargetIsBot
	}
	if recipient.TelegramUserID == req.Sender.TelegramUserID {
		return CreateDraftResult{}, ErrTargetIsSender
	}

	active, err := s.store.CountActiveDrafts(ctx, req.Sender.TelegramUserID, now)
	if err != nil {
		return CreateDraftResult{}, err
	}
	if active >= int64(s.options.MaxActiveDraftsPerUser) {
		return CreateDraftResult{}, ErrTooManyDrafts
	}
	recent, err := s.store.CountRecentWhispersBySender(ctx, req.Sender.TelegramUserID, now.Add(-time.Hour))
	if err != nil {
		return CreateDraftResult{}, err
	}
	if recent >= int64(s.options.MaxWhispersPerUserPerHour) {
		return CreateDraftResult{}, ErrRateLimited
	}

	compose, err := token.Generate()
	if err != nil {
		return CreateDraftResult{}, err
	}
	draft, err := domain.NewDraft(domain.NewDraftParams{
		ComposeTokenHash:       compose.Hash[:],
		SenderID:               req.Sender.TelegramUserID,
		RecipientID:            recipient.TelegramUserID,
		SourceChatID:           req.Chat.TelegramChatID,
		SourceThreadID:         req.SourceThreadID,
		SourceReplyMessageID:   req.SourceReplyMessageID,
		SourceCommandMessageID: req.SourceCommandMessageID,
		CreatedAt:              now,
		ExpiresAt:              now.Add(s.options.DraftTTL),
	})
	if err != nil {
		return CreateDraftResult{}, err
	}
	draft, err = s.store.CreateDraft(ctx, repository.CreateDraftParams{
		Draft: draft, ComposeTokenHash: compose.Hash[:], SourceCommandMessageID: req.SourceCommandMessageID,
		Now: now, MaxActiveDrafts: s.options.MaxActiveDraftsPerUser,
		RecentWhispersSince: now.Add(-time.Hour), MaxRecentWhispers: s.options.MaxWhispersPerUserPerHour,
	})
	if err != nil {
		if errors.Is(err, repository.ErrTooManyActiveDrafts) {
			return CreateDraftResult{}, ErrTooManyDrafts
		}
		if errors.Is(err, repository.ErrWhisperRateLimit) {
			return CreateDraftResult{}, ErrRateLimited
		}
		return CreateDraftResult{}, err
	}
	return CreateDraftResult{
		Draft: draft, Recipient: recipient, ComposeParameter: ComposePrefix + compose.Raw,
	}, nil
}

func (s *Service) resolveRecipient(ctx context.Context, req CreateDraftRequest, now time.Time) (domain.User, error) {
	if req.ReplyRecipient != nil && req.DirectTarget != nil {
		return domain.User{}, ErrTargetRequired
	}
	if req.ReplyRecipient != nil {
		if err := s.store.ObserveMembership(ctx, repository.ObserveMembershipParams{
			User: *req.ReplyRecipient, Chat: req.Chat, SeenAt: now,
		}); err != nil {
			return domain.User{}, err
		}
		return *req.ReplyRecipient, nil
	}
	if req.DirectTarget == nil {
		return domain.User{}, ErrTargetRequired
	}

	var (
		recipient domain.User
		err       error
	)
	switch req.DirectTarget.Kind {
	case command.TargetUserID:
		recipient, err = s.store.FindObservedUserByID(ctx, req.Chat.TelegramChatID, req.DirectTarget.UserID)
	case command.TargetUsername:
		recipient, err = s.store.FindObservedUserByUsername(ctx, req.Chat.TelegramChatID, req.DirectTarget.Username)
	default:
		return domain.User{}, ErrTargetRequired
	}
	if errors.Is(err, repository.ErrNotFound) {
		return domain.User{}, ErrTargetNotObserved
	}
	if errors.Is(err, repository.ErrAmbiguousRecipient) {
		return domain.User{}, ErrAmbiguousTarget
	}
	return recipient, err
}

type ResumeDraftResult struct {
	Draft     domain.Draft
	Recipient domain.User
}

func (s *Service) ResumeDraft(ctx context.Context, sender domain.User, composeParameter string) (ResumeDraftResult, error) {
	if _, err := s.RegisterPrivateUser(ctx, sender); err != nil {
		return ResumeDraftResult{}, err
	}
	hash, err := validateComposeParameter(composeParameter)
	if err != nil {
		return ResumeDraftResult{}, err
	}
	draft, err := s.store.FindDraftByComposeTokenHash(ctx, hash)
	if err != nil {
		return ResumeDraftResult{}, mapRepositoryError(err)
	}
	if draft.SenderID != sender.TelegramUserID {
		return ResumeDraftResult{}, ErrDraftNotFound
	}
	if !draft.IsActive(s.now()) {
		return ResumeDraftResult{}, ErrDraftExpired
	}
	recipient, err := s.store.FindObservedUserByID(ctx, draft.SourceChatID, draft.RecipientID)
	if err != nil {
		return ResumeDraftResult{}, err
	}
	return ResumeDraftResult{Draft: draft, Recipient: recipient}, nil
}

func (s *Service) CancelLatestDraft(ctx context.Context, senderID int64) (domain.Draft, error) {
	draft, err := s.store.CancelLatestDraftForSender(ctx, senderID, s.now())
	if errors.Is(err, repository.ErrNotFound) {
		return domain.Draft{}, ErrDraftNotFound
	}
	return draft, err
}
