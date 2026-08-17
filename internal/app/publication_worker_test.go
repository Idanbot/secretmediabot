package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type publishResponse struct {
	didWork bool
	err     error
}

type publicationPublisherStub struct {
	mu        sync.Mutex
	responses []publishResponse
	calls     int
	onCall    func(context.Context, int)
}

func (s *publicationPublisherStub) PublishNext(ctx context.Context) (bool, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	var response publishResponse
	if len(s.responses) > 0 {
		response = s.responses[0]
		s.responses = s.responses[1:]
	}
	onCall := s.onCall
	s.mu.Unlock()
	if onCall != nil {
		onCall(ctx, call)
	}
	return response.didWork, response.err
}

func (s *publicationPublisherStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestPublicationWorkerDrainsAtMostOneBoundedBatch(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	publisher := &publicationPublisherStub{
		responses: []publishResponse{{didWork: true}, {didWork: true}, {didWork: true}, {didWork: true}},
		onCall: func(_ context.Context, call int) {
			if call == 3 {
				cancel()
			}
		},
	}
	worker, err := NewPublicationWorkerWithOptions(publisher, PublicationWorkerOptions{
		Interval: time.Hour, BatchSize: 3, AttemptTimeout: time.Second, InitialBackoff: time.Second, MaxBackoff: time.Minute,
	}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatalf("NewPublicationWorker() error = %v", err)
	}

	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if calls := publisher.callCount(); calls != 3 {
		t.Fatalf("PublishNext() calls = %d, want bounded batch of 3", calls)
	}
}

func TestPublicationWorkerUsesABoundedDefaultBatch(t *testing.T) {
	t.Parallel()

	publisher := &publicationPublisherStub{responses: make([]publishResponse, defaultPublicationBatchSize)}
	for i := range publisher.responses {
		publisher.responses[i].didWork = true
	}
	worker, err := NewPublicationWorker(publisher, time.Hour, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatalf("NewPublicationWorker() error = %v", err)
	}
	worker.wait = func(context.Context, time.Duration) bool { return false }

	if err := worker.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if calls := publisher.callCount(); calls != defaultPublicationBatchSize {
		t.Fatalf("PublishNext() calls = %d, want default bounded batch of %d", calls, defaultPublicationBatchSize)
	}
}

func TestPublicationWorkerStopsDrainingWhenQueueIsEmpty(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	publisher := &publicationPublisherStub{
		responses: []publishResponse{{didWork: false}},
		onCall: func(_ context.Context, _ int) {
			cancel()
		},
	}
	worker := mustPublicationWorker(t, publisher)

	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if calls := publisher.callCount(); calls != 1 {
		t.Fatalf("PublishNext() calls = %d, want 1", calls)
	}
}

func TestPublicationWorkerBacksOffAndRedactsOperationalFailures(t *testing.T) {
	t.Parallel()

	secretError := errors.New("publish secret-media for user_id=123456 failed")
	publisher := &publicationPublisherStub{responses: []publishResponse{
		{err: secretError},
		{err: secretError},
		{didWork: false},
		{err: secretError},
	}}
	var logs bytes.Buffer
	worker, err := NewPublicationWorkerWithOptions(publisher, PublicationWorkerOptions{
		Interval: 10 * time.Second, BatchSize: 5, AttemptTimeout: time.Second, InitialBackoff: time.Second, MaxBackoff: 4 * time.Second,
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("NewPublicationWorker() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var delays []time.Duration
	worker.wait = func(_ context.Context, delay time.Duration) bool {
		delays = append(delays, delay)
		if len(delays) == 4 {
			cancel()
			return false
		}
		return true
	}
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	wantDelays := []time.Duration{time.Second, 2 * time.Second, 10 * time.Second, time.Second}
	if len(delays) != len(wantDelays) {
		t.Fatalf("wait delays = %v, want %v", delays, wantDelays)
	}
	for i := range wantDelays {
		if delays[i] != wantDelays[i] {
			t.Fatalf("wait delays = %v, want %v", delays, wantDelays)
		}
	}
	output := logs.String()
	if !strings.Contains(output, "publication retry failed") || !strings.Contains(output, "retry_in") {
		t.Fatalf("operational log = %q, want generic failure and retry delay", output)
	}
	for _, sensitive := range []string{"secret-media", "user_id", "123456"} {
		if strings.Contains(output, sensitive) {
			t.Fatalf("operational log leaked %q: %q", sensitive, output)
		}
	}
}

func TestPublicationWorkerStopsCleanlyWhilePublishIsBlocked(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	var once sync.Once
	publisher := &publicationPublisherStub{onCall: func(ctx context.Context, _ int) {
		once.Do(func() { close(started) })
		<-ctx.Done()
	}}
	worker := mustPublicationWorker(t, publisher)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not call PublishNext")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error after cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}
}

func TestPublicationWorkerAppliesAttemptTimeout(t *testing.T) {
	t.Parallel()

	const attemptTimeout = 2 * time.Second
	var deadline time.Time
	publisher := &publicationPublisherStub{onCall: func(ctx context.Context, _ int) {
		var ok bool
		deadline, ok = ctx.Deadline()
		if !ok {
			t.Fatal("PublishNext context has no deadline")
		}
	}}
	worker, err := NewPublicationWorkerWithTimeout(
		publisher,
		time.Hour,
		attemptTimeout,
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	)
	if err != nil {
		t.Fatalf("NewPublicationWorkerWithTimeout() error = %v", err)
	}
	worker.wait = func(context.Context, time.Duration) bool { return false }
	started := time.Now()

	if err := worker.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	want := started.Add(attemptTimeout)
	if delta := deadline.Sub(want); delta < -100*time.Millisecond || delta > 100*time.Millisecond {
		t.Fatalf("PublishNext deadline = %v, want approximately %v (delta %v)", deadline, want, delta)
	}
}

func TestNewPublicationWorkerValidatesDependenciesAndBounds(t *testing.T) {
	t.Parallel()

	validPublisher := &publicationPublisherStub{}
	valid := PublicationWorkerOptions{
		Interval: time.Second, BatchSize: 1, AttemptTimeout: time.Second, InitialBackoff: time.Second, MaxBackoff: time.Minute,
	}
	tests := []struct {
		name      string
		publisher PublicationPublisher
		options   PublicationWorkerOptions
	}{
		{name: "publisher", options: valid},
		{name: "interval", publisher: validPublisher, options: PublicationWorkerOptions{BatchSize: 1, AttemptTimeout: time.Second, InitialBackoff: time.Second, MaxBackoff: time.Minute}},
		{name: "batch", publisher: validPublisher, options: PublicationWorkerOptions{Interval: time.Second, AttemptTimeout: time.Second, InitialBackoff: time.Second, MaxBackoff: time.Minute}},
		{name: "attempt timeout", publisher: validPublisher, options: PublicationWorkerOptions{Interval: time.Second, BatchSize: 1, InitialBackoff: time.Second, MaxBackoff: time.Minute}},
		{name: "initial backoff", publisher: validPublisher, options: PublicationWorkerOptions{Interval: time.Second, BatchSize: 1, AttemptTimeout: time.Second, MaxBackoff: time.Minute}},
		{name: "maximum backoff", publisher: validPublisher, options: PublicationWorkerOptions{Interval: time.Second, BatchSize: 1, AttemptTimeout: time.Second, InitialBackoff: time.Minute, MaxBackoff: time.Second}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewPublicationWorkerWithOptions(test.publisher, test.options, nil); err == nil {
				t.Fatal("NewPublicationWorker() unexpectedly accepted invalid configuration")
			}
		})
	}
}

func mustPublicationWorker(t *testing.T, publisher PublicationPublisher) *PublicationWorker {
	t.Helper()
	worker, err := NewPublicationWorkerWithOptions(publisher, PublicationWorkerOptions{
		Interval: time.Hour, BatchSize: 10, AttemptTimeout: time.Second, InitialBackoff: time.Second, MaxBackoff: time.Minute,
	}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatalf("NewPublicationWorker() error = %v", err)
	}
	return worker
}
