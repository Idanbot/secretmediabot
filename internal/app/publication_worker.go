package app

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// PublicationPublisher owns one atomic claim/send/finish attempt. A false
// result means there is currently no due publication to process.
type PublicationPublisher interface {
	PublishNext(context.Context) (didWork bool, err error)
}

type PublicationWorkerOptions struct {
	Interval       time.Duration
	BatchSize      int
	AttemptTimeout time.Duration
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

const (
	defaultPublicationBatchSize  = 50
	defaultPublicationTimeout    = 15 * time.Second
	defaultPublicationBackoff    = time.Second
	defaultPublicationMaxBackoff = 30 * time.Second
)

// PublicationWorker drains a bounded number of due envelope publications.
// It intentionally knows nothing about whisper identifiers or payloads, which
// keeps operational logging from accidentally disclosing either.
type PublicationWorker struct {
	publisher PublicationPublisher
	options   PublicationWorkerOptions
	logger    *slog.Logger
	wait      func(context.Context, time.Duration) bool
}

func NewPublicationWorker(
	publisher PublicationPublisher,
	interval time.Duration,
	logger *slog.Logger,
) (*PublicationWorker, error) {
	return NewPublicationWorkerWithOptions(publisher, PublicationWorkerOptions{
		Interval:       interval,
		BatchSize:      defaultPublicationBatchSize,
		AttemptTimeout: defaultPublicationTimeout,
		InitialBackoff: defaultPublicationBackoff,
		MaxBackoff:     defaultPublicationMaxBackoff,
	}, logger)
}

// NewPublicationWorkerWithTimeout configures the per-publication attempt
// deadline independently of the shared Telegram HTTP client's media timeout.
func NewPublicationWorkerWithTimeout(
	publisher PublicationPublisher,
	interval time.Duration,
	attemptTimeout time.Duration,
	logger *slog.Logger,
) (*PublicationWorker, error) {
	return NewPublicationWorkerWithOptions(publisher, PublicationWorkerOptions{
		Interval:       interval,
		BatchSize:      defaultPublicationBatchSize,
		AttemptTimeout: attemptTimeout,
		InitialBackoff: defaultPublicationBackoff,
		MaxBackoff:     defaultPublicationMaxBackoff,
	}, logger)
}

func NewPublicationWorkerWithOptions(
	publisher PublicationPublisher,
	options PublicationWorkerOptions,
	logger *slog.Logger,
) (*PublicationWorker, error) {
	if publisher == nil {
		return nil, errors.New("publication worker requires a publisher")
	}
	if options.Interval <= 0 || options.BatchSize <= 0 || options.AttemptTimeout <= 0 || options.InitialBackoff <= 0 ||
		options.MaxBackoff < options.InitialBackoff {
		return nil, errors.New("publication worker requires positive, ordered scheduling bounds")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &PublicationWorker{
		publisher: publisher,
		options:   options,
		logger:    logger,
		wait:      waitContext,
	}, nil
}

func (w *PublicationWorker) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("publication worker requires a context")
	}
	backoff := w.options.InitialBackoff
	for {
		err := w.drain(ctx)
		if ctx.Err() != nil {
			return nil
		}

		delay := w.options.Interval
		if err != nil {
			delay = backoff
			// Repository and Telegram errors may include identifiers or request
			// data. Log the operation and schedule only; detailed error values
			// stay inside the trusted call path.
			w.logger.WarnContext(ctx, "publication retry failed", "retry_in", delay)
			backoff = growPublicationBackoff(backoff, w.options.MaxBackoff)
		} else {
			backoff = w.options.InitialBackoff
		}

		if !w.wait(ctx, delay) {
			return nil
		}
	}
}

func (w *PublicationWorker) drain(ctx context.Context) error {
	for range w.options.BatchSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, w.options.AttemptTimeout)
		didWork, err := w.publisher.PublishNext(attemptCtx)
		cancel()
		if err != nil {
			return err
		}
		if !didWork {
			return nil
		}
	}
	return nil
}

func growPublicationBackoff(current, maximum time.Duration) time.Duration {
	if current >= maximum || current > maximum-current {
		return maximum
	}
	return current * 2
}
