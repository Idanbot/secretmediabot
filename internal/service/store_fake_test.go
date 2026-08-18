package service

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
)

// memoryStore is a stateful test double at the service/repository boundary. It
// deliberately implements the same chat-scoped identity rules as PostgreSQL.
type memoryStore struct {
	mu sync.Mutex

	members map[int64]map[int64]domain.User
	users   map[int64]domain.User
	drafts  map[string]domain.Draft

	activeDrafts int64
	activeErr    error
	recentCount  int64
	recentErr    error
	createErr    error
	created      []repository.CreateDraftParams

	claimedDraft domain.Draft
	claimErr     error
	releases     []repository.ReleaseDraftIngestParams
	finalizeErr  error
	finalized    []repository.FinalizeDraftParams

	publishClaim repository.PublishClaim
	publishErr   error
	marked       []repository.MarkPublishedParams
	markFailures []repository.MarkPublishFailedParams

	reservation repository.OpenReservation
	reserveErr  error
	reserved    []repository.ReserveOpenParams
	completed   []repository.CompleteOpenParams
	failed      []repository.FailOpenParams

	ownerWhispers   []domain.Whisper
	ownerWhisper    domain.Whisper
	ownerContent     repository.StoredContent
	ownerErr         error
	whisperMediaBlob repository.WhisperMediaBlob
	whisperMediaErr  error
	ownerListCalls  int
	ownerLists      []repository.OwnerListWhispersParams
	ownerGetCalls   int
	ownerReadCalls  int
	ownerDeletes    []repository.OwnerDeleteWhisperParams
	ownerRetentions []repository.OwnerUpdateRetentionParams
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		members: make(map[int64]map[int64]domain.User),
		users:   make(map[int64]domain.User),
		drafts:  make(map[string]domain.Draft),
	}
}

func (s *memoryStore) ObserveMembership(_ context.Context, params repository.ObserveMembershipParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.members[params.Chat.TelegramChatID] == nil {
		s.members[params.Chat.TelegramChatID] = make(map[int64]domain.User)
	}
	s.members[params.Chat.TelegramChatID][params.User.TelegramUserID] = params.User
	s.users[params.User.TelegramUserID] = params.User
	return nil
}

func (s *memoryStore) ObserveUser(_ context.Context, user domain.User, _ time.Time) (domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[user.TelegramUserID] = user
	return user, nil
}

func (s *memoryStore) FindObservedUserByID(_ context.Context, chatID, userID int64) (domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.members[chatID][userID]
	if !ok {
		return domain.User{}, repository.ErrNotFound
	}
	return user, nil
}

func (s *memoryStore) FindObservedUserByUsername(_ context.Context, chatID int64, username string) (domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	normalized := strings.ToLower(strings.TrimLeft(strings.TrimSpace(username), "@"))
	var found domain.User
	matches := 0
	for _, user := range s.members[chatID] {
		if !user.IsBot && user.NormalizedUsername() == normalized {
			found = user
			matches++
		}
	}
	if matches == 0 {
		return domain.User{}, repository.ErrNotFound
	}
	if matches > 1 {
		return domain.User{}, repository.ErrAmbiguousRecipient
	}
	return found, nil
}

func (s *memoryStore) CountActiveDrafts(context.Context, int64, time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeDrafts, s.activeErr
}

func (s *memoryStore) CountRecentWhispersBySender(context.Context, int64, time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recentCount, s.recentErr
}

func (s *memoryStore) CreateDraft(_ context.Context, params repository.CreateDraftParams) (domain.Draft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = append(s.created, params)
	if s.createErr != nil {
		return domain.Draft{}, s.createErr
	}
	s.drafts[string(params.ComposeTokenHash)] = params.Draft
	return params.Draft, nil
}

func (s *memoryStore) FindDraftByComposeTokenHash(_ context.Context, hash []byte) (domain.Draft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	draft, ok := s.drafts[string(hash)]
	if !ok {
		return domain.Draft{}, repository.ErrNotFound
	}
	return draft, nil
}

func (s *memoryStore) CancelLatestDraftForSender(context.Context, int64, time.Time) (domain.Draft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimErr != nil {
		return domain.Draft{}, s.claimErr
	}
	if s.claimedDraft.ID == uuid.Nil {
		return domain.Draft{}, repository.ErrNotFound
	}
	return s.claimedDraft, nil
}

func (s *memoryStore) ClaimLatestDraftIngest(context.Context, int64, time.Time, time.Time) (domain.Draft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimErr != nil {
		return domain.Draft{}, s.claimErr
	}
	if s.claimedDraft.ID == uuid.Nil {
		return domain.Draft{}, repository.ErrNotFound
	}
	return s.claimedDraft, nil
}

func (s *memoryStore) ReleaseDraftIngest(_ context.Context, params repository.ReleaseDraftIngestParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases = append(s.releases, params)
	return nil
}

func (s *memoryStore) FinalizeDraft(_ context.Context, params repository.FinalizeDraftParams) (domain.Whisper, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalized = append(s.finalized, params)
	if s.finalizeErr != nil {
		return domain.Whisper{}, s.finalizeErr
	}
	return params.Whisper, nil
}

func (s *memoryStore) ClaimPublish(context.Context, repository.ClaimPublishParams) (repository.PublishClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publishClaim, s.publishErr
}

func (s *memoryStore) ClaimNextPublish(context.Context, time.Time, time.Time) (repository.PublishClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publishClaim, s.publishErr
}

func (s *memoryStore) MarkPublished(_ context.Context, params repository.MarkPublishedParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marked = append(s.marked, params)
	return s.publishErr
}

func (s *memoryStore) MarkPublishFailed(_ context.Context, params repository.MarkPublishFailedParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markFailures = append(s.markFailures, params)
	return s.publishErr
}

func (s *memoryStore) ReserveOpen(_ context.Context, params repository.ReserveOpenParams) (repository.OpenReservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserved = append(s.reserved, params)
	return s.reservation, s.reserveErr
}

func (s *memoryStore) CompleteOpen(_ context.Context, params repository.CompleteOpenParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completed = append(s.completed, params)
	return nil
}

func (s *memoryStore) FailOpen(_ context.Context, params repository.FailOpenParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = append(s.failed, params)
	return nil
}

func (s *memoryStore) OwnerListWhispers(_ context.Context, params repository.OwnerListWhispersParams) ([]domain.Whisper, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ownerListCalls++
	s.ownerLists = append(s.ownerLists, params)
	return append([]domain.Whisper(nil), s.ownerWhispers...), s.ownerErr
}

func (s *memoryStore) OwnerGetWhisper(context.Context, repository.OwnerGetWhisperParams) (domain.Whisper, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ownerGetCalls++
	return s.ownerWhisper, s.ownerErr
}

func (s *memoryStore) OwnerFetchEncryptedContent(context.Context, repository.OwnerGetWhisperParams) (repository.StoredContent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ownerReadCalls++
	return s.ownerContent, s.ownerErr
}

func (s *memoryStore) OwnerDeleteWhisper(_ context.Context, params repository.OwnerDeleteWhisperParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ownerDeletes = append(s.ownerDeletes, params)
	return s.ownerErr
}

func (s *memoryStore) OwnerUpdateRetention(_ context.Context, params repository.OwnerUpdateRetentionParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ownerRetentions = append(s.ownerRetentions, params)
	return s.ownerErr
}

func (s *memoryStore) FetchWhisperMedia(context.Context, uuid.UUID) (repository.WhisperMediaBlob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.whisperMediaBlob, s.whisperMediaErr
}

func (s *memoryStore) addMember(chatID int64, user domain.User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.members[chatID] == nil {
		s.members[chatID] = make(map[int64]domain.User)
	}
	s.members[chatID][user.TelegramUserID] = user
	s.users[user.TelegramUserID] = user
}

func (s *memoryStore) lastFinalization() (repository.FinalizeDraftParams, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.finalized) == 0 {
		return repository.FinalizeDraftParams{}, false
	}
	return s.finalized[len(s.finalized)-1], true
}

func (s *memoryStore) createdCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.created)
}

func (s *memoryStore) reservedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reserved)
}

var _ Store = (*memoryStore)(nil)
