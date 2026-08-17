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
	worker := NewCleanupWorker(
		store,
		time.Millisecond,
		1,
		time.Hour,
		slog.New(slog.NewTextHandler(&logs, nil)),
	)

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
	worker := NewEphemeralDeleteWorker(
		store,
		ephemeralDeleterFunc(func(context.Context, telegram.DeleteEphemeralMessageRequest) error { return nil }),
		time.Millisecond,
		slog.New(slog.NewTextHandler(&logs, nil)),
	)

	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	output := logs.String()
	if !strings.Contains(output, "ephemeral deletion worker failed") {
		t.Fatalf("ephemeral deletion log = %q, want generic failure", output)
	}
	if strings.Contains(output, sensitive) || strings.Contains(output, "987654") {
		t.Fatalf("ephemeral deletion log leaked store error: %q", output)
	}
}
