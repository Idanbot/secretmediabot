package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/token"
)

type concurrentTestStore struct {
	repository.Store
	mu             sync.Mutex
	opened         bool
	reservations   int
	completedOpens int
}

func (s *concurrentTestStore) ReserveOpen(ctx context.Context, params repository.ReserveOpenParams) (repository.OpenReservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opened {
		return repository.OpenReservation{}, repository.ErrAlreadyOpened
	}
	s.opened = true
	s.reservations++
	return repository.OpenReservation{
		Whisper: domain.Whisper{
			ID:          uuid.New(),
			SenderID:    100,
			RecipientID: params.TelegramUserID,
			Status:      domain.WhisperOpening,
			OneTime:     true,
		},
		EventID: 1,
		Content: repository.DeliveryContent{
			Kind: domain.PayloadText,
		},
	}, nil
}

func (s *concurrentTestStore) CompleteOpen(ctx context.Context, params repository.CompleteOpenParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completedOpens++
	return nil
}

func TestServiceConcurrentOneTimeOpenRace(t *testing.T) {
	t.Parallel()

	tok, err := token.Generate()
	if err != nil {
		t.Fatalf("token.Generate() error = %v", err)
	}

	mockStore := &concurrentTestStore{}
	svc := &Service{
		store:   mockStore,
		options: Options{OpenLease: 30 * time.Second},
		now:     time.Now,
	}

	const workerCount = 50
	start := make(chan struct{})
	type openResult struct {
		err error
	}
	results := make(chan openResult, workerCount)
	var wg sync.WaitGroup

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		callbackID := fmt.Sprintf("cb-%d", i)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.ReserveOpen(context.Background(), tok.Data, 200, callbackID)
			results <- openResult{err: err}
		}()
	}

	close(start)
	wg.Wait()
	close(results)

	successCount := 0
	deniedCount := 0

	for res := range results {
		if res.err == nil {
			successCount++
		} else {
			if !errors.Is(res.err, ErrWhisperUnavailable) && !errors.Is(res.err, ErrWhisperAlreadyOpened) {
				t.Errorf("unexpected error on losing race: %v", res.err)
			}
			deniedCount++
		}
	}

	if successCount != 1 {
		t.Fatalf("expected exactly 1 successful reservation among %d concurrent workers, got %d", workerCount, successCount)
	}
	if deniedCount != workerCount-1 {
		t.Fatalf("expected %d denied reservations, got %d", workerCount-1, deniedCount)
	}
}
