package telegram

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPairing_RequestAndApprove(t *testing.T) {
	dir := t.TempDir()
	allowlist := NewAllowlistStore(filepath.Join(dir, "allowlist.json"))
	pm := NewPairingManager(allowlist, filepath.Join(dir, "pairing.json"), 10*time.Minute)

	code, err := pm.RequestPairing(123, "alice", "Alice", "Smith", 999)
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 8 {
		t.Errorf("expected 8-char code, got %q", code)
	}

	// Same user should get same code.
	code2, err := pm.RequestPairing(123, "alice", "Alice", "Smith", 999)
	if err != nil {
		t.Fatal(err)
	}
	if code2 != code {
		t.Errorf("expected same code %q, got %q", code, code2)
	}

	// List should show 1 pending.
	pending := pm.List()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}

	// Approve.
	req, err := pm.Approve(code)
	if err != nil {
		t.Fatal(err)
	}
	if req.UserID != 123 {
		t.Errorf("expected user 123, got %d", req.UserID)
	}

	// Pending should be empty.
	pending = pm.List()
	if len(pending) != 0 {
		t.Errorf("expected 0 pending after approve, got %d", len(pending))
	}

	// User should be in allowlist.
	ok, _ := allowlist.Contains(123)
	if !ok {
		t.Error("user should be in allowlist after approval")
	}
}

func TestPairing_Expiry(t *testing.T) {
	dir := t.TempDir()
	allowlist := NewAllowlistStore(filepath.Join(dir, "allowlist.json"))
	pm := NewPairingManager(allowlist, filepath.Join(dir, "pairing.json"), 10*time.Millisecond)

	code, _ := pm.RequestPairing(123, "alice", "Alice", "", 999)
	time.Sleep(20 * time.Millisecond)

	_, err := pm.Approve(code)
	if err == nil {
		t.Error("should fail after code expired")
	}
}

func TestPairing_InvalidCode(t *testing.T) {
	dir := t.TempDir()
	allowlist := NewAllowlistStore(filepath.Join(dir, "allowlist.json"))
	pm := NewPairingManager(allowlist, filepath.Join(dir, "pairing.json"), 10*time.Minute)

	_, err := pm.Approve("NOTACODE")
	if err == nil {
		t.Error("should fail for invalid code")
	}
}

func TestPairing_FilePersistence(t *testing.T) {
	dir := t.TempDir()
	pairingPath := filepath.Join(dir, "pairing.json")
	allowlistPath := filepath.Join(dir, "allowlist.json")
	allowlist := NewAllowlistStore(allowlistPath)

	pm1 := NewPairingManager(allowlist, pairingPath, 10*time.Minute)
	code, _ := pm1.RequestPairing(123, "alice", "Alice", "", 999)

	// Simulate out-of-process approval (CLI).
	req, err := ApproveFromFile(pairingPath, allowlist, code)
	if err != nil {
		t.Fatal(err)
	}
	if req.UserID != 123 {
		t.Errorf("expected user 123, got %d", req.UserID)
	}

	// Verify allowlist was updated.
	ok, _ := allowlist.Contains(123)
	if !ok {
		t.Error("user should be in allowlist after CLI approval")
	}
}
