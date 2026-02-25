package telegram

import "testing"

func TestAuth_AllowedUser(t *testing.T) {
	auth := NewAuth([]int64{100, 200})

	if !auth.IsAllowed(100) {
		t.Error("expected 100 to be allowed")
	}
	if !auth.IsAllowed(200) {
		t.Error("expected 200 to be allowed")
	}
	if auth.IsAllowed(300) {
		t.Error("expected 300 to be denied")
	}
}

func TestAuth_EmptyAllowlist(t *testing.T) {
	auth := NewAuth(nil)

	if !auth.IsAllowed(100) {
		t.Error("empty allowlist should allow everyone")
	}
}
