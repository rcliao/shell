package telegram

import (
	"path/filepath"
	"testing"
)

func TestAllowlistStore_AddAndContains(t *testing.T) {
	dir := t.TempDir()
	store := NewAllowlistStore(filepath.Join(dir, "allowlist.json"))

	ok, err := store.Contains(123)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("should not contain user before add")
	}

	err = store.Add(AllowedUser{UserID: 123, Username: "alice", ApprovedAt: ApprovedAt()})
	if err != nil {
		t.Fatal(err)
	}

	ok, err = store.Contains(123)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("should contain user after add")
	}
}

func TestAllowlistStore_Deduplicate(t *testing.T) {
	dir := t.TempDir()
	store := NewAllowlistStore(filepath.Join(dir, "allowlist.json"))

	store.Add(AllowedUser{UserID: 100, ApprovedAt: ApprovedAt()})
	store.Add(AllowedUser{UserID: 100, ApprovedAt: ApprovedAt()})

	users, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Errorf("expected 1 user, got %d", len(users))
	}
}

func TestAllowlistStore_Remove(t *testing.T) {
	dir := t.TempDir()
	store := NewAllowlistStore(filepath.Join(dir, "allowlist.json"))

	store.Add(AllowedUser{UserID: 100, ApprovedAt: ApprovedAt()})
	store.Add(AllowedUser{UserID: 200, ApprovedAt: ApprovedAt()})

	if err := store.Remove(100); err != nil {
		t.Fatal(err)
	}

	ok, _ := store.Contains(100)
	if ok {
		t.Error("user 100 should be removed")
	}
	ok, _ = store.Contains(200)
	if !ok {
		t.Error("user 200 should still exist")
	}
}

func TestAllowlistStore_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	store := NewAllowlistStore(filepath.Join(dir, "nonexistent.json"))

	users, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 0 {
		t.Errorf("expected 0 users for nonexistent file, got %d", len(users))
	}
}
