package service

import (
	"bytes"
	"testing"
	"time"

	"github.com/idan/secretmediabot/internal/secretcrypto"
)

var serviceTestNow = time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)

func TestNewValidatesDependenciesConfigurationAndOwners(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	cipher := newServiceTestKeyring(t)
	if _, err := New(store, cipher, validServiceOptions()); err != nil {
		t.Fatalf("New() valid configuration error = %v", err)
	}

	tests := []struct {
		name   string
		store  Store
		cipher *secretcrypto.Keyring
		mutate func(*Options)
	}{
		{name: "store", cipher: cipher},
		{name: "cipher", store: store},
		{name: "draft TTL", store: store, cipher: cipher, mutate: func(o *Options) { o.DraftTTL = 0 }},
		{name: "whisper TTL", store: store, cipher: cipher, mutate: func(o *Options) { o.WhisperTTL = 0 }},
		{name: "retention", store: store, cipher: cipher, mutate: func(o *Options) { o.ContentRetention = 0 }},
		{name: "ingest lease", store: store, cipher: cipher, mutate: func(o *Options) { o.IngestLease = 0 }},
		{name: "open lease", store: store, cipher: cipher, mutate: func(o *Options) { o.OpenLease = 0 }},
		{name: "publish lease", store: store, cipher: cipher, mutate: func(o *Options) { o.PublishLease = 0 }},
		{name: "delete delay", store: store, cipher: cipher, mutate: func(o *Options) { o.EphemeralDeleteAfter = 0 }},
		{name: "media limit", store: store, cipher: cipher, mutate: func(o *Options) { o.MaxMediaBytes = 0 }},
		{name: "active draft limit", store: store, cipher: cipher, mutate: func(o *Options) { o.MaxActiveDraftsPerUser = 0 }},
		{name: "rate limit", store: store, cipher: cipher, mutate: func(o *Options) { o.MaxWhispersPerUserPerHour = 0 }},
		{name: "missing owner", store: store, cipher: cipher, mutate: func(o *Options) { o.OwnerIDs = nil }},
		{name: "invalid owner", store: store, cipher: cipher, mutate: func(o *Options) { o.OwnerIDs = []int64{0} }},
		{name: "invalid allowed chat", store: store, cipher: cipher, mutate: func(o *Options) { o.AllowedChatIDs = []int64{0} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := validServiceOptions()
			if test.mutate != nil {
				test.mutate(&options)
			}
			if _, err := New(test.store, test.cipher, options); err == nil {
				t.Fatal("New() unexpectedly accepted invalid configuration")
			}
		})
	}
}

func TestNewBuildsOwnerAndChatAuthorizationSets(t *testing.T) {
	t.Parallel()

	options := validServiceOptions()
	options.OwnerIDs = []int64{9001, 9002, 9001}
	options.AllowedChatIDs = []int64{-1001, -1002, -1001}
	service, err := New(newMemoryStore(), newServiceTestKeyring(t), options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if !service.IsOwner(9001) || !service.IsOwner(9002) || service.IsOwner(9003) {
		t.Fatal("owner authorization set does not match configured IDs")
	}
	if !service.chatAllowed(-1001) || !service.chatAllowed(-1002) || service.chatAllowed(-1003) {
		t.Fatal("chat authorization set does not match configured IDs")
	}
}

func validServiceOptions() Options {
	return Options{
		DraftTTL:                  10 * time.Minute,
		WhisperTTL:                24 * time.Hour,
		ContentRetention:          30 * 24 * time.Hour,
		IngestLease:               2 * time.Minute,
		OpenLease:                 30 * time.Second,
		PublishLease:              2 * time.Minute,
		EphemeralDeleteAfter:      30 * time.Second,
		MaxMediaBytes:             20 * 1024 * 1024,
		MaxActiveDraftsPerUser:         1,
		MaxWhispersPerUserPerHour:      30,
		MaxActiveGuestRequestsPerUser:  1,
		MaxGuestRequestsPerUserPerHour: 6,
		GuestModeEnabled:               true,
		DefaultOneTime:            true,
		ProtectContent:            true,
		OwnerIDs:                  []int64{9001},
	}
}

func newServiceTestKeyring(t *testing.T) *secretcrypto.Keyring {
	t.Helper()
	keyring, err := secretcrypto.NewKeyring("test-key", map[string][]byte{
		"test-key": bytes.Repeat([]byte{0x4d}, secretcrypto.KeySize),
	})
	if err != nil {
		t.Fatalf("create test keyring: %v", err)
	}
	return keyring
}

func newTestService(t *testing.T, store *memoryStore, options Options) (*Service, *secretcrypto.Keyring) {
	t.Helper()
	cipher := newServiceTestKeyring(t)
	service, err := New(store, cipher, options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	service.now = func() time.Time { return serviceTestNow }
	return service, cipher
}
