package store

import (
	"testing"
	"time"
)

// Nearest-rank percentiles over a known set. Written out longhand because an
// off-by-one here silently shifts every future comparison.
func TestPercentileIndexIsNearestRank(t *testing.T) {
	// 10 samples: p50 → index 4 (5th), p95 → index 9 (10th).
	for _, c := range []struct{ n, p, want int }{
		{10, 50, 4},
		{10, 95, 9},
		{1, 50, 0},
		{1, 95, 0},
		{2, 50, 0},
		{2, 95, 1},
		{100, 95, 94},
	} {
		if got := percentileIndex(c.n, c.p); got != c.want {
			t.Errorf("percentileIndex(%d, %d) = %d, want %d", c.n, c.p, got, c.want)
		}
	}
}

func TestSummariseOrdersAndPicks(t *testing.T) {
	got := summarise("total", []int64{50, 10, 30, 20, 40})
	if got.Count != 5 {
		t.Fatalf("count = %d, want 5", got.Count)
	}
	if got.P50 != 30 {
		t.Errorf("p50 = %d, want 30", got.P50)
	}
	if got.Max != 50 {
		t.Errorf("max = %d, want 50", got.Max)
	}
}

func TestSummariseHandlesEmpty(t *testing.T) {
	got := summarise("total", nil)
	if got.Count != 0 || got.P50 != 0 || got.Max != 0 {
		t.Fatalf("empty input produced %+v", got)
	}
}

// Unstamped rows must be excluded, not counted as instant. Including them would
// drag every percentile toward zero and make any later comparison flattering —
// the baseline would claim the turn path was faster than it was.
func TestTurnLatencyExcludesUnstampedRows(t *testing.T) {
	s := testStore(t)
	if err := s.SaveSession(-100200300, 0, "sess-1"); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	sess, err := s.GetSession(-100200300, 0)
	if err != nil || sess == nil {
		t.Fatalf("GetSession: %v", err)
	}

	// Three real turns and two unstamped ones.
	for i, total := range []int64{1000, 2000, 3000} {
		mustMapped(t, s, sess.ID, 100+i, 200+i, 100, 0, 500, total)
	}
	mustMapped(t, s, sess.ID, 300, 400, 0, 0, 0, 0)
	mustMapped(t, s, sess.ID, 301, 401, 0, 0, 0, 0)

	stats, err := s.TurnLatency(24 * time.Hour)
	if err != nil {
		t.Fatalf("TurnLatency: %v", err)
	}
	var total LatencyStats
	for _, st := range stats {
		if st.Metric == "total" {
			total = st
		}
	}
	if total.Count != 3 {
		t.Fatalf("count = %d, want 3 — unstamped rows leaked into the baseline", total.Count)
	}
	if total.P50 != 2000 {
		t.Errorf("p50 = %d, want 2000", total.P50)
	}
}

func mustMapped(t *testing.T, s *Store, sessID int64, userMsgID, botMsgID int, recv, lock, first, total int64) {
	t.Helper()
	if err := s.SaveMessageMap(-100200300, userMsgID, botMsgID, sessID, "u", "b"); err != nil {
		t.Fatalf("SaveMessageMap: %v", err)
	}
	if total == 0 && recv == 0 {
		return // deliberately unstamped
	}
	if err := s.SaveTurnE2E(-100200300, userMsgID, recv, lock, first, total); err != nil {
		t.Fatalf("SaveTurnE2E: %v", err)
	}
}
