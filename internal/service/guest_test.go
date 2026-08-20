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
}

func (f *fakeGuestStore) CreateGuestRequest(ctx context.Context, params repository.GuestCreateParams) (repository.GuestRequest, error) {
	return params.Request, nil
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
	svc := &Service{
		guestStore: &fakeGuestStore{},
		cipher:     keyring,
		options: Options{
			GuestModeEnabled: true,
			WhisperTTL:       time.Hour,
			ContentRetention: 24 * time.Hour,
		},
		now: time.Now,
	}

	session, err := svc.CreateGuestInlineSecret(context.Background(), CreateGuestInlineParams{
		Sender: domain.User{TelegramUserID: 101, Username: "Alice"},
		Target: command.Target{Kind: command.TargetUsername, Username: "bobby_user"},
		Text:   "Confidential invoice #1049",
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
}

func TestRecordRecentTargetPromotesClaimedStableID(t *testing.T) {
	svc := &Service{recentTargetsCache: make(map[int64][]domain.RecentTarget)}
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
