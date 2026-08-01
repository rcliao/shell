package store

import (
	"path/filepath"
	"testing"
	"time"
)

func undeliveredTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// UndeliveredSince is the drain path's second barrier: it must see a turn that
// was received but not yet answered.
func TestUndeliveredSinceCountsInFlight(t *testing.T) {
	s := undeliveredTestStore(t)

	if _, err := s.BeginPendingTurn(1, 0, 100, "someone", "hi"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	n, err := s.UndeliveredSince(5 * time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v, want 1", n, err)
	}

	if err := s.CompletePendingTurn(1, 100); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if n, err = s.UndeliveredSince(5 * time.Minute); err != nil || n != 0 {
		t.Fatalf("after delivery n=%d err=%v, want 0", n, err)
	}
}

// The age bound is load-bearing. Both live agents carry undone rows from two
// weeks ago; without it every deploy would wait out the full drain timeout.
func TestUndeliveredSinceIgnoresStaleRows(t *testing.T) {
	s := undeliveredTestStore(t)

	if _, err := s.BeginPendingTurn(1, 0, 101, "someone", "ancient"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE pending_turns SET created_at = ? WHERE telegram_msg_id = 101`,
		time.Now().UTC().Add(-24*time.Hour)); err != nil {
		t.Fatalf("age row: %v", err)
	}

	n, err := s.UndeliveredSince(5 * time.Minute)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v — a day-old undone row must not block a restart", n, err)
	}
	// Still visible to a wide-enough window, so nothing is silently lost.
	if n, err = s.UndeliveredSince(48 * time.Hour); err != nil || n != 1 {
		t.Fatalf("wide window n=%d err=%v, want 1", n, err)
	}
}
