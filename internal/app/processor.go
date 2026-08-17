package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

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

type UpdateProcessor struct {
	store   UpdateLeaseStore
	handler UpdateHandler
	lease   time.Duration
	now     func() time.Time
}

func NewUpdateProcessor(store UpdateLeaseStore, handler UpdateHandler, lease time.Duration) (*UpdateProcessor, error) {
	if store == nil || handler == nil || lease <= 0 {
		return nil, errors.New("update processor requires a store, handler, and positive lease")
	}
	return &UpdateProcessor{
		store: store, handler: handler, lease: lease,
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
	})
	if err != nil {
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

	if err := p.handler.HandleUpdate(ctx, update); err != nil {
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = p.store.FailUpdate(finishCtx, repository.FinishUpdateParams{
			TelegramUpdateID: update.UpdateID, ExpectedLeaseUntil: *lease.LeaseUntil,
			Now: p.now(), ErrorCode: "handler_failed",
		})
		return err
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	return p.store.CompleteUpdate(finishCtx, repository.FinishUpdateParams{
		TelegramUpdateID: update.UpdateID, ExpectedLeaseUntil: *lease.LeaseUntil, Now: p.now(),
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
