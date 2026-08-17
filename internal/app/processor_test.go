package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/telegram"
)

type updateLeaseStoreStub struct {
	lease         repository.UpdateLease
	completed     int
	failed        int
	claimRequests []repository.ClaimUpdateParams
}

func (s *updateLeaseStoreStub) ClaimUpdate(
	_ context.Context,
	params repository.ClaimUpdateParams,
) (repository.UpdateLease, error) {
	s.claimRequests = append(s.claimRequests, params)
	return s.lease, nil
}

func (s *updateLeaseStoreStub) CompleteUpdate(context.Context, repository.FinishUpdateParams) error {
	s.completed++
	return nil
}

func (s *updateLeaseStoreStub) FailUpdate(context.Context, repository.FinishUpdateParams) error {
	s.failed++
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
	}), time.Minute)
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
	}), time.Minute)
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
