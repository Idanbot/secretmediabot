package repository_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/repository"
	"github.com/idan/secretmediabot/internal/secretcrypto"
)

const integrationCleanupSQL = `
TRUNCATE TABLE
    guest_private_delete_jobs,
    guest_secret_payloads,
    guest_secret_requests,
    owner_audit_events,
    processed_updates,
    ephemeral_delete_jobs,
    whisper_open_events,
    encrypted_callback_tokens,
    encrypted_text_payloads,
    media_blobs,
    whispers,
    whisper_drafts,
    observed_chat_members,
    chats,
    users
RESTART IDENTITY CASCADE`

type postgresTest struct {
	store *repository.Store
	db    *repository.Database
	url   string
	now   time.Time
}

func TestPostgresRepositoryIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping PostgreSQL integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := repository.Open(ctx, repository.DatabaseOptions{
		URL:             databaseURL,
		MaxOpenConns:    16,
		MinIdleConns:    2,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("run embedded migrations: %v", err)
	}

	tests := []struct {
		name string
		run  func(*testing.T, postgresTest)
	}{
		{name: "recipient lookup stays within the source chat", run: testObservedLookupIsChatScoped},
		{name: "one active draft survives concurrent creation and direct constraint bypass", run: testOneActiveDraftConcurrency},
		{name: "finalization stores exactly one encrypted payload shape and rolls back atomically", run: testFinalizeEncryptedCardinalityAndRollback},
		{name: "one-time open reservation allows exactly one concurrent recipient", run: testReserveOneTimeOpenConcurrency},
		{name: "completed delivery enqueues a durable ephemeral deletion", run: testCompleteOpenDurablyEnqueuesDeletion},
		{name: "guest request claims target, stores content, and enqueues private deletion", run: testGuestRequestLifecycle},
		{name: "inline preview cancellation is sender-scoped and idempotently guarded", run: testCancelGuestRequestByID},
		{name: "recent targets coalesce stable IDs across username hints", run: testRecentTargetsCoalesceStableIDs},
		{name: "retention cleanup removes whisper metadata and encrypted children", run: testRetentionCleanupCascades},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			truncateIntegrationTables(t, database)
			test.run(t, postgresTest{
				store: repository.NewStore(database),
				db:    database,
				url:   databaseURL,
				now:   time.Now().UTC().Truncate(time.Microsecond),
			})
		})
	}
	truncateIntegrationTables(t, database)
}

func testObservedLookupIsChatScoped(t *testing.T, test postgresTest) {
	t.Helper()
	ctx := context.Background()
	groupA := domain.Chat{TelegramChatID: -10001, Type: domain.ChatTypeSupergroup, Title: "group-a"}
	groupB := domain.Chat{TelegramChatID: -10002, Type: domain.ChatTypeGroup, Title: "group-b"}
	alice := domain.User{TelegramUserID: 101, Username: "Alice"}
	bob := domain.User{TelegramUserID: 102, Username: "CaseSensitiveTarget"}
	charlie := domain.User{TelegramUserID: 103, Username: "CaseSensitiveTarget"}

	observeMembership(t, test.store, alice, groupA, test.now)
	observeMembership(t, test.store, bob, groupA, test.now)
	observeMembership(t, test.store, charlie, groupB, test.now)

	byUsername, err := test.store.FindObservedUserByUsername(ctx, groupA.TelegramChatID, "  @casesensitivetarget ")
	if err != nil {
		t.Fatalf("find group-a recipient by username: %v", err)
	}
	if byUsername.TelegramUserID != bob.TelegramUserID {
		t.Fatalf("username lookup returned user %d, want %d", byUsername.TelegramUserID, bob.TelegramUserID)
	}

	byID, err := test.store.FindObservedUserByID(ctx, groupA.TelegramChatID, bob.TelegramUserID)
	if err != nil {
		t.Fatalf("find group-a recipient by ID: %v", err)
	}
	if byID.TelegramUserID != bob.TelegramUserID {
		t.Fatalf("ID lookup returned user %d, want %d", byID.TelegramUserID, bob.TelegramUserID)
	}

	if _, err := test.store.FindObservedUserByID(ctx, groupA.TelegramChatID, charlie.TelegramUserID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("cross-chat ID lookup error = %v, want ErrNotFound", err)
	}
	if _, err := test.store.FindObservedUserByUsername(ctx, groupB.TelegramChatID, alice.Username); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("cross-chat username lookup error = %v, want ErrNotFound", err)
	}
}

func testGuestRequestLifecycle(t *testing.T, test postgresTest) {
	t.Helper()
	ctx := context.Background()
	sender := domain.User{TelegramUserID: 501, Username: "sender"}
	target := domain.User{TelegramUserID: 502, Username: "target"}
	chat := domain.Chat{TelegramChatID: -10501, Type: domain.ChatTypeSupergroup, Title: "guest"}
	targetID := target.TelegramUserID
	now := test.now
	request := repository.GuestRequest{
		ID: uuid.New(), TokenHash: digest("guest-token"), SenderID: sender.TelegramUserID, TargetUserID: &targetID,
		SourceChatID: &chat.TelegramChatID, GuestQueryID: "guest-query", State: repository.GuestStateAwaitingSecret,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour), RetentionDeleteAt: now.Add(24 * time.Hour),
	}
	created, err := test.store.CreateGuestRequest(ctx, repository.GuestCreateParams{Request: request, Sender: sender, Chat: &chat, Now: now})
	if err != nil {
		t.Fatalf("create guest request: %v", err)
	}
	if created.ID != request.ID {
		t.Fatalf("created request ID = %s, want %s", created.ID, request.ID)
	}
	claimed, err := test.store.ClaimGuestTarget(ctx, repository.GuestClaimTargetParams{TokenHash: request.TokenHash, User: target, Now: now.Add(time.Second)})
	if err != nil {
		t.Fatalf("claim guest target: %v", err)
	}
	if claimed.TargetUserID == nil || *claimed.TargetUserID != target.TelegramUserID {
		t.Fatalf("claimed target = %#v", claimed.TargetUserID)
	}
	lease := now.Add(2 * time.Minute)
	claim, err := test.store.ClaimGuestIngest(ctx, repository.GuestClaimIngestParams{
		TokenHash: request.TokenHash, SenderID: sender.TelegramUserID, Now: now.Add(2 * time.Second), LeaseUntil: lease,
	})
	if err != nil {
		t.Fatalf("claim guest ingest: %v", err)
	}
	payloadID := uuid.New()
	ciphertext := []byte("ciphertext")
	payload := repository.GuestPayload{
		ID: payloadID, RequestID: request.ID, Purpose: "text", EncryptionAlgorithm: "AES-256-GCM",
		EncryptionKeyID: "integration", Nonce: bytes.Repeat([]byte{0x01}, secretcrypto.NonceSize),
		Ciphertext: ciphertext, CiphertextSHA256: digest(string(ciphertext)), PlaintextSize: int64(len(ciphertext)),
		RetainUntil: now.Add(24 * time.Hour),
	}
	if err := test.store.FinalizeGuest(ctx, repository.GuestFinalizeParams{
		RequestID: request.ID, SenderID: sender.TelegramUserID, ExpectedLeaseUntil: *claim.IngestLeaseUntil,
		Kind: domain.PayloadText, Text: &payload, Now: now.Add(3 * time.Second),
	}); err != nil {
		t.Fatalf("finalize guest request: %v", err)
	}
	openLease := now.Add(4 * time.Minute)
	reservation, err := test.store.ClaimGuestOpen(ctx, repository.GuestClaimOpenParams{
		TokenHash: request.TokenHash, User: target, Now: now.Add(4 * time.Second), LeaseUntil: openLease,
	})
	if err != nil {
		t.Fatalf("claim guest open: %v", err)
	}
	if reservation.Content.Text == nil || reservation.Content.Kind != domain.PayloadText {
		t.Fatalf("guest delivery content = %#v", reservation.Content)
	}
	if err := test.store.CompleteGuestOpen(ctx, repository.GuestCompleteOpenParams{
		RequestID: request.ID, ExpectedLeaseUntil: *reservation.Request.OpeningLeaseUntil, MessageID: 700,
		DeleteAt: now.Add(35 * time.Second), Now: now.Add(5 * time.Second),
	}); err != nil {
		t.Fatalf("complete guest open: %v", err)
	}
	job, err := test.store.ClaimDueGuestDelete(ctx, repository.ClaimGuestDeleteParams{Now: now.Add(time.Minute), LeaseUntil: now.Add(2 * time.Minute)})
	if err != nil {
		t.Fatalf("claim guest delete: %v", err)
	}
	if job.ChatID != target.TelegramUserID || job.MessageID != 700 {
		t.Fatalf("guest delete job = %#v", job)
	}
	if err := test.store.MarkGuestDeleted(ctx, repository.FinishGuestDeleteParams{JobID: job.ID, ExpectedLeaseUntil: job.LeaseUntil, Now: now.Add(time.Minute)}); err != nil {
		t.Fatalf("mark guest deleted: %v", err)
	}
}

func testRecentTargetsCoalesceStableIDs(t *testing.T, test postgresTest) {
	t.Helper()
	ctx := context.Background()
	sender := domain.User{TelegramUserID: 551, Username: "recent_sender"}
	targetID := int64(552)
	now := test.now

	newRequest := func(queryID, token, username string) repository.GuestRequest {
		return repository.GuestRequest{
			ID: uuid.New(), TokenHash: digest(token), SenderID: sender.TelegramUserID,
			TargetUserID: &targetID, TargetUsername: username, GuestQueryID: queryID,
			State: repository.GuestStateAwaitingSecret, CreatedAt: now, UpdatedAt: now,
			ExpiresAt: now.Add(time.Hour), RetentionDeleteAt: now.Add(24 * time.Hour),
		}
	}
	first := newRequest("recent-query-old", "recent-token-old", "old_hint")
	if _, err := test.store.CreateGuestRequest(ctx, repository.GuestCreateParams{
		Request: first, Sender: sender, Now: now,
	}); err != nil {
		t.Fatalf("create first recent-target request: %v", err)
	}
	if err := test.db.GORM().Table("guest_secret_requests").Where("id = ?", first.ID).
		Update("state", repository.GuestStateCancelled).Error; err != nil {
		t.Fatalf("retire first recent-target request: %v", err)
	}

	second := newRequest("recent-query-new", "recent-token-new", "new_hint")
	second.CreatedAt = now.Add(time.Second)
	second.UpdatedAt = second.CreatedAt
	second.ExpiresAt = second.CreatedAt.Add(time.Hour)
	second.RetentionDeleteAt = second.CreatedAt.Add(24 * time.Hour)
	if _, err := test.store.CreateGuestRequest(ctx, repository.GuestCreateParams{
		Request: second, Sender: sender, Now: now,
	}); err != nil {
		t.Fatalf("create second recent-target request: %v", err)
	}

	results, err := test.store.FindRecentTargetsForSender(ctx, sender.TelegramUserID, 10)
	if err != nil {
		t.Fatalf("find recent targets: %v", err)
	}
	if len(results) != 1 || results[0].TargetUserID != targetID {
		t.Fatalf("recent targets = %#v, want one stable target ID %d", results, targetID)
	}
}

func testCancelGuestRequestByID(t *testing.T, test postgresTest) {
	t.Helper()
	ctx := context.Background()
	sender := domain.User{TelegramUserID: 561, Username: "cancel_sender"}
	targetID := int64(562)
	now := test.now
	readyAt := now
	request := repository.GuestRequest{
		ID: uuid.New(), TokenHash: digest("cancel-inline-token"), SenderID: sender.TelegramUserID,
		TargetUserID: &targetID, InlineQueryID: "cancel-inline-query", State: repository.GuestStateReady,
		PayloadKind: domain.PayloadText, CreatedAt: now, UpdatedAt: now,
		SecretReadyAt: &readyAt, ExpiresAt: now.Add(time.Hour), RetentionDeleteAt: now.Add(24 * time.Hour),
	}
	payload := repository.GuestPayload{
		ID: uuid.New(), RequestID: request.ID, Purpose: "text", EncryptionAlgorithm: "AES-256-GCM",
		EncryptionKeyID: "integration", Nonce: bytes.Repeat([]byte{0x02}, secretcrypto.NonceSize),
		Ciphertext: []byte("ciphertext"), CiphertextSHA256: digest("ciphertext"), PlaintextSize: 11,
		RetainUntil: now.Add(24 * time.Hour),
	}
	if _, err := test.store.CreateGuestRequest(ctx, repository.GuestCreateParams{
		Request: request, Sender: sender, TextPayload: &payload, Now: now,
	}); err != nil {
		t.Fatalf("create inline preview request: %v", err)
	}
	if err := test.store.CancelGuestRequestByID(ctx, repository.CancelGuestRequestByIDParams{
		RequestID: request.ID, SenderID: sender.TelegramUserID + 1, Now: now,
	}); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("wrong-sender cancellation error = %v, want ErrNotFound", err)
	}
	if err := test.store.CancelGuestRequestByID(ctx, repository.CancelGuestRequestByIDParams{
		RequestID: request.ID, SenderID: sender.TelegramUserID, Now: now,
	}); err != nil {
		t.Fatalf("cancel inline preview: %v", err)
	}
	if err := test.store.CancelGuestRequestByID(ctx, repository.CancelGuestRequestByIDParams{
		RequestID: request.ID, SenderID: sender.TelegramUserID, Now: now,
	}); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("repeat cancellation error = %v, want ErrNotFound", err)
	}
	reloaded, err := test.store.FindGuestRequestByTokenHash(ctx, request.TokenHash)
	if err != nil {
		t.Fatalf("reload cancelled inline preview: %v", err)
	}
	if reloaded.State != repository.GuestStateCancelled {
		t.Fatalf("cancelled preview state = %q, want %q", reloaded.State, repository.GuestStateCancelled)
	}
}

func testOneActiveDraftConcurrency(t *testing.T, test postgresTest) {
	t.Helper()
	ctx := context.Background()
	chat := domain.Chat{TelegramChatID: -20001, Type: domain.ChatTypeSupergroup, Title: "drafts"}
	sender := domain.User{TelegramUserID: 201, Username: "sender"}
	recipient := domain.User{TelegramUserID: 202, Username: "recipient"}
	observeMembership(t, test.store, sender, chat, test.now)
	observeMembership(t, test.store, recipient, chat, test.now)

	drafts := []domain.Draft{
		newDraft(t, sender.TelegramUserID, recipient.TelegramUserID, chat.TelegramChatID, test.now, "concurrent-a", 10*time.Minute),
		newDraft(t, sender.TelegramUserID, recipient.TelegramUserID, chat.TelegramChatID, test.now.Add(time.Microsecond), "concurrent-b", 11*time.Minute),
	}
	start := make(chan struct{})
	results := make(chan error, len(drafts))
	var workers sync.WaitGroup
	for _, draft := range drafts {
		draft := draft
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := test.store.CreateDraft(ctx, repository.CreateDraftParams{
				Draft:               draft,
				ComposeTokenHash:    draft.ComposeTokenHash,
				Now:                 test.now,
				MaxActiveDrafts:     1,
				RecentWhispersSince: test.now.Add(-time.Hour),
				MaxRecentWhispers:   30,
			})
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	succeeded := 0
	rejected := 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, repository.ErrTooManyActiveDrafts), errors.Is(err, repository.ErrConflict):
			rejected++
		default:
			t.Fatalf("concurrent CreateDraft() error = %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("concurrent draft results = %d succeeded, %d rejected; want 1 and 1", succeeded, rejected)
	}

	active, err := test.store.CountActiveDrafts(ctx, sender.TelegramUserID, test.now)
	if err != nil {
		t.Fatalf("count active drafts: %v", err)
	}
	if active != 1 {
		t.Fatalf("active draft count = %d, want 1", active)
	}

	bypassID := uuid.New()
	bypassHash := digest("constraint-bypass")
	err = test.db.GORM().WithContext(ctx).Exec(`
        INSERT INTO whisper_drafts (
            id, compose_token_hash, sender_id, recipient_id, source_chat_id,
            state, created_at, updated_at, expires_at
        ) VALUES (?, ?, ?, ?, ?, 'awaiting_media', ?, ?, ?)`,
		bypassID, bypassHash, sender.TelegramUserID, recipient.TelegramUserID, chat.TelegramChatID,
		test.now.Add(2*time.Microsecond), test.now.Add(2*time.Microsecond), test.now.Add(20*time.Minute)).Error
	if err == nil {
		t.Fatal("database constraint allowed a second active draft for one sender")
	}
}

func testFinalizeEncryptedCardinalityAndRollback(t *testing.T, test postgresTest) {
	t.Helper()
	ctx := context.Background()
	chat := domain.Chat{TelegramChatID: -30001, Type: domain.ChatTypeSupergroup, Title: "finalize"}
	sender := domain.User{TelegramUserID: 301, Username: "sender"}
	recipient := domain.User{TelegramUserID: 302, Username: "recipient"}
	observeMembership(t, test.store, sender, chat, test.now)
	observeMembership(t, test.store, recipient, chat, test.now)

	textDraft, textLease := createAndClaimDraft(t, test.store, sender.TelegramUserID, recipient.TelegramUserID, chat.TelegramChatID, test.now, "text")
	textBlobID := uuid.New()
	textWhisper := newWhisper(t, textDraft, test.now, "text", domain.ContentReference{
		Kind:       domain.PayloadText,
		TextBlobID: uuidPointer(textBlobID),
	})
	callback := encryptedInput(t, secretcrypto.PurposeCallback, uuid.New(), textWhisper.ID, []byte("callback-token-text"), "")
	textPayload := encryptedInput(t, secretcrypto.PurposeText, textBlobID, textWhisper.ID, []byte("encrypted text secret"), "text/plain")
	if _, err := test.store.FinalizeDraft(ctx, repository.FinalizeDraftParams{
		DraftID:            textDraft.ID,
		SenderID:           sender.TelegramUserID,
		ExpectedLeaseUntil: textLease,
		Whisper:            textWhisper,
		CallbackToken:      &callback,
		Text:               &textPayload,
		Now:                test.now.Add(time.Second),
	}); err != nil {
		t.Fatalf("finalize text draft: %v", err)
	}
	assertRowCount(t, test.db, "whispers", "id = ?", 1, textWhisper.ID)
	assertRowCount(t, test.db, "encrypted_callback_tokens", "whisper_id = ?", 1, textWhisper.ID)
	assertRowCount(t, test.db, "encrypted_text_payloads", "whisper_id = ? AND purpose = 'text'", 1, textWhisper.ID)
	assertRowCount(t, test.db, "encrypted_text_payloads", "whisper_id = ? AND purpose = 'caption'", 0, textWhisper.ID)
	assertRowCount(t, test.db, "media_blobs", "whisper_id = ?", 0, textWhisper.ID)
	assertCiphertextIsEncrypted(t, test.db, "encrypted_text_payloads", textBlobID, []byte("encrypted text secret"))
	ownerID := int64(900001)
	textContent, err := test.store.OwnerFetchEncryptedContent(ctx, repository.OwnerGetWhisperParams{
		OwnerTelegramUserID: ownerID, WhisperID: textWhisper.ID, Reason: "integration_audit",
	})
	if err != nil || textContent.Text == nil || textContent.Media != nil {
		t.Fatalf("owner fetch text content = %#v, %v", textContent, err)
	}
	assertLatestOwnerAuditAction(t, test.db, ownerID, textWhisper.ID, "retrieve_content")

	mediaDraft, mediaLease := createAndClaimDraft(t, test.store, sender.TelegramUserID, recipient.TelegramUserID, chat.TelegramChatID, test.now.Add(2*time.Second), "media")
	mediaBlobID := uuid.New()
	captionBlobID := uuid.New()
	mediaWhisper := newWhisper(t, mediaDraft, test.now.Add(2*time.Second), "media", domain.ContentReference{
		Kind: domain.PayloadMedia,
		Media: &domain.MediaReference{
			Provider:    domain.MediaProviderPostgresBlob,
			Type:        domain.MediaVoice,
			Ref:         "telegram-file-id",
			UniqueRef:   "telegram-file-unique-id",
			BlobID:      uuidPointer(mediaBlobID),
			ContentType: "audio/ogg",
			SizeBytes:   4096,
		},
		CaptionBlobID: uuidPointer(captionBlobID),
	})
	mediaCallback := encryptedInput(t, secretcrypto.PurposeCallback, uuid.New(), mediaWhisper.ID, []byte("callback-token-media"), "")
	mediaPayload := encryptedInput(t, secretcrypto.PurposeMedia, mediaBlobID, mediaWhisper.ID, []byte("encrypted voice bytes"), "audio/ogg")
	captionPayload := encryptedInput(t, secretcrypto.PurposeCaption, captionBlobID, mediaWhisper.ID, []byte("encrypted caption"), "text/plain")
	if _, err := test.store.FinalizeDraft(ctx, repository.FinalizeDraftParams{
		DraftID:              mediaDraft.ID,
		SenderID:             sender.TelegramUserID,
		ExpectedLeaseUntil:   mediaLease,
		Whisper:              mediaWhisper,
		TelegramFileID:       "telegram-file-id",
		TelegramFileUniqueID: "telegram-file-unique-id",
		CallbackToken:        &mediaCallback,
		Media:                &mediaPayload,
		Caption:              &captionPayload,
		Now:                  test.now.Add(3 * time.Second),
	}); err != nil {
		t.Fatalf("finalize media draft: %v", err)
	}
	assertRowCount(t, test.db, "whispers", "id = ?", 1, mediaWhisper.ID)
	assertRowCount(t, test.db, "encrypted_callback_tokens", "whisper_id = ?", 1, mediaWhisper.ID)
	assertRowCount(t, test.db, "media_blobs", "whisper_id = ?", 1, mediaWhisper.ID)
	assertRowCount(t, test.db, "encrypted_text_payloads", "whisper_id = ? AND purpose = 'caption'", 1, mediaWhisper.ID)
	assertRowCount(t, test.db, "encrypted_text_payloads", "whisper_id = ? AND purpose = 'text'", 0, mediaWhisper.ID)
	assertCiphertextIsEncrypted(t, test.db, "media_blobs", mediaBlobID, []byte("encrypted voice bytes"))
	if err := test.db.GORM().Exec(
		"UPDATE media_blobs SET plaintext_size_bytes = ? WHERE id = ?",
		int64(20*1024*1024+1), mediaBlobID,
	).Error; err == nil {
		t.Fatal("media_blobs accepted plaintext_size_bytes above the fixed 20 MiB V1 cap")
	}
	mediaContent, err := test.store.OwnerFetchEncryptedContent(ctx, repository.OwnerGetWhisperParams{
		OwnerTelegramUserID: ownerID, WhisperID: mediaWhisper.ID, Reason: "integration_audit",
	})
	if err != nil || mediaContent.Media == nil || mediaContent.Text != nil {
		t.Fatalf("owner fetch media content = %#v, %v", mediaContent, err)
	}
	assertLatestOwnerAuditAction(t, test.db, ownerID, mediaWhisper.ID, "retrieve_media")

	firstPage, err := test.store.OwnerListWhispers(ctx, repository.OwnerListWhispersParams{
		OwnerTelegramUserID: ownerID, Limit: 1, Offset: 0, Reason: "pagination_test",
	})
	if err != nil || len(firstPage) != 1 || firstPage[0].ID != mediaWhisper.ID {
		t.Fatalf("owner first page = %#v, %v; want media whisper", firstPage, err)
	}
	secondPage, err := test.store.OwnerListWhispers(ctx, repository.OwnerListWhispersParams{
		OwnerTelegramUserID: ownerID, Limit: 1, Offset: 1, Reason: "pagination_test",
	})
	if err != nil || len(secondPage) != 1 || secondPage[0].ID != textWhisper.ID {
		t.Fatalf("owner second page = %#v, %v; want text whisper", secondPage, err)
	}

	rollbackDraft, rollbackLease := createAndClaimDraft(t, test.store, sender.TelegramUserID, recipient.TelegramUserID, chat.TelegramChatID, test.now.Add(4*time.Second), "rollback")
	rollbackWhisper := newWhisper(t, rollbackDraft, test.now.Add(4*time.Second), "rollback", domain.ContentReference{
		Kind:       domain.PayloadText,
		TextBlobID: uuidPointer(textBlobID), // Valid shape, but duplicates the first payload PK in PostgreSQL.
	})
	rollbackCallback := encryptedInput(t, secretcrypto.PurposeCallback, uuid.New(), rollbackWhisper.ID, []byte("callback-token-rollback"), "")
	duplicateText := encryptedInput(t, secretcrypto.PurposeText, textBlobID, rollbackWhisper.ID, []byte("must roll back"), "text/plain")
	_, err = test.store.FinalizeDraft(ctx, repository.FinalizeDraftParams{
		DraftID:            rollbackDraft.ID,
		SenderID:           sender.TelegramUserID,
		ExpectedLeaseUntil: rollbackLease,
		Whisper:            rollbackWhisper,
		CallbackToken:      &rollbackCallback,
		Text:               &duplicateText,
		Now:                test.now.Add(5 * time.Second),
	})
	if !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("FinalizeDraft() duplicate payload error = %v, want ErrConflict", err)
	}
	assertRowCount(t, test.db, "whispers", "id = ?", 0, rollbackWhisper.ID)
	assertRowCount(t, test.db, "encrypted_callback_tokens", "id = ?", 0, rollbackCallback.ID)

	persistedDraft, err := test.store.FindDraftByComposeTokenHash(ctx, rollbackDraft.ComposeTokenHash)
	if err != nil {
		t.Fatalf("reload draft after failed finalization: %v", err)
	}
	if persistedDraft.State != domain.DraftIngestingMedia || persistedDraft.IngestLeaseUntil == nil || !persistedDraft.IngestLeaseUntil.Equal(rollbackLease) {
		t.Fatalf("draft after rollback = state %q lease %v, want ingesting lease %v", persistedDraft.State, persistedDraft.IngestLeaseUntil, rollbackLease)
	}
}

func testReserveOneTimeOpenConcurrency(t *testing.T, test postgresTest) {
	t.Helper()
	ctx := context.Background()
	chat := domain.Chat{TelegramChatID: -40001, Type: domain.ChatTypeSupergroup, Title: "open"}
	sender := domain.User{TelegramUserID: 401, Username: "sender"}
	recipient := domain.User{TelegramUserID: 402, Username: "recipient"}
	observeMembership(t, test.store, sender, chat, test.now)
	observeMembership(t, test.store, recipient, chat, test.now)
	whisper := createPublishedTextWhisper(t, test.store, sender.TelegramUserID, recipient.TelegramUserID, chat.TelegramChatID, test.now, "one-time")

	type reserveResult struct {
		reservation repository.OpenReservation
		err         error
	}
	start := make(chan struct{})
	results := make(chan reserveResult, 2)
	var workers sync.WaitGroup
	for i := 0; i < 2; i++ {
		callbackID := fmt.Sprintf("callback-concurrent-%d", i)
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			reservation, err := test.store.ReserveOpen(ctx, repository.ReserveOpenParams{
				OpenTokenHash:   whisper.OpenTokenHash,
				TelegramUserID:  recipient.TelegramUserID,
				CallbackQueryID: callbackID,
				Now:             test.now.Add(5 * time.Second),
				LeaseUntil:      test.now.Add(35 * time.Second),
			})
			results <- reserveResult{reservation: reservation, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	allowed := 0
	denied := 0
	for result := range results {
		if result.err == nil {
			allowed++
			if result.reservation.Whisper.ID != whisper.ID || result.reservation.EventID <= 0 {
				t.Fatalf("allowed reservation = whisper %s event %d, want whisper %s and a positive event", result.reservation.Whisper.ID, result.reservation.EventID, whisper.ID)
			}
			if result.reservation.Content.Kind != domain.PayloadText || result.reservation.Content.Text == nil {
				t.Fatalf("allowed reservation content = %#v, want encrypted text", result.reservation.Content)
			}
			continue
		}
		if !errors.Is(result.err, repository.ErrNotActive) && !errors.Is(result.err, repository.ErrAlreadyOpened) && !errors.Is(result.err, repository.ErrConflict) {
			t.Fatalf("losing ReserveOpen() error = %v", result.err)
		}
		denied++
	}
	if allowed != 1 || denied != 1 {
		t.Fatalf("concurrent open results = %d allowed, %d denied; want 1 and 1", allowed, denied)
	}
	assertRowCount(t, test.db, "whisper_open_events", "whisper_id = ? AND allowed = TRUE", 1, whisper.ID)
}

func testCompleteOpenDurablyEnqueuesDeletion(t *testing.T, test postgresTest) {
	t.Helper()
	ctx := context.Background()
	chat := domain.Chat{TelegramChatID: -50001, Type: domain.ChatTypeSupergroup, Title: "ephemeral-delete"}
	sender := domain.User{TelegramUserID: 501, Username: "sender"}
	recipient := domain.User{TelegramUserID: 502, Username: "recipient"}
	observeMembership(t, test.store, sender, chat, test.now)
	observeMembership(t, test.store, recipient, chat, test.now)
	whisper := createPublishedTextWhisper(t, test.store, sender.TelegramUserID, recipient.TelegramUserID, chat.TelegramChatID, test.now, "durable-delete")

	callbackID := "callback-durable-delete"
	reservation, err := test.store.ReserveOpen(ctx, repository.ReserveOpenParams{
		OpenTokenHash:   whisper.OpenTokenHash,
		TelegramUserID:  recipient.TelegramUserID,
		CallbackQueryID: callbackID,
		Now:             test.now.Add(5 * time.Second),
		LeaseUntil:      test.now.Add(35 * time.Second),
	})
	if err != nil {
		t.Fatalf("reserve durable-delete fixture: %v", err)
	}
	ephemeralMessageID := int64(777001)
	deleteAt := test.now.Add(30 * time.Second)
	complete := repository.CompleteOpenParams{
		WhisperID:          whisper.ID,
		EventID:            reservation.EventID,
		CallbackQueryID:    callbackID,
		EphemeralMessageID: &ephemeralMessageID,
		DeleteAt:           deleteAt,
		Now:                test.now.Add(6 * time.Second),
	}
	if err := test.store.CompleteOpen(ctx, complete); err != nil {
		t.Fatalf("complete open: %v", err)
	}
	complete.Now = test.now.Add(7 * time.Second)
	if err := test.store.CompleteOpen(ctx, complete); err != nil {
		t.Fatalf("idempotent CompleteOpen() retry: %v", err)
	}
	assertRowCount(t, test.db, "ephemeral_delete_jobs", "whisper_id = ? AND ephemeral_message_id = ?", 1, whisper.ID, ephemeralMessageID)
	assertRowCount(t, test.db, "whisper_open_events", "id = ? AND delivery_state = 'delivered'", 1, reservation.EventID)

	differentMessageID := ephemeralMessageID + 1
	complete.EphemeralMessageID = &differentMessageID
	if err := test.store.CompleteOpen(ctx, complete); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("CompleteOpen() mismatched retry error = %v, want ErrConflict", err)
	}

	if _, err := test.store.ClaimDueEphemeralDelete(ctx, repository.ClaimEphemeralDeleteParams{
		Now: test.now.Add(20 * time.Second), LeaseUntil: test.now.Add(40 * time.Second),
	}); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("ClaimDueEphemeralDelete() before deadline error = %v, want ErrNotFound", err)
	}

	reopened, err := repository.Open(ctx, repository.DatabaseOptions{
		URL:             test.url,
		MaxOpenConns:    4,
		MinIdleConns:    1,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: time.Minute,
	})
	if err != nil {
		t.Fatalf("reopen TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened test database: %v", err)
		}
	})
	restartedStore := repository.NewStore(reopened)
	deleteLease := test.now.Add(2 * time.Minute)
	job, err := restartedStore.ClaimDueEphemeralDelete(ctx, repository.ClaimEphemeralDeleteParams{
		Now: test.now.Add(31 * time.Second), LeaseUntil: deleteLease,
	})
	if err != nil {
		t.Fatalf("claim persisted deletion after repository restart: %v", err)
	}
	if job.ChatID != chat.TelegramChatID || job.RecipientID != recipient.TelegramUserID ||
		job.EphemeralMessageID != ephemeralMessageID || job.WhisperID == nil || *job.WhisperID != whisper.ID {
		t.Fatalf("claimed deletion job = %#v, want chat/recipient/message/whisper fixture IDs", job)
	}
	if job.AttemptCount != 1 || !job.LeaseUntil.Equal(deleteLease) {
		t.Fatalf("claimed deletion job attempt/lease = %d/%v, want 1/%v", job.AttemptCount, job.LeaseUntil, deleteLease)
	}
}

func testRetentionCleanupCascades(t *testing.T, test postgresTest) {
	t.Helper()
	ctx := context.Background()
	chat := domain.Chat{TelegramChatID: -60001, Type: domain.ChatTypeSupergroup, Title: "retention"}
	sender := domain.User{TelegramUserID: 601, Username: "sender"}
	recipient := domain.User{TelegramUserID: 602, Username: "recipient"}
	observeMembership(t, test.store, sender, chat, test.now)
	observeMembership(t, test.store, recipient, chat, test.now)
	draft, ingestLease := createAndClaimDraft(t, test.store, sender.TelegramUserID, recipient.TelegramUserID, chat.TelegramChatID, test.now, "retention")

	mediaBlobID := uuid.New()
	captionBlobID := uuid.New()
	retentionDeadline := test.now.Add(10 * time.Minute)
	whisper, err := domain.NewWhisper(domain.NewWhisperParams{
		ID:            uuid.New(),
		DraftID:       draft.ID,
		OpenTokenHash: digest("open-retention"),
		SenderID:      sender.TelegramUserID,
		RecipientID:   recipient.TelegramUserID,
		SourceChatID:  chat.TelegramChatID,
		Content: domain.ContentReference{
			Kind: domain.PayloadMedia,
			Media: &domain.MediaReference{
				Provider:    domain.MediaProviderPostgresBlob,
				Type:        domain.MediaPhoto,
				Ref:         "retention-file-id",
				BlobID:      uuidPointer(mediaBlobID),
				ContentType: "image/jpeg",
				SizeBytes:   2048,
			},
			CaptionBlobID: uuidPointer(captionBlobID),
		},
		CreatedAt:           test.now,
		ExpiresAt:           test.now.Add(24 * time.Hour),
		ContentRetainUntil:  &retentionDeadline,
		MetadataRetainUntil: &retentionDeadline,
	})
	if err != nil {
		t.Fatalf("create retention whisper: %v", err)
	}
	callback := encryptedInput(t, secretcrypto.PurposeCallback, uuid.New(), whisper.ID, []byte("callback-token-retention"), "")
	media := encryptedInput(t, secretcrypto.PurposeMedia, mediaBlobID, whisper.ID, []byte("retained-photo"), "image/jpeg")
	caption := encryptedInput(t, secretcrypto.PurposeCaption, captionBlobID, whisper.ID, []byte("retained-caption"), "text/plain")
	if _, err := test.store.FinalizeDraft(ctx, repository.FinalizeDraftParams{
		DraftID:            draft.ID,
		SenderID:           sender.TelegramUserID,
		ExpectedLeaseUntil: ingestLease,
		Whisper:            whisper,
		TelegramFileID:     "retention-file-id",
		CallbackToken:      &callback,
		Media:              &media,
		Caption:            &caption,
		Now:                test.now.Add(time.Second),
	}); err != nil {
		t.Fatalf("finalize retention whisper: %v", err)
	}
	assertRowCount(t, test.db, "media_blobs", "whisper_id = ?", 1, whisper.ID)
	assertRowCount(t, test.db, "encrypted_text_payloads", "whisper_id = ?", 1, whisper.ID)
	assertRowCount(t, test.db, "encrypted_callback_tokens", "whisper_id = ?", 1, whisper.ID)

	cleanupNow := retentionDeadline.Add(time.Microsecond)
	result, err := test.store.RunCleanup(ctx, repository.CleanupParams{
		Now:                    cleanupNow,
		ProcessedUpdatesBefore: cleanupNow.Add(-7 * 24 * time.Hour),
		BatchSize:              100,
	})
	if err != nil {
		t.Fatalf("run retention cleanup: %v", err)
	}
	if result.DeletedWhispers != 1 {
		t.Fatalf("cleanup deleted whispers = %d, want 1", result.DeletedWhispers)
	}
	if _, err := test.store.FindWhisperByOpenTokenHash(ctx, whisper.OpenTokenHash); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("find retained whisper after cleanup error = %v, want ErrNotFound", err)
	}
	assertRowCount(t, test.db, "media_blobs", "whisper_id = ?", 0, whisper.ID)
	assertRowCount(t, test.db, "encrypted_text_payloads", "whisper_id = ?", 0, whisper.ID)
	assertRowCount(t, test.db, "encrypted_callback_tokens", "whisper_id = ?", 0, whisper.ID)

	again, err := test.store.RunCleanup(ctx, repository.CleanupParams{
		Now:                    cleanupNow.Add(time.Second),
		ProcessedUpdatesBefore: cleanupNow.Add(-7 * 24 * time.Hour),
		BatchSize:              100,
	})
	if err != nil {
		t.Fatalf("repeat retention cleanup: %v", err)
	}
	if again.DeletedWhispers != 0 {
		t.Fatalf("repeated cleanup deleted whispers = %d, want 0", again.DeletedWhispers)
	}
}

func observeMembership(t *testing.T, store *repository.Store, user domain.User, chat domain.Chat, seenAt time.Time) {
	t.Helper()
	if err := store.ObserveMembership(context.Background(), repository.ObserveMembershipParams{
		User: user, Chat: chat, SeenAt: seenAt,
	}); err != nil {
		t.Fatalf("observe user %d in chat %d: %v", user.TelegramUserID, chat.TelegramChatID, err)
	}
}

func newDraft(t *testing.T, senderID, recipientID, chatID int64, createdAt time.Time, salt string, ttl time.Duration) domain.Draft {
	t.Helper()
	hash := digest("compose-" + salt)
	draft, err := domain.NewDraft(domain.NewDraftParams{
		ID:               uuid.New(),
		ComposeTokenHash: hash,
		SenderID:         senderID,
		RecipientID:      recipientID,
		SourceChatID:     chatID,
		CreatedAt:        createdAt,
		ExpiresAt:        createdAt.Add(ttl),
	})
	if err != nil {
		t.Fatalf("create draft fixture %q: %v", salt, err)
	}
	return draft
}

func createAndClaimDraft(t *testing.T, store *repository.Store, senderID, recipientID, chatID int64, now time.Time, salt string) (domain.Draft, time.Time) {
	t.Helper()
	draft := newDraft(t, senderID, recipientID, chatID, now, salt, 10*time.Minute)
	created, err := store.CreateDraft(context.Background(), repository.CreateDraftParams{
		Draft:               draft,
		ComposeTokenHash:    draft.ComposeTokenHash,
		Now:                 now,
		MaxActiveDrafts:     1,
		RecentWhispersSince: now.Add(-time.Hour),
		MaxRecentWhispers:   30,
	})
	if err != nil {
		t.Fatalf("persist draft fixture %q: %v", salt, err)
	}
	leaseUntil := now.Add(5 * time.Minute)
	claimed, err := store.ClaimDraftIngest(context.Background(), repository.ClaimDraftIngestParams{
		DraftID: created.ID, SenderID: senderID, Now: now, LeaseUntil: leaseUntil,
	})
	if err != nil {
		t.Fatalf("claim draft fixture %q: %v", salt, err)
	}
	if claimed.State != domain.DraftIngestingMedia {
		t.Fatalf("claimed draft state = %q, want %q", claimed.State, domain.DraftIngestingMedia)
	}
	return claimed, leaseUntil
}

func newWhisper(t *testing.T, draft domain.Draft, createdAt time.Time, salt string, content domain.ContentReference) domain.Whisper {
	t.Helper()
	contentRetention := createdAt.Add(30 * 24 * time.Hour)
	metadataRetention := createdAt.Add(30 * 24 * time.Hour)
	whisper, err := domain.NewWhisper(domain.NewWhisperParams{
		ID:                  uuid.New(),
		DraftID:             draft.ID,
		OpenTokenHash:       digest("open-" + salt),
		SenderID:            draft.SenderID,
		RecipientID:         draft.RecipientID,
		SourceChatID:        draft.SourceChatID,
		SourceThreadID:      draft.SourceThreadID,
		Content:             content,
		CreatedAt:           createdAt,
		ExpiresAt:           createdAt.Add(24 * time.Hour),
		ContentRetainUntil:  &contentRetention,
		MetadataRetainUntil: &metadataRetention,
	})
	if err != nil {
		t.Fatalf("create whisper fixture %q: %v", salt, err)
	}
	return whisper
}

func createPublishedTextWhisper(t *testing.T, store *repository.Store, senderID, recipientID, chatID int64, now time.Time, salt string) domain.Whisper {
	t.Helper()
	draft, ingestLease := createAndClaimDraft(t, store, senderID, recipientID, chatID, now, salt)
	textBlobID := uuid.New()
	whisper := newWhisper(t, draft, now, salt, domain.ContentReference{
		Kind:       domain.PayloadText,
		TextBlobID: uuidPointer(textBlobID),
	})
	callback := encryptedInput(t, secretcrypto.PurposeCallback, uuid.New(), whisper.ID, []byte("callback-token-"+salt), "")
	textPayload := encryptedInput(t, secretcrypto.PurposeText, textBlobID, whisper.ID, []byte("text-secret-"+salt), "text/plain")
	if _, err := store.FinalizeDraft(context.Background(), repository.FinalizeDraftParams{
		DraftID:            draft.ID,
		SenderID:           senderID,
		ExpectedLeaseUntil: ingestLease,
		Whisper:            whisper,
		CallbackToken:      &callback,
		Text:               &textPayload,
		Now:                now.Add(time.Second),
	}); err != nil {
		t.Fatalf("finalize published fixture %q: %v", salt, err)
	}
	publishLease := now.Add(time.Minute)
	claim, err := store.ClaimPublish(context.Background(), repository.ClaimPublishParams{
		WhisperID:  whisper.ID,
		Now:        now.Add(2 * time.Second),
		LeaseUntil: publishLease,
	})
	if err != nil {
		t.Fatalf("claim published fixture %q: %v", salt, err)
	}
	if claim.Whisper.ID != whisper.ID || claim.CallbackToken.ID != callback.ID {
		t.Fatalf("publish claim does not match finalized fixture %q", salt)
	}
	if err := store.MarkPublished(context.Background(), repository.MarkPublishedParams{
		WhisperID:          whisper.ID,
		ExpectedLeaseUntil: publishLease,
		PublicMessageID:    int64(9000 + len(salt)),
		Now:                now.Add(3 * time.Second),
	}); err != nil {
		t.Fatalf("mark published fixture %q: %v", salt, err)
	}
	published, err := store.FindWhisperByOpenTokenHash(context.Background(), whisper.OpenTokenHash)
	if err != nil {
		t.Fatalf("reload published fixture %q: %v", salt, err)
	}
	if published.PublishState != domain.PublishPublished {
		t.Fatalf("published fixture state = %q, want %q", published.PublishState, domain.PublishPublished)
	}
	return published
}

func encryptedInput(t *testing.T, purpose secretcrypto.RecordPurpose, id, whisperID uuid.UUID, plaintext []byte, contentType string) repository.EncryptedBlobInput {
	t.Helper()
	keyring, err := secretcrypto.NewKeyring("integration", map[string][]byte{
		"integration": bytes.Repeat([]byte{0x5a}, secretcrypto.KeySize),
	})
	if err != nil {
		t.Fatalf("create integration keyring: %v", err)
	}
	aad, err := secretcrypto.AssociatedData(purpose, id, whisperID)
	if err != nil {
		t.Fatalf("derive %s associated data: %v", purpose, err)
	}
	payload, err := keyring.Encrypt(plaintext, aad)
	if err != nil {
		t.Fatalf("encrypt %s fixture: %v", purpose, err)
	}
	return repository.EncryptedBlobInput{
		ID:            id,
		Payload:       payload,
		ContentType:   contentType,
		PlaintextSize: int64(len(plaintext)),
	}
}

func uuidPointer(value uuid.UUID) *uuid.UUID {
	return &value
}

func digest(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return append([]byte(nil), sum[:]...)
}

func truncateIntegrationTables(t *testing.T, database *repository.Database) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := database.GORM().WithContext(ctx).Exec(integrationCleanupSQL).Error; err != nil {
		t.Fatalf("reset integration test tables: %v", err)
	}
}

func countRows(t *testing.T, database *repository.Database, table, predicate string, args ...any) int64 {
	t.Helper()
	query := fmt.Sprintf("SELECT count(*) FROM %s", table)
	if predicate != "" {
		query += " WHERE " + predicate
	}
	var count int64
	if err := database.GORM().Raw(query, args...).Scan(&count).Error; err != nil {
		t.Fatalf("count %s rows: %v", table, err)
	}
	return count
}

func assertRowCount(t *testing.T, database *repository.Database, table, predicate string, want int64, args ...any) {
	t.Helper()
	if got := countRows(t, database, table, predicate, args...); got != want {
		t.Fatalf("%s row count = %d, want %d", table, got, want)
	}
}

func assertLatestOwnerAuditAction(t *testing.T, database *repository.Database, ownerID int64, whisperID uuid.UUID, want string) {
	t.Helper()
	var row struct {
		Action string `gorm:"column:action"`
	}
	if err := database.GORM().Table("owner_audit_events").Select("action").
		Where("owner_telegram_user_id = ? AND whisper_id = ?", ownerID, whisperID).
		Order("id DESC").Take(&row).Error; err != nil {
		t.Fatalf("read owner audit action: %v", err)
	}
	if row.Action != want {
		t.Fatalf("owner audit action = %q, want %q", row.Action, want)
	}
}

func assertCiphertextIsEncrypted(t *testing.T, database *repository.Database, table string, id uuid.UUID, plaintext []byte) {
	t.Helper()
	var row struct {
		Ciphertext []byte
	}
	query := fmt.Sprintf("SELECT ciphertext FROM %s WHERE id = ?", table)
	if err := database.GORM().Raw(query, id).Take(&row).Error; err != nil {
		t.Fatalf("read %s ciphertext: %v", table, err)
	}
	if len(row.Ciphertext) == 0 {
		t.Fatalf("%s ciphertext is empty", table)
	}
	if bytes.Equal(row.Ciphertext, plaintext) || bytes.Contains(row.Ciphertext, plaintext) {
		t.Fatalf("%s persisted plaintext bytes instead of ciphertext", table)
	}
}
