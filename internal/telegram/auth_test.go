package telegram

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAuth_ConfigUserAlwaysAllowed(t *testing.T) {
	auth := NewAuth(AuthOptions{
		DMPolicy:    DMPolicyAllowlist,
		ConfigUsers: []int64{100, 200},
	})

	result := auth.Check(SenderInfo{UserID: 100, ChatID: 1})
	if result != AuthAllowed {
		t.Error("config user should be allowed")
	}
	result = auth.Check(SenderInfo{UserID: 300, ChatID: 1})
	if result != AuthDenied {
		t.Error("non-config user should be denied")
	}
}

func TestAuth_FailClosed(t *testing.T) {
	// Empty config + allowlist policy = deny all.
	auth := NewAuth(AuthOptions{
		DMPolicy:    DMPolicyAllowlist,
		ConfigUsers: []int64{},
	})

	result := auth.Check(SenderInfo{UserID: 100, ChatID: 1})
	if result != AuthDenied {
		t.Error("empty allowlist should deny (fail-closed)")
	}
}

func TestAuth_PairingMode(t *testing.T) {
	dir := t.TempDir()
	allowlist := NewAllowlistStore(filepath.Join(dir, "allowlist.json"))
	pairing := NewPairingManager(allowlist, filepath.Join(dir, "pairing.json"), 10*time.Minute)

	auth := NewAuth(AuthOptions{
		DMPolicy:       DMPolicyPairing,
		ConfigUsers:    []int64{},
		AllowlistStore: allowlist,
		Pairing:        pairing,
		Limiter:        NewRateLimiter(5, time.Minute),
	})

	// Unknown user gets pairing result.
	result := auth.Check(SenderInfo{UserID: 100, ChatID: 1})
	if result != AuthPairing {
		t.Errorf("expected AuthPairing, got %d", result)
	}

	// After adding to allowlist, user should be allowed.
	allowlist.Add(AllowedUser{UserID: 100, ApprovedAt: ApprovedAt()})
	result = auth.Check(SenderInfo{UserID: 100, ChatID: 1})
	if result != AuthAllowed {
		t.Error("allowlisted user should be allowed")
	}
}

func TestAuth_DMDisabled(t *testing.T) {
	auth := NewAuth(AuthOptions{
		DMPolicy:    DMPolicyDisabled,
		ConfigUsers: []int64{100},
	})

	// Config user still allowed even when DMs disabled.
	result := auth.Check(SenderInfo{UserID: 100, ChatID: 1})
	if result != AuthAllowed {
		t.Error("config user should bypass disabled DM policy")
	}

	result = auth.Check(SenderInfo{UserID: 200, ChatID: 1})
	if result != AuthDenied {
		t.Error("non-config user should be denied when DMs disabled")
	}
}

func TestAuth_GroupPolicyDisabled(t *testing.T) {
	auth := NewAuth(AuthOptions{
		DMPolicy:    DMPolicyAllowlist,
		GroupPolicy: GroupPolicyDisabled,
		ConfigUsers: []int64{100},
	})

	// Config user allowed in group.
	result := auth.Check(SenderInfo{UserID: 100, ChatID: 1, IsGroup: true})
	if result != AuthAllowed {
		t.Error("config user should be allowed in group")
	}

	// Non-config user denied in group when disabled.
	result = auth.Check(SenderInfo{UserID: 200, ChatID: 1, IsGroup: true})
	if result != AuthDenied {
		t.Error("non-config user should be denied in disabled group")
	}
}

func TestAuth_GroupPolicyAllowlist(t *testing.T) {
	dir := t.TempDir()
	allowlist := NewAllowlistStore(filepath.Join(dir, "allowlist.json"))

	auth := NewAuth(AuthOptions{
		DMPolicy:          DMPolicyAllowlist,
		GroupPolicy:       GroupPolicyAllowlist,
		ConfigUsers:       []int64{},
		GroupAllowedUsers: []int64{300},
		AllowlistStore:    allowlist,
	})

	result := auth.Check(SenderInfo{UserID: 300, ChatID: 1, IsGroup: true})
	if result != AuthAllowed {
		t.Error("group-allowed user should be allowed")
	}

	result = auth.Check(SenderInfo{UserID: 400, ChatID: 1, IsGroup: true})
	if result != AuthDenied {
		t.Error("non-group-allowed user should be denied")
	}

	// Dynamic allowlist users should work in groups too.
	allowlist.Add(AllowedUser{UserID: 500, ApprovedAt: ApprovedAt()})
	result = auth.Check(SenderInfo{UserID: 500, ChatID: 1, IsGroup: true})
	if result != AuthAllowed {
		t.Error("dynamically allowlisted user should be allowed in group")
	}
}

func TestAuth_RateLimiting(t *testing.T) {
	auth := NewAuth(AuthOptions{
		DMPolicy:    DMPolicyPairing,
		ConfigUsers: []int64{},
		Limiter:     NewRateLimiter(2, time.Minute),
	})

	// First 2 attempts should return pairing.
	auth.Check(SenderInfo{UserID: 100, ChatID: 1})
	auth.Check(SenderInfo{UserID: 100, ChatID: 1})

	// 3rd should be rate limited.
	result := auth.Check(SenderInfo{UserID: 100, ChatID: 1})
	if result != AuthRateLimited {
		t.Errorf("expected AuthRateLimited, got %d", result)
	}
}

func TestAuth_GroupPolicyPairing(t *testing.T) {
	dir := t.TempDir()
	allowlist := NewAllowlistStore(filepath.Join(dir, "allowlist.json"))
	pairing := NewPairingManager(allowlist, filepath.Join(dir, "pairing.json"), 10*time.Minute)

	auth := NewAuth(AuthOptions{
		DMPolicy:       DMPolicyAllowlist,
		GroupPolicy:    GroupPolicyPairing,
		ConfigUsers:    []int64{},
		AllowlistStore: allowlist,
		Pairing:        pairing,
		Limiter:        NewRateLimiter(5, time.Minute),
	})

	// Unknown group user gets pairing result.
	result := auth.Check(SenderInfo{UserID: 100, ChatID: 1, IsGroup: true})
	if result != AuthPairing {
		t.Errorf("expected AuthPairing for group, got %d", result)
	}

	// After adding to allowlist, user should be allowed.
	allowlist.Add(AllowedUser{UserID: 100, ApprovedAt: ApprovedAt()})
	result = auth.Check(SenderInfo{UserID: 100, ChatID: 1, IsGroup: true})
	if result != AuthAllowed {
		t.Error("allowlisted user should be allowed in group")
	}
}

func TestAuth_DefaultPolicies(t *testing.T) {
	// When no policies specified, defaults should apply.
	auth := NewAuth(AuthOptions{
		ConfigUsers: []int64{100},
	})

	// DM default: allowlist.
	result := auth.Check(SenderInfo{UserID: 200, ChatID: 1})
	if result != AuthDenied {
		t.Error("default DM policy should deny unknown users")
	}

	// Group default: disabled.
	result = auth.Check(SenderInfo{UserID: 200, ChatID: 1, IsGroup: true})
	if result != AuthDenied {
		t.Error("default group policy should deny")
	}
}
