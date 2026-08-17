package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/secretcrypto"
	"github.com/idan/secretmediabot/internal/token"
)

const (
	MaxSecretTextRunes = 4096
	MaxCaptionRunes    = 1024
	ComposePrefix      = "compose_"
)

type Store interface {
	ObserveMembership(context.Context, repository.ObserveMembershipParams) error
	ObserveUser(context.Context, domain.User, time.Time) (domain.User, error)
	FindObservedUserByID(context.Context, int64, int64) (domain.User, error)
	FindObservedUserByUsername(context.Context, int64, string) (domain.User, error)
	CountActiveDrafts(context.Context, int64, time.Time) (int64, error)
	CountRecentWhispersBySender(context.Context, int64, time.Time) (int64, error)
	CreateDraft(context.Context, repository.CreateDraftParams) (domain.Draft, error)
	FindDraftByComposeTokenHash(context.Context, []byte) (domain.Draft, error)
	CancelLatestDraftForSender(context.Context, int64, time.Time) (domain.Draft, error)
	ClaimLatestDraftIngest(context.Context, int64, time.Time, time.Time) (domain.Draft, error)
	ReleaseDraftIngest(context.Context, repository.ReleaseDraftIngestParams) error
	FinalizeDraft(context.Context, repository.FinalizeDraftParams) (domain.Whisper, error)
	ClaimPublish(context.Context, repository.ClaimPublishParams) (repository.PublishClaim, error)
	ClaimNextPublish(context.Context, time.Time, time.Time) (repository.PublishClaim, error)
	MarkPublished(context.Context, repository.MarkPublishedParams) error
	MarkPublishFailed(context.Context, repository.MarkPublishFailedParams) error
	ReserveOpen(context.Context, repository.ReserveOpenParams) (repository.OpenReservation, error)
	CompleteOpen(context.Context, repository.CompleteOpenParams) error
	FailOpen(context.Context, repository.FailOpenParams) error
	OwnerListWhispers(context.Context, repository.OwnerListWhispersParams) ([]domain.Whisper, error)
	OwnerGetWhisper(context.Context, repository.OwnerGetWhisperParams) (domain.Whisper, error)
	OwnerFetchEncryptedContent(context.Context, repository.OwnerGetWhisperParams) (repository.StoredContent, error)
	OwnerDeleteWhisper(context.Context, repository.OwnerDeleteWhisperParams) error
	OwnerUpdateRetention(context.Context, repository.OwnerUpdateRetentionParams) error
}

type Options struct {
	DraftTTL                  time.Duration
	WhisperTTL                time.Duration
	ContentRetention          time.Duration
	IngestLease               time.Duration
	OpenLease                 time.Duration
	PublishLease              time.Duration
	EphemeralDeleteAfter      time.Duration
	MaxMediaBytes             int64
	MaxActiveDraftsPerUser    int
	MaxWhispersPerUserPerHour int
	DefaultOneTime            bool
	ProtectContent            bool
	AllowedChatIDs            []int64
	OwnerIDs                  []int64
}

type Service struct {
	store      Store
	guestStore GuestStore
	cipher     *secretcrypto.Keyring
	options    Options
	now        func() time.Time
	allowed    map[int64]struct{}
	owners     map[int64]struct{}
}

func New(store Store, cipher *secretcrypto.Keyring, options Options) (*Service, error) {
	if store == nil || cipher == nil {
		return nil, errors.New("service store and cipher are required")
	}
	if options.DraftTTL <= 0 || options.WhisperTTL <= 0 || options.ContentRetention <= 0 ||
		options.IngestLease <= 0 || options.OpenLease <= 0 || options.PublishLease <= 0 ||
		options.EphemeralDeleteAfter <= 0 || options.MaxMediaBytes <= 0 ||
		options.MaxActiveDraftsPerUser <= 0 || options.MaxWhispersPerUserPerHour <= 0 {
		return nil, errors.New("service durations and limits must be positive")
	}
	allowed := make(map[int64]struct{}, len(options.AllowedChatIDs))
	for _, chatID := range options.AllowedChatIDs {
		if chatID == 0 {
			return nil, errors.New("allowed chat ID cannot be zero")
		}
		allowed[chatID] = struct{}{}
	}
	owners := make(map[int64]struct{}, len(options.OwnerIDs))
	for _, ownerID := range options.OwnerIDs {
		if ownerID <= 0 {
			return nil, errors.New("owner Telegram IDs must be positive")
		}
		owners[ownerID] = struct{}{}
	}
	if len(owners) == 0 {
		return nil, errors.New("at least one owner Telegram ID is required")
	}
	guestStore, _ := store.(GuestStore)
	return &Service{
		store: store, guestStore: guestStore, cipher: cipher, options: options,
		now: func() time.Time { return time.Now().UTC() }, allowed: allowed, owners: owners,
	}, nil
}

func (s *Service) IsOwner(telegramUserID int64) bool {
	_, ok := s.owners[telegramUserID]
	return ok
}

func (s *Service) chatAllowed(chatID int64) bool {
	if len(s.allowed) == 0 {
		return true
	}
	_, ok := s.allowed[chatID]
	return ok
}

func mapRepositoryError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, repository.ErrNotFound):
		return ErrDraftNotFound
	case errors.Is(err, repository.ErrAmbiguousRecipient):
		return ErrAmbiguousTarget
	default:
		return err
	}
}

func validateComposeParameter(value string) ([]byte, error) {
	raw := strings.TrimPrefix(strings.TrimSpace(value), ComposePrefix)
	if raw == value || raw == "" {
		return nil, ErrInvalidOpenToken
	}
	hash, err := parseRawTokenHash(raw)
	if err != nil {
		return nil, ErrInvalidOpenToken
	}
	return hash, nil
}

func parseRawTokenHash(raw string) ([]byte, error) {
	hash, err := tokenHashFromCallback("w:" + raw)
	if err != nil {
		return nil, err
	}
	return hash, nil
}

// tokenHashFromCallback is kept in a small helper so compose and callback
// tokens use one strict parser without storing either plaintext token.
func tokenHashFromCallback(callbackData string) ([]byte, error) {
	hash, err := token.ParseAndHash(callbackData)
	if err != nil {
		return nil, fmt.Errorf("parse opaque token: %w", err)
	}
	return hash[:], nil
}
