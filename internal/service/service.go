package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/secretcrypto"
	"github.com/idan/secretmediabot/internal/token"
)

const (
	MaxSecretTextRunes = 4096
	MaxCaptionRunes    = 1024
	ComposePrefix      = "compose_"

	recentTargetsCacheTTL        = 10 * time.Minute
	recentTargetsCacheMaxSenders = 1024
	recentTargetsPerSender       = 5
)

type recentTargetsCacheEntry struct {
	targets  []domain.RecentTarget
	storedAt time.Time
}

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
	OwnerListWhisperDetails(context.Context, repository.OwnerListWhispersParams) ([]domain.OwnerWhisper, error)
	OwnerGetWhisper(context.Context, repository.OwnerGetWhisperParams) (domain.Whisper, error)
	OwnerFetchEncryptedContent(context.Context, repository.OwnerGetWhisperParams) (repository.StoredContent, error)
	OwnerDeleteWhisper(context.Context, repository.OwnerDeleteWhisperParams) error
	OwnerUpdateRetention(context.Context, repository.OwnerUpdateRetentionParams) error
	FetchWhisperMedia(context.Context, uuid.UUID) (repository.WhisperMediaBlob, error)
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
	// MaxActiveGuestRequestsPerUser and MaxGuestRequestsPerUserPerHour bound
	// guest/inline request creation; both must be positive.
	MaxActiveGuestRequestsPerUser  int
	MaxGuestRequestsPerUserPerHour int
	DefaultOneTime                 bool
	ProtectContent                 bool
	AllowedChatIDs                 []int64
	OwnerIDs                       []int64
	// GuestModeEnabled toggles guest mentions and inline locked envelopes.
	// When false, guest request creation fails closed.
	GuestModeEnabled bool
}

type Service struct {
	store              Store
	guestStore         GuestStore
	cipher             *secretcrypto.Keyring
	options            Options
	now                func() time.Time
	allowed            map[int64]struct{}
	owners             map[int64]struct{}
	mu                 sync.RWMutex
	recentTargetsCache map[int64]recentTargetsCacheEntry
}

func New(store Store, cipher *secretcrypto.Keyring, options Options) (*Service, error) {
	if store == nil || cipher == nil {
		return nil, errors.New("service store and cipher are required")
	}
	if options.DraftTTL <= 0 || options.WhisperTTL <= 0 || options.ContentRetention <= 0 ||
		options.IngestLease <= 0 || options.OpenLease <= 0 || options.PublishLease <= 0 ||
		options.EphemeralDeleteAfter < 0 || options.MaxMediaBytes <= 0 ||
		options.MaxActiveDraftsPerUser <= 0 || options.MaxWhispersPerUserPerHour <= 0 ||
		options.MaxActiveGuestRequestsPerUser <= 0 || options.MaxGuestRequestsPerUserPerHour <= 0 {
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
		recentTargetsCache: make(map[int64]recentTargetsCacheEntry),
	}, nil
}

func (s *Service) GetEphemeralDeleteAfter() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.options.EphemeralDeleteAfter
}

func (s *Service) SetEphemeralDeleteAfter(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d < 0 {
		d = 0
	}
	s.options.EphemeralDeleteAfter = d
}

func (s *Service) GetRecentTargets(ctx context.Context, senderID int64, limit int) ([]domain.RecentTarget, error) {
	if limit <= 0 {
		limit = 3
	} else if limit > recentTargetsPerSender {
		limit = recentTargetsPerSender
	}
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now()
	}
	s.mu.RLock()
	entry, ok := s.recentTargetsCache[senderID]
	if ok && now.Sub(entry.storedAt) <= recentTargetsCacheTTL {
		cached := trimRecentTargets(cloneRecentTargets(entry.targets), limit)
		s.mu.RUnlock()
		return cached, nil
	}
	s.mu.RUnlock()

	if s.guestStore != nil {
		targets, err := s.guestStore.FindRecentTargetsForSender(ctx, senderID, limit)
		if err == nil && len(targets) > 0 {
			s.mu.Lock()
			s.storeRecentTargetsLocked(senderID, targets, now)
			s.mu.Unlock()
			return trimRecentTargets(cloneRecentTargets(targets), limit), nil
		}
	}
	return nil, nil
}

func (s *Service) RecordRecentTarget(senderID int64, target domain.RecentTarget) {
	if senderID <= 0 || (target.TargetUserID <= 0 && target.TargetUsername == "") {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recentTargetsCache == nil {
		s.recentTargetsCache = make(map[int64]recentTargetsCacheEntry)
	}
	current := s.recentTargetsCache[senderID].targets
	updated := make([]domain.RecentTarget, 0, recentTargetsPerSender)
	updated = append(updated, target)
	for _, item := range current {
		if (target.TargetUserID > 0 && item.TargetUserID == target.TargetUserID) ||
			(target.TargetUsername != "" && strings.EqualFold(item.TargetUsername, target.TargetUsername)) {
			continue
		}
		updated = append(updated, item)
		if len(updated) >= recentTargetsPerSender {
			break
		}
	}
	storedAt := target.LastUsedAt
	if storedAt.IsZero() {
		storedAt = time.Now().UTC()
		if s.now != nil {
			storedAt = s.now()
		}
	}
	s.storeRecentTargetsLocked(senderID, updated, storedAt)
}

func (s *Service) storeRecentTargetsLocked(senderID int64, targets []domain.RecentTarget, storedAt time.Time) {
	if s.recentTargetsCache == nil {
		s.recentTargetsCache = make(map[int64]recentTargetsCacheEntry)
	}
	if _, exists := s.recentTargetsCache[senderID]; !exists && len(s.recentTargetsCache) >= recentTargetsCacheMaxSenders {
		var (
			oldestID int64
			oldestAt time.Time
		)
		for candidateID, entry := range s.recentTargetsCache {
			if oldestAt.IsZero() || entry.storedAt.Before(oldestAt) {
				oldestID = candidateID
				oldestAt = entry.storedAt
			}
		}
		delete(s.recentTargetsCache, oldestID)
	}
	if len(targets) > recentTargetsPerSender {
		targets = targets[:recentTargetsPerSender]
	}
	s.recentTargetsCache[senderID] = recentTargetsCacheEntry{
		targets:  cloneRecentTargets(targets),
		storedAt: storedAt,
	}
}

func cloneRecentTargets(targets []domain.RecentTarget) []domain.RecentTarget {
	return append([]domain.RecentTarget(nil), targets...)
}

func trimRecentTargets(targets []domain.RecentTarget, limit int) []domain.RecentTarget {
	if len(targets) > limit {
		return targets[:limit]
	}
	return targets
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
