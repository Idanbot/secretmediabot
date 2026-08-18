package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/telegram"
)

type updateLeaseStoreStub struct {
	lease         repository.UpdateLease
	claimErr      error
	failCapture   *repository.FinishUpdateParams
	completed     int
	failed        int
	claimRequests []repository.ClaimUpdateParams
}

func (s *updateLeaseStoreStub) ClaimUpdate(
	_ context.Context,
	params repository.ClaimUpdateParams,
) (repository.UpdateLease, error) {
	s.claimRequests = append(s.claimRequests, params)
	if s.claimErr != nil {
		return repository.UpdateLease{}, s.claimErr
	}
	return s.lease, nil
}

func (s *updateLeaseStoreStub) CompleteUpdate(context.Context, repository.FinishUpdateParams) error {
	s.completed++
	return nil
}

func (s *updateLeaseStoreStub) FailUpdate(_ context.Context, params repository.FinishUpdateParams) error {
	s.failed++
	if s.failCapture != nil {
		*s.failCapture = params
	}
	return nil
}

type updateHandlerFunc func(context.Context, telegram.Update) error

func (f updateHandlerFunc) HandleUpdate(ctx context.Context, update telegram.Update) error {
	return f(ctx, update)
}

func TestUpdateProcessorReturnsBusyForAnActiveLease(t *testing.T) {
	t.Parallel()

	store := &updateLeaseStoreStub{lease: repository.UpdateLease{Acquired: false, AlreadyDone: false}}
	handlerCalls := 0
	processor, err := NewUpdateProcessor(store, updateHandlerFunc(func(context.Context, telegram.Update) error {
		handlerCalls++
		return nil
	}), time.Minute, ProcessorOptions{})
	if err != nil {
		t.Fatalf("NewUpdateProcessor() error = %v", err)
	}

	err = processor.Process(context.Background(), telegram.Update{UpdateID: 42})

	if !errors.Is(err, ErrUpdateBusy) {
		t.Fatalf("Process() error = %v, want ErrUpdateBusy", err)
	}
	if handlerCalls != 0 || store.completed != 0 || store.failed != 0 {
		t.Fatalf(
			"busy update side effects: handler=%d complete=%d fail=%d, want all zero",
			handlerCalls,
			store.completed,
			store.failed,
		)
	}
}

func TestUpdateProcessorAcknowledgesAnAlreadyCompletedLease(t *testing.T) {
	t.Parallel()

	store := &updateLeaseStoreStub{lease: repository.UpdateLease{AlreadyDone: true}}
	handlerCalls := 0
	processor, err := NewUpdateProcessor(store, updateHandlerFunc(func(context.Context, telegram.Update) error {
		handlerCalls++
		return nil
	}), time.Minute, ProcessorOptions{})
	if err != nil {
		t.Fatalf("NewUpdateProcessor() error = %v", err)
	}

	if err := processor.Process(context.Background(), telegram.Update{UpdateID: 42}); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if handlerCalls != 0 || store.completed != 0 || store.failed != 0 {
		t.Fatalf(
			"completed update side effects: handler=%d complete=%d fail=%d, want all zero",
			handlerCalls,
			store.completed,
			store.failed,
		)
	}
}

func TestUpdateProcessorRecoversHandlerPanics(t *testing.T) {
	t.Parallel()

	leaseUntil := time.Now().UTC().Add(time.Minute)
	var failParams repository.FinishUpdateParams
	store := &updateLeaseStoreStub{
		lease: repository.UpdateLease{Acquired: true, Attempts: 1, LeaseUntil: &leaseUntil},
	}
	store.failCapture = &failParams
	processor, err := NewUpdateProcessor(store, updateHandlerFunc(func(context.Context, telegram.Update) error {
		panic("poison update payload")
	}), time.Minute, ProcessorOptions{})
	if err != nil {
		t.Fatalf("NewUpdateProcessor() error = %v", err)
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatal("Process must not propagate handler panics")
		}
	}()
	err = processor.Process(context.Background(), telegram.Update{UpdateID: 7})
	if err == nil || !errors.Is(err, errPanicRecovered) {
		t.Fatalf("Process() error = %v, want a wrapped errPanicRecovered", err)
	}
	if store.completed != 0 || store.failed != 1 {
		t.Fatalf("side effects: complete=%d fail=%d, want fail=1", store.completed, store.failed)
	}
	if failParams.ErrorCode != "panic_recovered" {
		t.Errorf("FailUpdate error code = %q, want panic_recovered", failParams.ErrorCode)
	}
}

func TestUpdateProcessorDeadLettersExhaustedUpdates(t *testing.T) {
	t.Parallel()

	store := &updateLeaseStoreStub{
		claimErr: fmt.Errorf("%w: update 7 failed 5 times", repository.ErrUpdateDead),
	}
	handlerCalls := 0
	processor, err := NewUpdateProcessor(store, updateHandlerFunc(func(context.Context, telegram.Update) error {
		handlerCalls++
		return nil
	}), time.Minute, ProcessorOptions{})
	if err != nil {
		t.Fatalf("NewUpdateProcessor() error = %v", err)
	}

	// A dead-lettered update is acknowledged so the polling offset can advance.
	if err := processor.Process(context.Background(), telegram.Update{UpdateID: 7}); err != nil {
		t.Fatalf("Process() error = %v, want nil for a dead-lettered update", err)
	}
	if handlerCalls != 0 || store.completed != 0 || store.failed != 0 {
		t.Fatalf("dead update side effects: handler=%d complete=%d fail=%d", handlerCalls, store.completed, store.failed)
	}
}

func TestUpdateProcessorRejectsInvalidOptions(t *testing.T) {
	t.Parallel()

	if _, err := NewUpdateProcessor(&updateLeaseStoreStub{}, updateHandlerFunc(nil), time.Minute, ProcessorOptions{MaxAttempts: -1}); err == nil {
		t.Error("negative MaxAttempts must be rejected")
	}
	if _, err := NewUpdateProcessor(nil, updateHandlerFunc(nil), time.Minute, ProcessorOptions{}); err == nil {
		t.Error("nil store must be rejected")
	}
}
