package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/telegram"
)

type cleanupStoreFunc func(context.Context, repository.CleanupParams) (repository.CleanupResult, error)

func (f cleanupStoreFunc) RunCleanup(ctx context.Context, params repository.CleanupParams) (repository.CleanupResult, error) {
	return f(ctx, params)
}

type ephemeralDeleteStoreStub struct {
	claim func(context.Context, repository.ClaimEphemeralDeleteParams) (repository.EphemeralDeleteJob, error)
}

func (s ephemeralDeleteStoreStub) ClaimDueEphemeralDelete(
	ctx context.Context,
	params repository.ClaimEphemeralDeleteParams,
) (repository.EphemeralDeleteJob, error) {
	return s.claim(ctx, params)
}

func (ephemeralDeleteStoreStub) MarkEphemeralDeleted(context.Context, repository.FinishEphemeralDeleteParams) error {
	return nil
}

func (ephemeralDeleteStoreStub) RetryEphemeralDelete(context.Context, repository.FinishEphemeralDeleteParams) error {
	return nil
}

type ephemeralDeleterFunc func(context.Context, telegram.DeleteEphemeralMessageRequest) error

func (f ephemeralDeleterFunc) DeleteEphemeralMessage(
	ctx context.Context,
	request telegram.DeleteEphemeralMessageRequest,
) error {
	return f(ctx, request)
}

func TestCleanupWorkerFailureLogIsRedacted(t *testing.T) {
	t.Parallel()

	const sensitive = "database error contains secret-media-id=987654"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0
	store := cleanupStoreFunc(func(context.Context, repository.CleanupParams) (repository.CleanupResult, error) {
		calls++
		if calls == 1 {
			return repository.CleanupResult{}, errors.New(sensitive)
		}
		cancel()
		return repository.CleanupResult{}, nil
	})
	var logs bytes.Buffer
	worker, workerErr := NewCleanupWorker(
		store,
		time.Millisecond,
		1,
		time.Hour,
		slog.New(slog.NewTextHandler(&logs, nil)),
	)
	if workerErr != nil {
		t.Fatalf("NewCleanupWorker() error = %v", workerErr)
	}

	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	output := logs.String()
	if !strings.Contains(output, "initial cleanup failed") {
		t.Fatalf("cleanup log = %q, want generic failure", output)
	}
	if strings.Contains(output, sensitive) || strings.Contains(output, "987654") {
		t.Fatalf("cleanup log leaked store error: %q", output)
	}
}

func TestEphemeralDeleteWorkerFailureLogIsRedacted(t *testing.T) {
	t.Parallel()

	const sensitive = "telegram error contains secret-chat-id=987654"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0
	store := ephemeralDeleteStoreStub{claim: func(
		context.Context,
		repository.ClaimEphemeralDeleteParams,
	) (repository.EphemeralDeleteJob, error) {
		calls++
		if calls == 1 {
			return repository.EphemeralDeleteJob{}, errors.New(sensitive)
		}
		cancel()
		return repository.EphemeralDeleteJob{}, repository.ErrNotFound
	}}
	var logs bytes.Buffer
	worker, workerErr := NewEphemeralDeleteWorker(
		store,
		ephemeralDeleterFunc(func(context.Context, telegram.DeleteEphemeralMessageRequest) error { return nil }),
		time.Millisecond,
		slog.New(slog.NewTextHandler(&logs, nil)),
	)
	if workerErr != nil {
		t.Fatalf("NewEphemeralDeleteWorker() error = %v", workerErr)
	}

	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	output := logs.String()
	if !strings.Contains(output, "deletion worker failed") {
		t.Fatalf("ephemeral deletion log = %q, want generic failure", output)
	}
	if strings.Contains(output, sensitive) || strings.Contains(output, "987654") {
		t.Fatalf("ephemeral deletion log leaked store error: %q", output)
	}
}

type fakeExpiryNotifier struct {
	expiredDrafts []int64
	expiredGuests []int64
}

func (f *fakeExpiryNotifier) NotifyExpiredDraft(_ context.Context, senderID int64) error {
	f.expiredDrafts = append(f.expiredDrafts, senderID)
	return nil
}

func (f *fakeExpiryNotifier) NotifyExpiredGuestRequest(_ context.Context, senderID int64) error {
	f.expiredGuests = append(f.expiredGuests, senderID)
	return nil
}

func TestCleanupWorkerNotifiesExpiredSenders(t *testing.T) {
	t.Parallel()

	var receivedParams repository.CleanupParams
	store := cleanupStoreFunc(func(_ context.Context, params repository.CleanupParams) (repository.CleanupResult, error) {
		receivedParams = params
		return repository.CleanupResult{
			ExpiredDrafts:         2,
			ExpiredDraftSenderIDs: []int64{101, 102},
			ExpiredGuestSenderIDs: []int64{201},
		}, nil
	})

	notifier := &fakeExpiryNotifier{}
	worker, err := NewCleanupWorkerWithOptions(store, CleanupWorkerOptions{
		Interval:                  time.Hour,
		BatchSize:                 250,
		ProcessedUpdateRetention:  24 * time.Hour,
		ObservedIdentityRetention: 30 * 24 * time.Hour,
		Notifier:                  notifier,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewCleanupWorkerWithOptions() error = %v", err)
	}

	if err := worker.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce() error = %v", err)
	}

	if receivedParams.BatchSize != 250 {
		t.Fatalf("BatchSize = %d, want 250", receivedParams.BatchSize)
	}
	if receivedParams.IdentityRetention != 30*24*time.Hour {
		t.Fatalf("IdentityRetention = %s, want 720h", receivedParams.IdentityRetention)
	}
	if len(notifier.expiredDrafts) != 2 || notifier.expiredDrafts[0] != 101 || notifier.expiredDrafts[1] != 102 {
		t.Fatalf("expiredDrafts = %v, want [101 102]", notifier.expiredDrafts)
	}
	if len(notifier.expiredGuests) != 1 || notifier.expiredGuests[0] != 201 {
		t.Fatalf("expiredGuests = %v, want [201]", notifier.expiredGuests)
	}
}

func TestCleanupWorkerOptionsValidation(t *testing.T) {
	t.Parallel()

	store := cleanupStoreFunc(func(context.Context, repository.CleanupParams) (repository.CleanupResult, error) {
		return repository.CleanupResult{}, nil
	})

	_, err := NewCleanupWorkerWithOptions(store, CleanupWorkerOptions{
		Interval:                 0,
		BatchSize:                100,
		ProcessedUpdateRetention: time.Hour,
	}, nil)
	if err == nil {
		t.Fatal("expected error for non-positive interval")
	}

	_, err = NewCleanupWorkerWithOptions(store, CleanupWorkerOptions{
		Interval:                  time.Hour,
		BatchSize:                 100,
		ProcessedUpdateRetention:  time.Hour,
		ObservedIdentityRetention: -time.Hour,
	}, nil)
	if err == nil {
		t.Fatal("expected error for negative identity retention")
	}
}
