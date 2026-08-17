package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func validWhisperParams(now time.Time) NewWhisperParams {
	media := MediaReference{
		Provider:  MediaProviderTelegram,
		Type:      MediaVoice,
		Ref:       "telegram-file-id",
		UniqueRef: "telegram-unique-id",
	}
	return NewWhisperParams{
		DraftID:       uuid.New(),
		OpenTokenHash: make([]byte, 32),
		SenderID:      101,
		RecipientID:   202,
		SourceChatID:  -100300,
		Content: ContentReference{
			Kind:  PayloadMedia,
			Media: &media,
		},
		CreatedAt: now,
	}
}

func TestNewWhisperSecureDefaults(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	w, err := NewWhisper(validWhisperParams(now))
	if err != nil {
		t.Fatalf("NewWhisper() error = %v", err)
	}
	if w.Status != WhisperActive {
		t.Fatalf("status = %q", w.Status)
	}
	if w.PublishState != PublishPending {
		t.Fatalf("publish state = %q", w.PublishState)
	}
	if !w.OneTime {
		t.Fatal("one-time must default to true")
	}
	if !w.ProtectContent {
		t.Fatal("protect-content must default to true")
	}
	if !w.ExpiresAt.Equal(now.Add(DefaultWhisperTTL)) {
		t.Fatalf("expiry = %v", w.ExpiresAt)
	}
	if w.ContentRetainUntil == nil || !w.ContentRetainUntil.Equal(now.Add(DefaultContentRetention)) {
		t.Fatalf("content retention = %v", w.ContentRetainUntil)
	}
	if w.MetadataRetainUntil == nil || !w.MetadataRetainUntil.Equal(now.Add(DefaultMetadataRetention)) {
		t.Fatalf("metadata retention = %v", w.MetadataRetainUntil)
	}
}

func TestNewWhisperAllowsExplicitReusableMode(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	p := validWhisperParams(now)
	reusable := false
	protect := false
	p.OneTime = &reusable
	p.ProtectContent = &protect
	w, err := NewWhisper(p)
	if err != nil {
		t.Fatalf("NewWhisper() error = %v", err)
	}
	if w.OneTime || w.ProtectContent {
		t.Fatal("explicit false options were not preserved")
	}
}

func TestNewWhisperAcceptsEncryptedTextReference(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	p := validWhisperParams(now)
	textBlobID := uuid.New()
	p.Content = ContentReference{Kind: PayloadText, TextBlobID: &textBlobID}
	w, err := NewWhisper(p)
	if err != nil {
		t.Fatalf("NewWhisper() error = %v", err)
	}
	if w.Content.Kind != PayloadText || w.Content.TextBlobID == nil || *w.Content.TextBlobID != textBlobID {
		t.Fatalf("content = %#v", w.Content)
	}
	// The constructor owns a defensive copy of reference pointers.
	textBlobID = uuid.New()
	if *w.Content.TextBlobID == textBlobID {
		t.Fatal("content reference aliases caller-owned memory")
	}
}

func TestContentReferenceCardinality(t *testing.T) {
	t.Parallel()
	textBlobID := uuid.New()
	captionBlobID := uuid.New()
	mediaBlobID := uuid.New()
	media := MediaReference{
		Provider:    MediaProviderPostgresBlob,
		Type:        MediaPhoto,
		Ref:         "telegram-source-file-id",
		BlobID:      &mediaBlobID,
		ContentType: "image/jpeg",
		SizeBytes:   1024,
	}

	valid := []ContentReference{
		{Kind: PayloadText, TextBlobID: &textBlobID},
		{Kind: PayloadMedia, Media: &media},
		{Kind: PayloadMedia, Media: &media, CaptionBlobID: &captionBlobID},
	}
	for _, content := range valid {
		if err := content.Validate(); err != nil {
			t.Errorf("valid content %#v: %v", content, err)
		}
	}

	invalid := []struct {
		content ContentReference
		want    error
	}{
		{content: ContentReference{}, want: ErrInvalidPayloadKind},
		{content: ContentReference{Kind: PayloadText}, want: ErrInvalidTextBlobID},
		{content: ContentReference{Kind: PayloadText, TextBlobID: &textBlobID, Media: &media}, want: ErrUnexpectedMedia},
		{content: ContentReference{Kind: PayloadText, TextBlobID: &textBlobID, CaptionBlobID: &captionBlobID}, want: ErrUnexpectedCaption},
		{content: ContentReference{Kind: PayloadMedia}, want: ErrMissingMediaReference},
		{content: ContentReference{Kind: PayloadMedia, TextBlobID: &textBlobID, Media: &media}, want: ErrUnexpectedTextBlob},
	}
	for _, testCase := range invalid {
		if err := testCase.content.Validate(); !errors.Is(err, testCase.want) {
			t.Errorf("content %#v error = %v, want %v", testCase.content, err, testCase.want)
		}
	}
}

func TestNewWhisperValidatesTokenHashAndMedia(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	p := validWhisperParams(now)
	p.OpenTokenHash = make([]byte, 31)
	if _, err := NewWhisper(p); !errors.Is(err, ErrInvalidOpenTokenHash) {
		t.Fatalf("hash error = %v", err)
	}

	p = validWhisperParams(now)
	p.Content.Media.Ref = " "
	if _, err := NewWhisper(p); !errors.Is(err, ErrEmptyMediaReference) {
		t.Fatalf("media error = %v", err)
	}
}

func TestWhisperExpiryAndAuthorization(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	w, err := NewWhisper(validWhisperParams(now))
	if err != nil {
		t.Fatal(err)
	}
	w.Status = WhisperActive
	if w.CanAttemptOpen(now) {
		t.Fatal("unpublished whisper must not be openable")
	}
	w.PublishState = PublishPublished
	if !w.CanAttemptOpen(now) || !w.CanRecipientOpen(w.RecipientID, now) {
		t.Fatal("active, unexpired whisper should be openable by its recipient")
	}
	if w.CanRecipientOpen(w.SenderID, now) {
		t.Fatal("sender must not be able to open recipient media")
	}
	if !w.IsParticipant(w.SenderID) || !w.IsParticipant(w.RecipientID) || w.IsParticipant(999) {
		t.Fatal("participant check is incorrect")
	}
	if !w.IsExpired(w.ExpiresAt) || w.CanAttemptOpen(w.ExpiresAt) {
		t.Fatal("whisper must be expired and unopenable at exact expiry")
	}
	w.Status = WhisperOpened
	if w.CanAttemptOpen(now) {
		t.Fatal("opened one-time whisper must not be openable")
	}
}

func TestWhisperValidatesLeasedStates(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	w, err := NewWhisper(validWhisperParams(now))
	if err != nil {
		t.Fatal(err)
	}
	w.PublishState = PublishPublishing
	if err := w.Validate(); !errors.Is(err, ErrInvalidPublishLease) {
		t.Fatalf("unleased publication validation error = %v", err)
	}
	publishLease := now.Add(time.Minute)
	w.PublishLeaseUntil = &publishLease
	if err := w.Validate(); err != nil {
		t.Fatalf("leased publication validation error = %v", err)
	}

	w.PublishState = PublishPublished
	w.PublishLeaseUntil = nil
	w.Status = WhisperOpening
	if err := w.Validate(); !errors.Is(err, ErrInvalidOpeningLease) {
		t.Fatalf("unleased opening validation error = %v", err)
	}
	openingLease := now.Add(time.Minute)
	w.OpeningLeaseUntil = &openingLease
	if err := w.Validate(); err != nil {
		t.Fatalf("leased opening validation error = %v", err)
	}
}

func TestWhisperStatusTransitions(t *testing.T) {
	t.Parallel()
	allowed := map[WhisperStatus][]WhisperStatus{
		WhisperActive:  {WhisperOpening, WhisperExpired, WhisperRevoked},
		WhisperOpening: {WhisperActive, WhisperOpened, WhisperExpired, WhisperRevoked},
	}
	for from, destinations := range allowed {
		for _, to := range destinations {
			if !from.CanTransitionTo(to) {
				t.Errorf("%q should transition to %q", from, to)
			}
		}
	}
	for _, terminal := range []WhisperStatus{WhisperOpened, WhisperExpired, WhisperRevoked} {
		if terminal.CanTransitionTo(WhisperActive) {
			t.Errorf("terminal state %q transitioned to active", terminal)
		}
	}
}

func TestPublishStateTransitions(t *testing.T) {
	t.Parallel()
	allowed := map[PublishState][]PublishState{
		PublishPending:    {PublishPublishing, PublishFailed},
		PublishPublishing: {PublishPublished, PublishRetryWait, PublishFailed},
		PublishRetryWait:  {PublishPublishing, PublishFailed},
		PublishFailed:     {PublishPending},
	}
	for from, destinations := range allowed {
		for _, to := range destinations {
			if !from.CanTransitionTo(to) {
				t.Errorf("%q should transition to %q", from, to)
			}
		}
	}
	if PublishPublished.CanTransitionTo(PublishPublishing) {
		t.Fatal("published state must be terminal")
	}
}

func TestRetentionBoundary(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	w, err := NewWhisper(validWhisperParams(now))
	if err != nil {
		t.Fatal(err)
	}
	if w.ShouldDeleteContent(w.ContentRetainUntil.Add(-time.Nanosecond)) {
		t.Fatal("content should be retained before its deadline")
	}
	if !w.ShouldDeleteContent(*w.ContentRetainUntil) {
		t.Fatal("content should be eligible for deletion at its deadline")
	}
	w.ContentRetainUntil = nil
	if w.ShouldDeleteContent(now.Add(100 * 365 * 24 * time.Hour)) {
		t.Fatal("nil deadline should retain content indefinitely")
	}
}

func TestMediaReferenceValidation(t *testing.T) {
	t.Parallel()
	for _, mediaType := range []MediaType{MediaPhoto, MediaVoice, MediaVideo, MediaAudio, MediaDocument} {
		media := MediaReference{Provider: MediaProviderTelegram, Type: mediaType, Ref: "file-id"}
		if err := media.Validate(); err != nil {
			t.Errorf("%q: %v", mediaType, err)
		}
	}
	if (MediaType("sticker")).IsValid() {
		t.Fatal("unsupported media type was accepted")
	}
	blobID := uuid.New()
	blob := MediaReference{Provider: MediaProviderPostgresBlob, Type: MediaVideo, Ref: "telegram-source-file-id", BlobID: &blobID, ContentType: "video/mp4", SizeBytes: 42}
	if err := blob.Validate(); err != nil {
		t.Fatalf("postgres blob reference: %v", err)
	}
	if err := (MediaReference{Provider: MediaProviderPostgresBlob, Type: MediaVideo}).Validate(); !errors.Is(err, ErrInvalidMediaBlobID) {
		t.Fatalf("missing blob ID error = %v", err)
	}
}

func TestAuditAndOpenEventEnums(t *testing.T) {
	t.Parallel()
	if !OwnerAuditRetrieveContent.IsValid() || !OwnerAuditRetrieveMedia.IsValid() || !OwnerAuditUpdateRetention.IsValid() {
		t.Fatal("expected owner audit actions to be valid")
	}
	if OwnerAuditAction("read_everything").IsValid() {
		t.Fatal("unknown audit action accepted")
	}
	if !OpenAllowed.Allowed() || OpenDeliveryFailed.Allowed() {
		t.Fatal("open outcome allowed classification is incorrect")
	}
	if !OpenDeliveryDelivered.IsValid() || OpenDeliveryState("lost").IsValid() {
		t.Fatal("open delivery-state validation is incorrect")
	}
}
