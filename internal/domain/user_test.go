package domain

import "testing"

func TestUserDisplayName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		user User
		want string
	}{
		{name: "full name", user: User{FirstName: " Alice ", LastName: " Example "}, want: "Alice Example"},
		{name: "first name", user: User{FirstName: "Alice"}, want: "Alice"},
		{name: "username", user: User{Username: "@Alice_Example"}, want: "@Alice_Example"},
		{name: "numeric ID", user: User{TelegramUserID: 12345}, want: "12345"},
		{name: "unknown", user: User{}, want: "Unknown user"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.user.DisplayName(); got != tt.want {
				t.Fatalf("DisplayName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizedUsername(t *testing.T) {
	t.Parallel()
	u := User{Username: " @@Alice_Example "}
	if got := u.NormalizedUsername(); got != "alice_example" {
		t.Fatalf("NormalizedUsername() = %q", got)
	}
}

func TestChatTypeSupportsWhispers(t *testing.T) {
	t.Parallel()
	if !ChatTypeGroup.SupportsWhispers() || !ChatTypeSupergroup.SupportsWhispers() {
		t.Fatal("groups and supergroups must support whispers")
	}
	if ChatTypePrivate.SupportsWhispers() || ChatTypeChannel.SupportsWhispers() {
		t.Fatal("private chats and channels must not support group whispers")
	}
}
