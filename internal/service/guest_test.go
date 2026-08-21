package service

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/command"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/secretcrypto"
)

func TestGuestPayloadEncryptsAndDecryptsWithRequestBoundAAD(t *testing.T) {
	keyring := newServiceTestKeyring(t)
	service := &Service{cipher: keyring}
	requestID := uuid.New()
	plaintext := []byte("guest secret")
	payload, err := service.encryptGuestPayload(secretcrypto.PurposeText, requestID, plaintext, "text/plain; charset=utf-8", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("encryptGuestPayload() error = %v", err)
	}
	stored := repository.StoredEncryptedPayload{
		ID: payload.ID, EncryptionAlgorithm: payload.EncryptionAlgorithm, EncryptionKeyID: payload.EncryptionKeyID,
		Nonce: payload.Nonce, Ciphertext: payload.Ciphertext, CiphertextSHA256: payload.CiphertextSHA256,
		ContentType: payload.ContentType, PlaintextSize: payload.PlaintextSize, RetainUntil: payload.RetainUntil,
	}
	got, err := service.decryptGuestStored(secretcrypto.PurposeText, requestID, stored)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("decryptGuestStored() = %q, %v", got, err)
	}
	if _, err := service.decryptGuestStored(secretcrypto.PurposeText, uuid.New(), stored); err == nil {
		t.Fatal("decryptGuestStored() accepted a payload under a different request ID")
	}
	secretcrypto.Zero(got)
}

func TestGuestPlaintextContentZeroClearsMemory(t *testing.T) {
	t.Parallel()

	content := GuestPlaintextContent{
		Text:    []byte("sensitive text"),
		Caption: []byte("sensitive caption"),
	}
	content.Zero()
	if content.Text != nil || content.Caption != nil {
		t.Fatal("content.Zero() did not clear text and caption pointers")
	}
}

func TestServiceStructsRedactCallbackTokens(t *testing.T) {
	t.Parallel()

	pub := Publication{
		CallbackData: "raw_callback_secret_token_12345",
	}
	if str := pub.String(); bytes.Contains([]byte(str), []byte("raw_callback_secret_token")) || !bytes.Contains([]byte(str), []byte("[REDACTED]")) {
		t.Fatalf("Publication.String() failed to redact token: %s", str)
	}

	whisper := CreatedWhisper{
		CallbackData: "raw_callback_secret_token_67890",
	}
	if str := whisper.String(); bytes.Contains([]byte(str), []byte("raw_callback_secret_token")) || !bytes.Contains([]byte(str), []byte("[REDACTED]")) {
		t.Fatalf("CreatedWhisper.String() failed to redact token: %s", str)
	}
}

type fakeGuestStore struct {
	GuestStore
	recentTargets  []domain.RecentTarget
	recentCalls    int
	cancelByID     func(context.Context, repository.CancelGuestRequestByIDParams) error
	cancelByIDCall repository.CancelGuestRequestByIDParams
}

func (f *fakeGuestStore) CreateGuestRequest(ctx context.Context, params repository.GuestCreateParams) (repository.GuestRequest, error) {
	return params.Request, nil
}

func (f *fakeGuestStore) FindRecentTargetsForSender(_ context.Context, _ int64, limit int) ([]domain.RecentTarget, error) {
	f.recentCalls++
	if limit <= 0 || limit > len(f.recentTargets) {
		limit = len(f.recentTargets)
	}
	return append([]domain.RecentTarget(nil), f.recentTargets[:limit]...), nil
}

func (f *fakeGuestStore) CancelGuestRequestByID(ctx context.Context, params repository.CancelGuestRequestByIDParams) error {
	f.cancelByIDCall = params
	if f.cancelByID != nil {
		return f.cancelByID(ctx, params)
	}
	return nil
}

func TestCreateGuestRequestRejectsSelfTargeting(t *testing.T) {
	t.Parallel()

	svc := &Service{
		guestStore: &fakeGuestStore{},
		options: Options{
			GuestModeEnabled: true,
		},
		now: time.Now,
	}

	_, err := svc.CreateGuestRequest(context.Background(), CreateGuestRequestParams{
		Sender: domain.User{
			TelegramUserID: 12345,
			Username:       "Alice",
		},
		Target: command.Target{
			Kind:     command.TargetUsername,
			Username: "@alice",
		},
		GuestQueryID: "q1",
	})
	if !errors.Is(err, ErrTargetIsSender) {
		t.Fatalf("expected ErrTargetIsSender, got %v", err)
	}
}

func TestCreateGuestInlineSecret(t *testing.T) {
	t.Parallel()

	keyring := newServiceTestKeyring(t)
	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	svc := &Service{
		guestStore: &fakeGuestStore{},
		cipher:     keyring,
		options: Options{
			GuestModeEnabled: true,
			WhisperTTL:       time.Hour,
			ContentRetention: 24 * time.Hour,
		},
		now: func() time.Time { return now },
	}

	session, err := svc.CreateGuestInlineSecret(context.Background(), CreateGuestInlineParams{
		Sender:        domain.User{TelegramUserID: 101, Username: "Alice"},
		Target:        command.Target{Kind: command.TargetUsername, Username: "bobby_user"},
		Text:          "Confidential invoice #1049",
		InlineQueryID: "inline-query-1",
	})
	if err != nil {
		t.Fatalf("CreateGuestInlineSecret error = %v", err)
	}
	if session.Request.State != repository.GuestStateReady {
		t.Fatalf("expected GuestStateReady, got %v", session.Request.State)
	}
	if session.Request.PayloadKind != domain.PayloadText {
		t.Fatalf("expected domain.PayloadText, got %v", session.Request.PayloadKind)
	}
	if session.Parameter == "" || !strings.HasPrefix(session.Parameter, GuestPrefix) {
		t.Fatalf("expected guest parameter prefix, got %q", session.Parameter)
	}
	if session.Request.InlineQueryID != "inline-query-1" {
		t.Fatalf("inline query ID = %q, want inline-query-1", session.Request.InlineQueryID)
	}
	if want := now.Add(guestInlinePreviewTTL); !session.Request.ExpiresAt.Equal(want) {
		t.Fatalf("preview expiry = %s, want %s", session.Request.ExpiresAt, want)
	}
	if want := now.Add(24 * time.Hour); !session.Request.RetentionDeleteAt.Equal(want) {
		t.Fatalf("retention deletion = %s, want %s", session.Request.RetentionDeleteAt, want)
	}

	_, err = svc.CreateGuestInlineSecret(context.Background(), CreateGuestInlineParams{
		Sender:        domain.User{TelegramUserID: 101, Username: "Alice"},
		Target:        command.Target{Kind: command.TargetUsername, Username: "bobby_user"},
		Text:          "missing query identity",
		InlineQueryID: " ",
	})
	if !errors.Is(err, ErrGuestInvalidRequest) {
		t.Fatalf("missing inline query ID error = %v, want ErrGuestInvalidRequest", err)
	}
}

func TestCancelGuestRequestByIDForwardsIdentityAndMapsNotFound(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	requestID := uuid.New()
	store := &fakeGuestStore{cancelByID: func(context.Context, repository.CancelGuestRequestByIDParams) error {
		return repository.ErrNotFound
	}}
	svc := &Service{
		guestStore: store,
		now:        func() time.Time { return now },
	}

	err := svc.CancelGuestRequestByID(context.Background(), requestID, 101)
	if !errors.Is(err, ErrGuestNotFound) {
		t.Fatalf("CancelGuestRequestByID() error = %v, want ErrGuestNotFound", err)
	}
	if store.cancelByIDCall.RequestID != requestID || store.cancelByIDCall.SenderID != 101 || !store.cancelByIDCall.Now.Equal(now) {
		t.Fatalf("cancel parameters = %+v, want request %s, sender 101, now %s", store.cancelByIDCall, requestID, now)
	}
}

func TestRecordRecentTargetPromotesClaimedStableID(t *testing.T) {
	svc := &Service{recentTargetsCache: make(map[int64]recentTargetsCacheEntry)}
	svc.RecordRecentTarget(101, domain.RecentTarget{
		TargetUsername: "old_username", DisplayName: "@old_username",
	})
	svc.RecordRecentTarget(101, domain.RecentTarget{
		TargetUserID: 202, TargetUsername: "old_username", DisplayName: "Bob",
	})

	targets, err := svc.GetRecentTargets(context.Background(), 101, 1)
	if err != nil {
		t.Fatalf("GetRecentTargets() error = %v", err)
	}
	if len(targets) != 1 || targets[0].TargetUserID != 202 || targets[0].TargetIdentifier() != "202" {
		t.Fatalf("recent targets = %#v, want claimed numeric target", targets)
	}
}

func TestRecentTargetsCacheReturnsCopiesAndExpires(t *testing.T) {
	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	store := &fakeGuestStore{recentTargets: []domain.RecentTarget{{
		TargetUsername: "first_target", DisplayName: "@first_target", LastUsedAt: now,
	}}}
	svc := &Service{
		guestStore:         store,
		now:                func() time.Time { return now },
		recentTargetsCache: make(map[int64]recentTargetsCacheEntry),
	}

	got, err := svc.GetRecentTargets(context.Background(), 101, 3)
	if err != nil {
		t.Fatalf("first GetRecentTargets() error = %v", err)
	}
	if store.recentCalls != 1 || len(got) != 1 {
		t.Fatalf("first cache lookup calls/results = %d/%#v", store.recentCalls, got)
	}
	got[0].DisplayName = "mutated caller copy"

	cached, err := svc.GetRecentTargets(context.Background(), 101, 3)
	if err != nil {
		t.Fatalf("cached GetRecentTargets() error = %v", err)
	}
	if store.recentCalls != 1 || cached[0].DisplayName != "@first_target" {
		t.Fatalf("cached targets = %#v, calls = %d; want an isolated fresh cache copy", cached, store.recentCalls)
	}

	now = now.Add(recentTargetsCacheTTL + time.Second)
	store.recentTargets = []domain.RecentTarget{{
		TargetUsername: "second_target", DisplayName: "@second_target", LastUsedAt: now,
	}}
	fresh, err := svc.GetRecentTargets(context.Background(), 101, 3)
	if err != nil {
		t.Fatalf("expired GetRecentTargets() error = %v", err)
	}
	if store.recentCalls != 2 || len(fresh) != 1 || fresh[0].TargetUsername != "second_target" {
		t.Fatalf("expired cache lookup calls/results = %d/%#v", store.recentCalls, fresh)
	}
}

func TestRecentTargetsCacheBoundsSendersAndResults(t *testing.T) {
	svc := &Service{recentTargetsCache: make(map[int64]recentTargetsCacheEntry)}
	for senderID := int64(1); senderID <= recentTargetsCacheMaxSenders+1; senderID++ {
		svc.RecordRecentTarget(senderID, domain.RecentTarget{
			TargetUserID: senderID + 1000, DisplayName: "bounded",
		})
	}

	svc.mu.RLock()
	cacheSize := len(svc.recentTargetsCache)
	svc.mu.RUnlock()
	if cacheSize > recentTargetsCacheMaxSenders {
		t.Fatalf("recent target cache size = %d, want <= %d", cacheSize, recentTargetsCacheMaxSenders)
	}

	for index := 0; index < recentTargetsPerSender+2; index++ {
		svc.RecordRecentTarget(9000, domain.RecentTarget{
			TargetUsername: strings.Repeat("target", index+1), DisplayName: "bounded",
		})
	}
	svc.mu.RLock()
	targetCount := len(svc.recentTargetsCache[9000].targets)
	svc.mu.RUnlock()
	if targetCount > recentTargetsPerSender {
		t.Fatalf("per-sender recent target count = %d, want <= %d", targetCount, recentTargetsPerSender)
	}
}
