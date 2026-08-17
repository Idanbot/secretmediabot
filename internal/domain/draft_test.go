package domain

import (
	"errors"
	"testing"
	"time"
)

func TestNewDraftDefaultsAndActivityBoundary(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	draft, err := NewDraft(NewDraftParams{
		ComposeTokenHash: make([]byte, 32),
		SenderID:         1,
		RecipientID:      2,
		SourceChatID:     -1001,
		CreatedAt:        now,
	})
	if err != nil {
		t.Fatalf("NewDraft() error = %v", err)
	}
	if draft.State != DraftAwaitingMedia {
		t.Fatalf("state = %q", draft.State)
	}
	if want := now.Add(DefaultDraftTTL); !draft.ExpiresAt.Equal(want) {
		t.Fatalf("expiry = %v, want %v", draft.ExpiresAt, want)
	}
	if !draft.IsActive(draft.ExpiresAt.Add(-time.Nanosecond)) {
		t.Fatal("draft should be active immediately before expiry")
	}
	if draft.IsActive(draft.ExpiresAt) {
		t.Fatal("draft should be inactive at its expiry instant")
	}
	if !draft.IsExpired(draft.ExpiresAt) {
		t.Fatal("draft should be expired at its expiry instant")
	}
}

func TestNewDraftRejectsSelfRecipient(t *testing.T) {
	t.Parallel()
	_, err := NewDraft(NewDraftParams{ComposeTokenHash: make([]byte, 32), SenderID: 7, RecipientID: 7, SourceChatID: -1})
	if !errors.Is(err, ErrInvalidRecipient) {
		t.Fatalf("error = %v, want ErrInvalidRecipient", err)
	}
}

func TestDraftStateTransitions(t *testing.T) {
	t.Parallel()
	for _, next := range []DraftState{DraftIngestingMedia, DraftCancelled, DraftExpired} {
		if !DraftAwaitingMedia.CanTransitionTo(next) {
			t.Errorf("awaiting_media should transition to %q", next)
		}
	}
	for _, next := range []DraftState{DraftAwaitingMedia, DraftCompleted, DraftCancelled, DraftExpired} {
		if !DraftIngestingMedia.CanTransitionTo(next) {
			t.Errorf("ingesting_media should transition to %q", next)
		}
	}
	if DraftAwaitingMedia.CanTransitionTo(DraftCompleted) {
		t.Fatal("draft must be claimed for ingestion before completion")
	}
	if DraftCompleted.CanTransitionTo(DraftExpired) {
		t.Fatal("completed draft must be terminal")
	}
	if DraftAwaitingMedia.CanTransitionTo(DraftAwaitingMedia) {
		t.Fatal("same-state transition must be rejected")
	}
}

func TestIngestingDraftRemainsActive(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	draft, err := NewDraft(NewDraftParams{ComposeTokenHash: make([]byte, 32), SenderID: 1, RecipientID: 2, SourceChatID: -1, CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	draft.State = DraftIngestingMedia
	if !draft.IsActive(now) {
		t.Fatal("claimed ingestion should count as an active draft")
	}
	if err := draft.Validate(); !errors.Is(err, ErrInvalidIngestLease) {
		t.Fatalf("unleased ingestion validation error = %v", err)
	}
	startedAt := now
	leaseUntil := now.Add(time.Minute)
	draft.IngestStartedAt = &startedAt
	draft.IngestLeaseUntil = &leaseUntil
	if err := draft.Validate(); err != nil {
		t.Fatalf("leased ingestion validation error = %v", err)
	}
}
