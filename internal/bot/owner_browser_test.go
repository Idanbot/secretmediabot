package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/idan/secretmediabot/internal/domain"
	"github.com/idan/secretmediabot/internal/service"
	"github.com/idan/secretmediabot/internal/telegram"
)

func TestOwnerStateParsesCurrentAndLegacyCallbacks(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  ownerListQuery
		valid bool
	}{
		{
			name:  "current all",
			state: encodeOwnerState(ownerListQuery{Limit: 12, Offset: 24}),
			want:  ownerListQuery{Limit: 12, Offset: 24},
			valid: true,
		},
		{
			name:  "current media",
			state: encodeOwnerState(ownerListQuery{Limit: 8, Offset: 16, MediaFilter: "recording"}),
			want: ownerListQuery{
				Limit: 8, Offset: 16, MediaFilter: "recording",
				MediaTypes: []domain.MediaType{domain.MediaVoice, domain.MediaAudio},
			},
			valid: true,
		},
		{
			name:  "current sender",
			state: encodeOwnerState(ownerListQuery{Limit: 7, Offset: 14, SenderID: int64Pointer(202)}),
			want:  ownerListQuery{Limit: 7, Offset: 14, SenderID: int64Pointer(202)},
			valid: true,
		},
		{name: "legacy all", state: "ac", want: ownerListQuery{Limit: ownerPageSize, Offset: 12}, valid: true},
		{
			name:  "legacy media",
			state: "mic",
			want: ownerListQuery{
				Limit: ownerPageSize, Offset: 12, MediaFilter: "image",
				MediaTypes: []domain.MediaType{domain.MediaPhoto},
			},
			valid: true,
		},
		{name: "legacy sender", state: "s5.c", want: ownerListQuery{Limit: ownerPageSize, Offset: 12, SenderID: int64Pointer(5)}, valid: true},
		{name: "missing payload", state: "a", valid: false},
		{name: "zero page size", state: "a0.0", valid: false},
		{name: "negative offset", state: "a5.-1", valid: false},
		{name: "too many page parts", state: "a5.0.extra", valid: false},
		{name: "unknown media", state: "mx5.0", valid: false},
		{name: "zero sender", state: "s0.5.0", valid: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseOwnerState(test.state)
			if !test.valid {
				if err == nil {
					t.Fatalf("parseOwnerState(%q) error = nil", test.state)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOwnerState(%q) error = %v", test.state, err)
			}
			if got.Limit != test.want.Limit || got.Offset != test.want.Offset ||
				got.MediaFilter != test.want.MediaFilter || !equalMediaTypes(got.MediaTypes, test.want.MediaTypes) ||
				!equalInt64Pointer(got.SenderID, test.want.SenderID) {
				t.Fatalf("parseOwnerState(%q) = %#v, want %#v", test.state, got, test.want)
			}
		})
	}
}

func TestOwnerUsernameNavigationUsesStableSenderID(t *testing.T) {
	query := ownerListQuery{Limit: ownerPageSize, SenderUsername: "alice_1"}
	details := []domain.OwnerWhisper{{Whisper: domain.Whisper{SenderID: 202}}}

	navigation := ownerListNavigationQuery(query, details)
	if query.SenderID != nil || query.SenderUsername != "alice_1" {
		t.Fatalf("input query was mutated: %#v", query)
	}
	if navigation.SenderID == nil || *navigation.SenderID != 202 || navigation.SenderUsername != "" {
		t.Fatalf("navigation query = %#v", navigation)
	}

	roundTrip, err := parseOwnerState(encodeOwnerState(navigation))
	if err != nil {
		t.Fatalf("parseOwnerState() error = %v", err)
	}
	if roundTrip.SenderID == nil || *roundTrip.SenderID != 202 || roundTrip.Limit != ownerPageSize {
		t.Fatalf("round-trip navigation query = %#v", roundTrip)
	}
}

func TestOwnerRetentionCommandsValidateAndForward(t *testing.T) {
	whisperID := uuid.New()
	tests := []struct {
		name         string
		command      string
		wantDuration time.Duration
		wantReply    string
		valid        bool
	}{
		{
			name: "retain", command: "/owner_retain " + whisperID.String() + " 48h",
			wantDuration: 48 * time.Hour, wantReply: "Retention deadline updated", valid: true,
		},
		{
			name: "legacy alias", command: "/owner_set_retention " + whisperID.String() + " 72h",
			wantDuration: 72 * time.Hour, wantReply: "Retention deadline updated", valid: true,
		},
		{name: "invalid UUID", command: "/owner_retain not-a-uuid 48h", wantReply: "must be a UUID"},
		{name: "nonpositive duration", command: "/owner_retain " + whisperID.String() + " 0s", wantReply: "positive Go duration"},
		{name: "missing duration", command: "/owner_retain " + whisperID.String(), wantReply: "Usage: /owner_retain"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			useCases := &fakeUseCases{
				isOwner: func(id int64) bool { return id == 101 },
				ownerRetention: func(_ context.Context, ownerID int64, id uuid.UUID, duration time.Duration) error {
					calls++
					if ownerID != 101 || id != whisperID || duration != test.wantDuration {
						t.Fatalf("OwnerSetRetention() args = %d/%s/%s", ownerID, id, duration)
					}
					return nil
				},
			}
			tg := &fakeTelegram{}
			if err := testHandler(useCases, tg).HandleUpdate(context.Background(), privateUpdate(101, test.command)); err != nil {
				t.Fatalf("HandleUpdate() error = %v", err)
			}
			wantCalls := 0
			if test.valid {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("OwnerSetRetention() calls = %d, want %d", calls, wantCalls)
			}
			if len(tg.messages) != 1 || !strings.Contains(tg.messages[0].Text, test.wantReply) {
				t.Fatalf("owner retention reply = %#v", tg.messages)
			}
		})
	}
}

func TestOwnerDetailCallbacksOpenAndRetain(t *testing.T) {
	whisperID := uuid.New()
	plaintext := []byte("owner callback secret")
	var reviewed uuid.UUID
	var retained uuid.UUID
	var retention time.Duration
	useCases := &fakeUseCases{
		isOwner: func(id int64) bool { return id == 101 },
		ownerReview: func(_ context.Context, ownerID int64, id uuid.UUID) (service.OwnerReview, error) {
			if ownerID != 101 {
				t.Fatalf("OwnerReview() owner ID = %d", ownerID)
			}
			reviewed = id
			return service.OwnerReview{Content: service.PlaintextContent{Kind: domain.PayloadText, Text: plaintext}}, nil
		},
		ownerRetention: func(_ context.Context, ownerID int64, id uuid.UUID, duration time.Duration) error {
			if ownerID != 101 {
				t.Fatalf("OwnerSetRetention() owner ID = %d", ownerID)
			}
			retained = id
			retention = duration
			return nil
		},
	}
	tg := &fakeTelegram{}
	h := testHandler(useCases, tg)
	update := telegram.Update{CallbackQuery: &telegram.CallbackQuery{
		ID: "owner-open", From: telegram.User{ID: 101},
		Message: &telegram.Message{MessageID: 77, Chat: telegram.Chat{ID: 101, Type: "private"}},
		Data:    ownerCallbackPrefix + "o:" + compactOwnerUUID(whisperID),
	}}

	if err := h.HandleUpdate(context.Background(), update); err != nil {
		t.Fatalf("HandleUpdate(open callback) error = %v", err)
	}
	if reviewed != whisperID || len(tg.messages) != 1 || tg.messages[0].Text != "owner callback secret" ||
		!tg.messages[0].ProtectContent || len(tg.answers) != 1 || !strings.Contains(tg.answers[0].Text, "sent privately") {
		t.Fatalf("owner open callback = reviewed %s messages %#v answers %#v", reviewed, tg.messages, tg.answers)
	}
	for index, value := range plaintext {
		if value != 0 {
			t.Fatalf("owner callback plaintext[%d] was not zeroed", index)
		}
	}

	tg.messages = nil
	tg.answers = nil
	update.CallbackQuery.ID = "owner-retain"
	update.CallbackQuery.Data = ownerCallbackPrefix + "r:" + compactOwnerUUID(whisperID)
	if err := h.HandleUpdate(context.Background(), update); err != nil {
		t.Fatalf("HandleUpdate(retain callback) error = %v", err)
	}
	if retained != whisperID || retention != ownerDefaultRetain || len(tg.messages) != 0 || len(tg.editedMessages) != 0 ||
		len(tg.answers) != 1 || !strings.Contains(tg.answers[0].Text, "extended by 30 days") {
		t.Fatalf("owner retain callback = retained %s/%s messages %#v edits %#v answers %#v",
			retained, retention, tg.messages, tg.editedMessages, tg.answers)
	}
}
