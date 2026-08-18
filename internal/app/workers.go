package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/idan/secretmediabot/internal/metrics"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/telegram"
)

// Shared delete-job worker constants.
const (
	deleteJobDrainLimit   = 50
	deleteJobLease        = 20 * time.Second
	deleteJobRequestWait  = 8 * time.Second
	deleteJobMaxBackoff   = 5 * time.Minute
	deleteJobMaxAttempts  = 30
	deleteFinishWriteWait = 3 * time.Second
)

var deleteJobOutcomes = metrics.Counter(
	"ephemeral_delete_jobs_total", "Deletion job terminations by kind and outcome.", "kind", "outcome")

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
) (*CleanupWorker, error) {
	if store == nil || interval <= 0 || batchSize <= 0 || processedUpdateRetention <= 0 {
		return nil, errors.New("cleanup worker requires a store and positive interval, batch size, and retention")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &CleanupWorker{
		store: store, interval: interval, batchSize: batchSize,
		processedUpdateRetention: processedUpdateRetention, logger: logger,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (w *CleanupWorker) Run(ctx context.Context) error {
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

func NewEphemeralDeleteWorker(
	store EphemeralDeleteStore,
	telegramClient EphemeralDeleter,
	interval time.Duration,
	logger *slog.Logger,
) (*EphemeralDeleteWorker, error) {
	if store == nil || telegramClient == nil || interval <= 0 {
		return nil, errors.New("ephemeral delete worker requires a store, deleter, and positive interval")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &EphemeralDeleteWorker{
		store: store, telegram: telegramClient, interval: interval, logger: logger,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (w *EphemeralDeleteWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			drainDeleteJobs(ctx, w.logger, "ephemeral", &ephemeralJobQueue{store: w.store, deleter: w.telegram}, w.now)
		}
	}
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

func NewGuestPrivateDeleteWorker(store GuestPrivateDeleteStore, telegramClient GuestPrivateDeleter, interval time.Duration, logger *slog.Logger) (*GuestPrivateDeleteWorker, error) {
	if store == nil || telegramClient == nil || interval <= 0 {
		return nil, errors.New("guest delete worker requires a store, deleter, and positive interval")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &GuestPrivateDeleteWorker{
		store: store, telegram: telegramClient, interval: interval, logger: logger,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (w *GuestPrivateDeleteWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			drainDeleteJobs(ctx, w.logger, "guest_private", &guestJobQueue{store: w.store, deleter: w.telegram}, w.now)
		}
	}
}

// deleteJob is the shared shape of both durable deletion queues.
type deleteJob struct {
	id           int64
	chatID       int64
	messageID    int64
	receiverID   int64
	attemptCount int
	leaseUntil   time.Time
}

// deleteJobQueue abstracts one durable deletion queue: claim a due job,
// attempt the Telegram deletion, and record the outcome.
type deleteJobQueue interface {
	claim(context.Context, time.Time, time.Time) (deleteJob, error)
	delete(context.Context, deleteJob) error
	markDeleted(context.Context, deleteJob, time.Time, string) error
	retry(context.Context, deleteJob, time.Time, time.Time, string) error
}

type ephemeralJobQueue struct {
	store   EphemeralDeleteStore
	deleter EphemeralDeleter
}

func (q *ephemeralJobQueue) claim(ctx context.Context, now, leaseUntil time.Time) (deleteJob, error) {
	job, err := q.store.ClaimDueEphemeralDelete(ctx, repository.ClaimEphemeralDeleteParams{Now: now, LeaseUntil: leaseUntil})
	if err != nil {
		return deleteJob{}, err
	}
	return deleteJob{
		id: job.ID, chatID: job.ChatID, receiverID: job.RecipientID,
		messageID: job.EphemeralMessageID, attemptCount: job.AttemptCount, leaseUntil: job.LeaseUntil,
	}, nil
}

func (q *ephemeralJobQueue) delete(ctx context.Context, job deleteJob) error {
	return q.deleter.DeleteEphemeralMessage(ctx, telegram.DeleteEphemeralMessageRequest{
		ChatID: job.chatID, ReceiverUserID: job.receiverID, EphemeralMessageID: job.messageID,
	})
}

func (q *ephemeralJobQueue) markDeleted(ctx context.Context, job deleteJob, now time.Time, code string) error {
	return q.store.MarkEphemeralDeleted(ctx, repository.FinishEphemeralDeleteParams{
		JobID: job.id, ExpectedLeaseUntil: job.leaseUntil, Now: now, ErrorCode: code,
	})
}

func (q *ephemeralJobQueue) retry(ctx context.Context, job deleteJob, now, next time.Time, code string) error {
	return q.store.RetryEphemeralDelete(ctx, repository.FinishEphemeralDeleteParams{
		JobID: job.id, ExpectedLeaseUntil: job.leaseUntil, Now: now, NextAttemptAt: next, ErrorCode: code,
	})
}

type guestJobQueue struct {
	store   GuestPrivateDeleteStore
	deleter GuestPrivateDeleter
}

func (q *guestJobQueue) claim(ctx context.Context, now, leaseUntil time.Time) (deleteJob, error) {
	job, err := q.store.ClaimDueGuestDelete(ctx, repository.ClaimGuestDeleteParams{Now: now, LeaseUntil: leaseUntil})
	if err != nil {
		return deleteJob{}, err
	}
	return deleteJob{
		id: job.ID, chatID: job.ChatID, messageID: job.MessageID,
		attemptCount: job.AttemptCount, leaseUntil: job.LeaseUntil,
	}, nil
}

func (q *guestJobQueue) delete(ctx context.Context, job deleteJob) error {
	return q.deleter.DeleteMessage(ctx, telegram.DeleteMessageRequest{
		ChatID: job.chatID, MessageID: job.messageID,
	})
}

func (q *guestJobQueue) markDeleted(ctx context.Context, job deleteJob, now time.Time, code string) error {
	return q.store.MarkGuestDeleted(ctx, repository.FinishGuestDeleteParams{
		JobID: job.id, ExpectedLeaseUntil: job.leaseUntil, Now: now, ErrorCode: code,
	})
}

func (q *guestJobQueue) retry(ctx context.Context, job deleteJob, now, next time.Time, code string) error {
	return q.store.RetryGuestDelete(ctx, repository.FinishGuestDeleteParams{
		JobID: job.id, ExpectedLeaseUntil: job.leaseUntil, Now: now, NextAttemptAt: next, ErrorCode: code,
	})
}

// drainDeleteJobs claims and executes deletion jobs until the queue is empty
// or the bounded drain budget is spent.
func drainDeleteJobs(ctx context.Context, logger *slog.Logger, kind string, queue deleteJobQueue, now func() time.Time) {
	for range deleteJobDrainLimit {
		if ctx.Err() != nil {
			return
		}
		current := now()
		job, err := queue.claim(ctx, current, current.Add(deleteJobLease))
		if errors.Is(err, repository.ErrNotFound) {
			return
		}
		if err != nil {
			if ctx.Err() == nil {
				logger.ErrorContext(ctx, "deletion worker failed to claim a job", "kind", kind)
			}
			return
		}
		if err := executeDeleteJob(ctx, kind, queue, job, now); err != nil && ctx.Err() == nil {
			logger.ErrorContext(ctx, "deletion worker failed", "kind", kind)
		}
	}
}

func executeDeleteJob(ctx context.Context, kind string, queue deleteJobQueue, job deleteJob, now func() time.Time) error {
	requestCtx, cancel := context.WithTimeout(ctx, deleteJobRequestWait)
	err := queue.delete(requestCtx, job)
	cancel()

	// Finish writes deliberately use a detached context: a Telegram deletion
	// that already succeeded must be recorded even if shutdown started.
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), deleteFinishWriteWait)
	defer finishCancel()
	finishNow := now()

	switch {
	case err == nil:
		deleteJobOutcomes.Inc(kind, "deleted")
		return queue.markDeleted(finishCtx, job, finishNow, "")
	case permanentDeleteClose(err):
		// The message is gone, the bot lost access, or Telegram permanently
		// refuses the call: stop retrying and record why the job closed.
		deleteJobOutcomes.Inc(kind, "permanent")
		return queue.markDeleted(finishCtx, job, finishNow, "telegram_permanent")
	case job.attemptCount >= deleteJobMaxAttempts:
		// Bounded retry: an unremovable message must not occupy the queue (and
		// the table) forever.
		deleteJobOutcomes.Inc(kind, "gave_up")
		return queue.markDeleted(finishCtx, job, finishNow, "gave_up")
	default:
		backoff := min(time.Duration(1<<min(job.attemptCount, 8))*time.Second, deleteJobMaxBackoff)
		var apiErr *telegram.APIError
		if errors.As(err, &apiErr) {
			if requested := apiErr.RetryAfter(); requested > backoff {
				backoff = requested
			}
		}
		deleteJobOutcomes.Inc(kind, "retry")
		return queue.retry(finishCtx, job, finishNow, finishNow.Add(backoff), "telegram_delete_failed")
	}
}

// permanentDeleteClose narrows "stop retrying" to the Telegram responses that
// genuinely mean the target message can never be deleted this way. A generic
// 400 may be our own request shape changing and is retried until the attempt
// cap closes the job as gave_up.
func permanentDeleteClose(err error) bool {
	apiErr := (*telegram.APIError)(nil)
	if !errors.As(err, &apiErr) || apiErr.RateLimited() {
		return false
	}
	if apiErr.ErrorCode == 403 || apiErr.ErrorCode == 404 || apiErr.StatusCode == 403 || apiErr.StatusCode == 404 {
		return true
	}
	description := strings.ToLower(apiErr.Description)
	return strings.Contains(description, "message to delete not found")
}
