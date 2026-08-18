package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/idan/secretmediabot/internal/metrics"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/telegram"
)

type UpdateHandler interface {
	HandleUpdate(context.Context, telegram.Update) error
}

type UpdateLeaseStore interface {
	ClaimUpdate(context.Context, repository.ClaimUpdateParams) (repository.UpdateLease, error)
	CompleteUpdate(context.Context, repository.FinishUpdateParams) error
	FailUpdate(context.Context, repository.FinishUpdateParams) error
}

// ErrUpdateBusy means another worker still owns the update lease. Transports
// must retry the update instead of acknowledging it as completed.
var ErrUpdateBusy = errors.New("update is currently being processed")

const defaultMaxUpdateAttempts = 5

var (
	updatesProcessed = metrics.Counter("updates_processed_total", "Telegram updates acknowledged after processing.", "kind")
	updatesFailed    = metrics.Counter("updates_failed_total", "Telegram update processing failures by error class.", "error_class")
	updatesDead      = metrics.Counter("updates_dead_letter_total", "Telegram updates skipped after exhausting the retry budget.")
	updatePanics     = metrics.Counter("update_handler_panics_total", "Panics recovered while handling Telegram updates.")
)

type UpdateProcessor struct {
	store       UpdateLeaseStore
	handler     UpdateHandler
	lease       time.Duration
	maxAttempts int
	logger      *slog.Logger
	now         func() time.Time
}

type ProcessorOptions struct {
	// MaxAttempts bounds retries per update before it is dead-lettered.
	// Defaults to 5; values below 1 are rejected.
	MaxAttempts int
	Logger      *slog.Logger
}

func NewUpdateProcessor(store UpdateLeaseStore, handler UpdateHandler, lease time.Duration, options ProcessorOptions) (*UpdateProcessor, error) {
	if store == nil || handler == nil || lease <= 0 {
		return nil, errors.New("update processor requires a store, handler, and positive lease")
	}
	maxAttempts := defaultMaxUpdateAttempts
	if options.MaxAttempts != 0 {
		maxAttempts = options.MaxAttempts
	}
	if maxAttempts < 1 {
		return nil, errors.New("update processor requires a positive retry budget")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &UpdateProcessor{
		store: store, handler: handler, lease: lease, maxAttempts: maxAttempts, logger: logger,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (p *UpdateProcessor) Process(ctx context.Context, update telegram.Update) error {
	now := p.now()
	payload, err := json.Marshal(update)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(payload)
	lease, err := p.store.ClaimUpdate(ctx, repository.ClaimUpdateParams{
		TelegramUpdateID: update.UpdateID, UpdateType: updateType(update),
		PayloadSHA256: digest[:], Now: now, LeaseUntil: now.Add(p.lease),
		MaxAttempts: p.maxAttempts,
	})
	if err != nil {
		if errors.Is(err, repository.ErrUpdateDead) {
			// The retry budget is exhausted: acknowledge and skip the update so
			// the stream keeps flowing. The row keeps its failed state and
			// last_error for diagnosis and is pruned by retention cleanup.
			updatesDead.Inc()
			p.logger.Error("update dead-lettered after repeated failures",
				"update_id", update.UpdateID, "update_type", updateType(update))
			return nil
		}
		return err
	}
	if lease.AlreadyDone {
		return nil
	}
	if !lease.Acquired {
		return ErrUpdateBusy
	}
	if lease.LeaseUntil == nil {
		return repository.ErrConflict
	}

	handlerErr := p.handleSafely(ctx, update)
	if handlerErr != nil {
		errorCode := "handler_failed"
		if errors.Is(handlerErr, errPanicRecovered) {
			errorCode = "panic_recovered"
		}
		p.finishFail(ctx, update.UpdateID, *lease.LeaseUntil, errorCode)
		return handlerErr
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	err = p.store.CompleteUpdate(finishCtx, repository.FinishUpdateParams{
		TelegramUpdateID: update.UpdateID, ExpectedLeaseUntil: *lease.LeaseUntil, Now: p.now(),
	})
	if err == nil {
		updatesProcessed.Inc(updateType(update))
	}
	return err
}

// errPanicRecovered wraps panics raised by the handler so Process can record
// them as a distinguishable failure instead of crashing the process. One
// poison update must never take down the whole bot.
var errPanicRecovered = errors.New("update handler panicked")

func (p *UpdateProcessor) handleSafely(ctx context.Context, update telegram.Update) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			updatePanics.Inc()
			err = fmt.Errorf("%w: %v", errPanicRecovered, recovered)
		}
	}()
	return p.handler.HandleUpdate(ctx, update)
}

func (p *UpdateProcessor) finishFail(ctx context.Context, updateID int64, leaseUntil time.Time, errorCode string) {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = p.store.FailUpdate(finishCtx, repository.FinishUpdateParams{
		TelegramUpdateID: updateID, ExpectedLeaseUntil: leaseUntil,
		Now: p.now(), ErrorCode: errorCode,
	})
}

func updateType(update telegram.Update) string {
	switch {
	case update.CallbackQuery != nil:
		return "callback_query"
	case update.GuestMessage != nil:
		return "guest_message"
	case update.InlineQuery != nil:
		return "inline_query"
	case update.Message != nil:
		return "message"
	case update.EditedMessage != nil:
		return "edited_message"
	default:
		return "unknown"
	}
}
