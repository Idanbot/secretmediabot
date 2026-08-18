package app

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/idan/secretmediabot/internal/metrics"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/telegram"
)

type UpdateSource interface {
	GetUpdates(context.Context, telegram.GetUpdatesRequest) ([]telegram.Update, error)
}

type Processor interface {
	Process(context.Context, telegram.Update) error
}

const pollBackoffCeiling = 30 * time.Second

var pollFailures = metrics.Counter("telegram_poll_failures_total", "Failed getUpdates calls by error class.", "error_class")

type Poller struct {
	source         UpdateSource
	process        Processor
	pollTimeout    time.Duration
	requestTimeout time.Duration
	logger         *slog.Logger
	wait           func(context.Context, time.Duration) bool
	jitter         func(time.Duration) time.Duration
}

func NewPoller(
	source UpdateSource,
	process Processor,
	pollTimeout time.Duration,
	requestTimeout time.Duration,
	logger *slog.Logger,
) (*Poller, error) {
	if source == nil || process == nil || pollTimeout <= 0 || requestTimeout <= 0 {
		return nil, errors.New("poller requires a source, processor, and positive timeouts")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Poller{
		source:         source,
		process:        process,
		pollTimeout:    pollTimeout,
		requestTimeout: requestTimeout,
		logger:         logger,
		wait:           waitContext,
		jitter:         defaultJitter,
	}, nil
}

func (p *Poller) Run(ctx context.Context) error {
	var offset int64
	backoff := time.Second
	for ctx.Err() == nil {
		requestCtx, cancel := context.WithTimeout(ctx, p.pollTimeout+p.requestTimeout)
		updates, err := p.source.GetUpdates(requestCtx, telegram.GetUpdatesRequest{
			Offset:         offset,
			Limit:          100,
			Timeout:        p.pollTimeout,
			AllowedUpdates: []string{"message", "callback_query", "guest_message", "inline_query"},
		})
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Telegram errors can include request details. Keep operational logs
			// useful without copying untrusted response text into them.
			if requested := telegramRetryAfter(err); requested > backoff {
				backoff = requested
			}
			class := errorClass(err)
			pollFailures.Inc(class)
			p.logger.WarnContext(ctx, "telegram polling failed", "retry_in", backoff, "error_class", class)
			if !p.wait(ctx, backoff) {
				return nil
			}
			backoff = p.jitter(min(backoff*2, pollBackoffCeiling))
			continue
		}

		batchFailed := false
		for _, update := range updates {
			if ctx.Err() != nil {
				return nil
			}
			if update.UpdateID < offset {
				continue
			}
			if err := p.process.Process(ctx, update); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				// Do not advance the offset. Telegram will return this update again,
				// while the processed_updates lease prevents duplicate commits. The
				// processor dead-letters updates that exhaust their retry budget,
				// so this loop always terminates for a deterministically failing
				// update.
				class := errorClass(err)
				pollFailures.Inc(class)
				p.logger.ErrorContext(ctx, "telegram update processing failed",
					"retry_in", backoff, "error_class", class)
				if !p.wait(ctx, backoff) {
					return nil
				}
				backoff = p.jitter(min(backoff*2, pollBackoffCeiling))
				batchFailed = true
				break
			}
			offset = update.UpdateID + 1
		}
		if !batchFailed {
			backoff = time.Second
		}
	}
	return nil
}

// telegramRetryAfter honors Telegram's requested Retry-After delay, which may
// exceed the poller's own backoff ceiling under flood limits.
func telegramRetryAfter(err error) time.Duration {
	var apiErr *telegram.APIError
	if errors.As(err, &apiErr) {
		return apiErr.RetryAfter()
	}
	return 0
}

func errorClass(err error) string {
	switch {
	case errors.Is(err, ErrUpdateBusy):
		return "update_busy"
	case errors.Is(err, repository.ErrLeaseLost):
		return "lease_lost"
	case errors.Is(err, repository.ErrConflict):
		return "conflict"
	case telegram.IsRateLimited(err):
		return "rate_limited"
	case telegram.IsPermanent(err):
		return "telegram_permanent"
	default:
		var apiErr *telegram.APIError
		if errors.As(err, &apiErr) {
			return "telegram_api"
		}
		if errors.Is(err, telegram.ErrInvalidResponse) {
			return "telegram_protocol"
		}
		return "internal"
	}
}

// defaultJitter spreads synchronized retries by up to 25% of the delay.
func defaultJitter(delay time.Duration) time.Duration {
	if delay <= 0 {
		return delay
	}
	return delay + time.Duration(rand.Int64N(int64(delay/4)+1))
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
