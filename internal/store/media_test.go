package store

import "testing"

// V2-H19: the media ledger must round-trip archive rows and surface the
// newest photos per chat with the agent's later-added description.
func TestMediaLedger(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	id1, err := s.RecordMedia(-100, 0, 42, "/media/2026-07/a.jpg", "看看這個")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.RecordMedia(-100, 0, 43, "/media/2026-07/b.jpg", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordMedia(555, 0, 1, "/media/2026-07/dm.jpg", "DM photo"); err != nil {
		t.Fatal(err)
	}

	if err := s.SetMediaDescription(id1, "solar panel bracket on fascia"); err != nil {
		t.Fatal(err)
	}

	rows, err := s.RecentMedia(-100, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows for chat -100, want 2 (no cross-chat leak)", len(rows))
	}
	// Newest first.
	if rows[0].ID != id2 || rows[1].ID != id1 {
		t.Fatalf("order wrong: got [%d %d], want [%d %d]", rows[0].ID, rows[1].ID, id2, id1)
	}
	if rows[1].Description != "solar panel bracket on fascia" || rows[1].Caption != "看看這個" {
		t.Fatalf("desc/caption round-trip failed: %+v", rows[1])
	}

	// Limit respected.
	rows, _ = s.RecentMedia(-100, 0, 1)
	if len(rows) != 1 || rows[0].ID != id2 {
		t.Fatalf("limit failed: %+v", rows)
	}
}
