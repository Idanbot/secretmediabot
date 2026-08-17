package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/telegram"
)

type CleanupStore interface {
	RunCleanup(context.Context, repository.CleanupParams) (repository.CleanupResult, error)
}

type CleanupWorker struct {
	store                    CleanupStore
	interval                 time.Duration
	batchSize                int
	processedUpdateRetention time.Duration
	logger                   *slog.Logger
	now                      func() time.Time
}

func NewCleanupWorker(
	store CleanupStore,
	interval time.Duration,
	batchSize int,
	processedUpdateRetention time.Duration,
	logger *slog.Logger,
) *CleanupWorker {
	return &CleanupWorker{
		store: store, interval: interval, batchSize: batchSize,
		processedUpdateRetention: processedUpdateRetention, logger: logger,
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (w *CleanupWorker) Run(ctx context.Context) error {
	if w.logger == nil {
		w.logger = slog.Default()
	}
	if err := w.runOnce(ctx); err != nil && ctx.Err() == nil {
		w.logger.ErrorContext(ctx, "initial cleanup failed")
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.runOnce(ctx); err != nil && ctx.Err() == nil {
				w.logger.ErrorContext(ctx, "cleanup failed")
			}
		}
	}
}

func (w *CleanupWorker) runOnce(ctx context.Context) error {
	now := w.now()
	result, err := w.store.RunCleanup(ctx, repository.CleanupParams{
		Now: now, ProcessedUpdatesBefore: now.Add(-w.processedUpdateRetention), BatchSize: w.batchSize,
	})
	if err != nil {
		return err
	}
	if total := cleanupTotal(result); total > 0 {
		w.logger.InfoContext(ctx, "cleanup completed", "affected_rows", total)
	}
	return nil
}

func cleanupTotal(result repository.CleanupResult) int64 {
	return result.ExpiredDrafts + result.ReleasedDraftIngests + result.ExpiredWhispers +
		result.ReleasedOpenLeases + result.ReleasedPublishLeases + result.DeletedWhispers +
		result.DeletedProcessedUpdates + result.DeletedEphemeralJobs + result.DeletedGuestRequests + result.DeletedGuestJobs
}

type EphemeralDeleteStore interface {
	ClaimDueEphemeralDelete(context.Context, repository.ClaimEphemeralDeleteParams) (repository.EphemeralDeleteJob, error)
	MarkEphemeralDeleted(context.Context, repository.FinishEphemeralDeleteParams) error
	RetryEphemeralDelete(context.Context, repository.FinishEphemeralDeleteParams) error
}

type EphemeralDeleter interface {
	DeleteEphemeralMessage(context.Context, telegram.DeleteEphemeralMessageRequest) error
}

type EphemeralDeleteWorker struct {
	store    EphemeralDeleteStore
	telegram EphemeralDeleter
	interval time.Duration
	logger   *slog.Logger
	now      func() time.Time
}

type GuestPrivateDeleteStore interface {
	ClaimDueGuestDelete(context.Context, repository.ClaimGuestDeleteParams) (repository.GuestPrivateDeleteJob, error)
	MarkGuestDeleted(context.Context, repository.FinishGuestDeleteParams) error
	RetryGuestDelete(context.Context, repository.FinishGuestDeleteParams) error
}

type GuestPrivateDeleter interface {
	DeleteMessage(context.Context, telegram.DeleteMessageRequest) error
}

type GuestPrivateDeleteWorker struct {
	store    GuestPrivateDeleteStore
	telegram GuestPrivateDeleter
	interval time.Duration
	logger   *slog.Logger
	now      func() time.Time
}

func NewGuestPrivateDeleteWorker(store GuestPrivateDeleteStore, telegramClient GuestPrivateDeleter, interval time.Duration, logger *slog.Logger) *GuestPrivateDeleteWorker {
	return &GuestPrivateDeleteWorker{
		store: store, telegram: telegramClient, interval: interval, logger: logger,
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (w *GuestPrivateDeleteWorker) Run(ctx context.Context) error {
	if w.logger == nil {
		w.logger = slog.Default()
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			for range 50 {
				didWork, err := w.deleteOne(ctx)
				if err != nil && ctx.Err() == nil {
					w.logger.ErrorContext(ctx, "guest private deletion worker failed")
					break
				}
				if !didWork {
					break
				}
			}
		}
	}
}

func (w *GuestPrivateDeleteWorker) deleteOne(ctx context.Context) (bool, error) {
	now := w.now()
	job, err := w.store.ClaimDueGuestDelete(ctx, repository.ClaimGuestDeleteParams{Now: now, LeaseUntil: now.Add(20 * time.Second)})
	if errors.Is(err, repository.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	err = w.telegram.DeleteMessage(requestCtx, telegram.DeleteMessageRequest{ChatID: job.ChatID, MessageID: job.MessageID})
	cancel()
	finish := repository.FinishGuestDeleteParams{JobID: job.ID, ExpectedLeaseUntil: job.LeaseUntil, Now: w.now()}
	if err == nil || permanentDeleteError(err) {
		return true, w.store.MarkGuestDeleted(ctx, finish)
	}
	finish.NextAttemptAt = finish.Now.Add(min(time.Duration(1<<min(job.AttemptCount, 8))*time.Second, 5*time.Minute))
	finish.ErrorCode = "telegram_delete_failed"
	return true, w.store.RetryGuestDelete(ctx, finish)
}

func NewEphemeralDeleteWorker(
	store EphemeralDeleteStore,
	telegramClient EphemeralDeleter,
	interval time.Duration,
	logger *slog.Logger,
) *EphemeralDeleteWorker {
	return &EphemeralDeleteWorker{
		store: store, telegram: telegramClient, interval: interval, logger: logger,
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (w *EphemeralDeleteWorker) Run(ctx context.Context) error {
	if w.logger == nil {
		w.logger = slog.Default()
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			for range 50 {
				didWork, err := w.deleteOne(ctx)
				if err != nil && ctx.Err() == nil {
					w.logger.ErrorContext(ctx, "ephemeral deletion worker failed")
					break
				}
				if !didWork {
					break
				}
			}
		}
	}
}

func (w *EphemeralDeleteWorker) deleteOne(ctx context.Context) (bool, error) {
	now := w.now()
	leaseUntil := now.Add(20 * time.Second)
	job, err := w.store.ClaimDueEphemeralDelete(ctx, repository.ClaimEphemeralDeleteParams{
		Now: now, LeaseUntil: leaseUntil,
	})
	if errors.Is(err, repository.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	requestCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	err = w.telegram.DeleteEphemeralMessage(requestCtx, telegram.DeleteEphemeralMessageRequest{
		ChatID: job.ChatID, ReceiverUserID: job.RecipientID, EphemeralMessageID: job.EphemeralMessageID,
	})
	cancel()
	finish := repository.FinishEphemeralDeleteParams{
		JobID: job.ID, ExpectedLeaseUntil: job.LeaseUntil, Now: w.now(),
	}
	if err == nil || permanentDeleteError(err) {
		return true, w.store.MarkEphemeralDeleted(ctx, finish)
	}

	retry := min(time.Duration(1<<min(job.AttemptCount, 8))*time.Second, 5*time.Minute)
	finish.NextAttemptAt = finish.Now.Add(retry)
	finish.ErrorCode = "telegram_delete_failed"
	return true, w.store.RetryEphemeralDelete(ctx, finish)
}

func permanentDeleteError(err error) bool {
	var apiErr *telegram.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode >= 400 && apiErr.ErrorCode < 500 && apiErr.ErrorCode != 429
}
