package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/idan/secretmediabot/internal/telegram"
)

type UpdateSource interface {
	GetUpdates(context.Context, telegram.GetUpdatesRequest) ([]telegram.Update, error)
}

type Processor interface {
	Process(context.Context, telegram.Update) error
}

type Poller struct {
	source         UpdateSource
	process        Processor
	pollTimeout    time.Duration
	requestTimeout time.Duration
	logger         *slog.Logger
	wait           func(context.Context, time.Duration) bool
}

func NewPoller(
	source UpdateSource,
	process Processor,
	pollTimeout time.Duration,
	requestTimeout time.Duration,
	logger *slog.Logger,
) *Poller {
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
	}
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
			p.logger.WarnContext(ctx, "telegram polling failed", "retry_in", backoff)
			if !p.wait(ctx, backoff) {
				return nil
			}
			backoff = min(backoff*2, 30*time.Second)
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
				// while the processed_updates lease prevents duplicate commits.
				p.logger.ErrorContext(ctx, "telegram update processing failed", "retry_in", backoff)
				if !p.wait(ctx, backoff) {
					return nil
				}
				backoff = min(backoff*2, 30*time.Second)
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
