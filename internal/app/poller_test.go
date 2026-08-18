package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idan/secretmediabot/internal/telegram"
)

type sourceStub struct {
	mu       sync.Mutex
	requests []telegram.GetUpdatesRequest
	updates  []telegram.Update
}

func (s *sourceStub) GetUpdates(ctx context.Context, req telegram.GetUpdatesRequest) ([]telegram.Update, error) {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	if len(s.updates) > 0 {
		updates := s.updates
		s.updates = nil
		s.mu.Unlock()
		return updates, nil
	}
	s.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

type processStub struct {
	mu      sync.Mutex
	updates []int64
	err     error
	cancel  context.CancelFunc
}

type sourceFunc func(context.Context, telegram.GetUpdatesRequest) ([]telegram.Update, error)

func (f sourceFunc) GetUpdates(ctx context.Context, request telegram.GetUpdatesRequest) ([]telegram.Update, error) {
	return f(ctx, request)
}

type processFunc func(context.Context, telegram.Update) error

func (f processFunc) Process(ctx context.Context, update telegram.Update) error {
	return f(ctx, update)
}

func (p *processStub) Process(_ context.Context, update telegram.Update) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.updates = append(p.updates, update.UpdateID)
	if p.cancel != nil {
		p.cancel()
	}
	return p.err
}

func TestPollerProcessesUpdatesAndAdvancesOffset(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	source := &sourceStub{updates: []telegram.Update{{UpdateID: 10}, {UpdateID: 11}}}
	processor := &processStub{cancel: cancel}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	poller, err := NewPoller(source, processor, time.Second, time.Second, logger)
	if err != nil {
		t.Fatalf("NewPoller() error = %v", err)
	}

	if err := poller.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	processor.mu.Lock()
	defer processor.mu.Unlock()
	if len(processor.updates) != 1 || processor.updates[0] != 10 {
		t.Fatalf("processed updates = %v", processor.updates)
	}
}

func TestPollerStopsOnContextAfterProcessingFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	source := &sourceStub{updates: []telegram.Update{{UpdateID: 10}}}
	processor := &processStub{err: errors.New("boom"), cancel: cancel}
	poller, err := NewPoller(source, processor, time.Second, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewPoller() error = %v", err)
	}

	if err := poller.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestPollerBoundsGetUpdatesByPollAndRequestTimeout(t *testing.T) {
	t.Parallel()

	const (
		pollTimeout    = 2 * time.Second
		requestTimeout = 3 * time.Second
	)
	ctx, cancel := context.WithCancel(context.Background())
	var deadline time.Time
	source := sourceFunc(func(callCtx context.Context, _ telegram.GetUpdatesRequest) ([]telegram.Update, error) {
		var ok bool
		deadline, ok = callCtx.Deadline()
		if !ok {
			t.Fatal("GetUpdates context has no deadline")
		}
		cancel()
		return nil, callCtx.Err()
	})
	poller, err := NewPoller(source, processFunc(func(context.Context, telegram.Update) error {
		return nil
	}), pollTimeout, requestTimeout, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewPoller() error = %v", err)
	}
	started := time.Now()

	if err := poller.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	want := started.Add(pollTimeout + requestTimeout)
	if delta := deadline.Sub(want); delta < -100*time.Millisecond || delta > 100*time.Millisecond {
		t.Fatalf("GetUpdates deadline = %v, want approximately %v (delta %v)", deadline, want, delta)
	}
}

func TestPollerRetriesBusyUpdateWithGrowingBackoffAndRedactedLogs(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	const sensitive = "secret-media user_id=987654"
	var offsets []int64
	source := sourceFunc(func(_ context.Context, request telegram.GetUpdatesRequest) ([]telegram.Update, error) {
		offsets = append(offsets, request.Offset)
		return []telegram.Update{{UpdateID: 987654}}, nil
	})
	processor := processFunc(func(context.Context, telegram.Update) error {
		return errors.Join(ErrUpdateBusy, errors.New(sensitive))
	})
	var logs bytes.Buffer
	poller, err := NewPoller(source, processor, time.Second, time.Second, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("NewPoller() error = %v", err)
	}
	var delays []time.Duration
	poller.jitter = func(delay time.Duration) time.Duration { return delay }
	poller.wait = func(_ context.Context, delay time.Duration) bool {
		delays = append(delays, delay)
		if len(delays) == 2 {
			cancel()
			return false
		}
		return true
	}

	if err := poller.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second}
	if len(delays) != len(want) || delays[0] != want[0] || delays[1] != want[1] {
		t.Fatalf("processing backoff delays = %v, want %v", delays, want)
	}
	if len(offsets) != 2 || offsets[0] != 0 || offsets[1] != 0 {
		t.Fatalf("poll offsets = %v, want busy update retried without advancing offset", offsets)
	}
	if output := logs.String(); strings.Contains(output, sensitive) || strings.Contains(output, "987654") {
		t.Fatalf("processing log leaked sensitive error or update ID: %q", output)
	}
}

func TestPollerSourceFailureLogIsRedacted(t *testing.T) {
	t.Parallel()

	const sensitive = "telegram response contains secret-token-value"
	source := sourceFunc(func(context.Context, telegram.GetUpdatesRequest) ([]telegram.Update, error) {
		return nil, errors.New(sensitive)
	})
	var logs bytes.Buffer
	poller, pollerErr := NewPoller(source, processFunc(func(context.Context, telegram.Update) error {
		return nil
	}), time.Second, time.Second, slog.New(slog.NewTextHandler(&logs, nil)))
	if pollerErr != nil {
		t.Fatalf("NewPoller() error = %v", pollerErr)
	}
	poller.wait = func(context.Context, time.Duration) bool { return false }

	if err := poller.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if output := logs.String(); strings.Contains(output, sensitive) {
		t.Fatalf("polling log leaked source error: %q", output)
	}
}
