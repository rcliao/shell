package store

import (
	"os"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, func()) {
	t.Helper()
	tmp, _ := os.CreateTemp("", "store-topic-test-*.db")
	path := tmp.Name()
	tmp.Close()
	s, err := Open(path)
	if err != nil {
		os.Remove(path)
		t.Fatal(err)
	}
	return s, func() {
		s.Close()
		os.Remove(path)
		os.Remove(path + "-shm")
		os.Remove(path + "-wal")
	}
}

func TestTopicThreadBump(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	// Bump a non-existent thread → creates it.
	tt, err := s.BumpTopicThread(12345, "plants", 100)
	if err != nil {
		t.Fatal(err)
	}
	if tt == nil {
		t.Fatal("expected created thread")
	}
	if tt.TurnCount != 1 {
		t.Errorf("expected turn_count=1, got %d", tt.TurnCount)
	}

	// Bump again → turn_count=2.
	tt, err = s.BumpTopicThread(12345, "plants", 102)
	if err != nil {
		t.Fatal(err)
	}
	if tt.TurnCount != 2 {
		t.Errorf("expected turn_count=2, got %d", tt.TurnCount)
	}

	// Different topic, same chat → separate row.
	tt, err = s.BumpTopicThread(12345, "meals", 103)
	if err != nil {
		t.Fatal(err)
	}
	if tt.TurnCount != 1 {
		t.Errorf("new meals thread expected turn_count=1, got %d", tt.TurnCount)
	}

	// List shows both.
	threads, _ := s.ListTopicThreads(12345)
	if len(threads) != 2 {
		t.Errorf("expected 2 threads, got %d", len(threads))
	}
}

func TestTopicThreadCommitments(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	s.BumpTopicThread(99, "plants", 1)

	// Add an overdue commitment.
	err := s.AddCommitment(99, "plants", Commitment{
		Action: "check leaves on 5/14",
		DueAt:  time.Now().Add(-24 * time.Hour), // 1 day overdue
		Status: "open",
	})
	if err != nil {
		t.Fatal(err)
	}

	// And a not-yet-due one.
	err = s.AddCommitment(99, "plants", Commitment{
		Action: "repot if needed",
		DueAt:  time.Now().Add(7 * 24 * time.Hour),
		Status: "open",
	})
	if err != nil {
		t.Fatal(err)
	}

	overdue, err := s.ListOverdueCommitments(99)
	if err != nil {
		t.Fatal(err)
	}
	if len(overdue) != 1 {
		t.Errorf("expected 1 overdue, got %d", len(overdue))
	}
	if len(overdue) > 0 && overdue[0].Action != "check leaves on 5/14" {
		t.Errorf("wrong commitment surfaced: %q", overdue[0].Action)
	}
}

func TestTopicThreadSetSummary(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	s.BumpTopicThread(7, "plants", 1)

	err := s.SetThreadSummary(7, "plants", "Brazilian wood overwatered; soil check 5/9 still wet; revisit 5/14")
	if err != nil {
		t.Fatal(err)
	}
	tt, _ := s.GetTopicThread(7, "plants")
	if tt.Summary == "" {
		t.Errorf("summary not persisted")
	}
}

func TestTopicTurnLog(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	tt, _ := s.BumpTopicThread(1, "plants", 99)
	err := s.LogTopicTurn(tt.ID, 99, "user", "my plant is dying")
	if err != nil {
		t.Fatal(err)
	}
	// Just smoke-test that no error is returned; log readback is a future helper.
}
