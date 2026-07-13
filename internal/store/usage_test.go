package store

import (
	"math"
	"testing"
)

// The Claude CLI reports cost as a cumulative session total; LogUsage must
// store per-exchange deltas so SUM(cost_usd) is true spend.
func TestLogUsageStoresCostDelta(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	log := func(sessID int64, cumulative float64) {
		t.Helper()
		if err := s.LogUsage(1, sessID, 10, 5, 0, 0, cumulative, 1, "interactive", "claude-opus-4-8", 0, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
	}

	log(100, 1.00) // first exchange: delta = 1.00
	log(100, 2.50) // delta = 1.50
	log(100, 2.50) // cached/no-cost turn: delta = 0
	log(200, 0.30) // separate session unaffected by session 100's total
	log(100, 0.20) // CLI restart mid-session: running total reset, value IS the cost

	rows, err := s.db.Query(`SELECT session_id, cost_usd, cost_usd_total FROM usage ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	want := []struct {
		sess  int64
		delta float64
		total float64
	}{
		{100, 1.00, 1.00},
		{100, 1.50, 2.50},
		{100, 0.00, 2.50},
		{200, 0.30, 0.30},
		{100, 0.20, 0.20},
	}
	i := 0
	for rows.Next() {
		var sess int64
		var delta, total float64
		if err := rows.Scan(&sess, &delta, &total); err != nil {
			t.Fatal(err)
		}
		if i >= len(want) {
			t.Fatalf("more rows than expected")
		}
		w := want[i]
		if sess != w.sess || math.Abs(delta-w.delta) > 1e-9 || math.Abs(total-w.total) > 1e-9 {
			t.Errorf("row %d: got (sess=%d delta=%.2f total=%.2f), want (%d %.2f %.2f)",
				i, sess, delta, total, w.sess, w.delta, w.total)
		}
		i++
	}
	if i != len(want) {
		t.Fatalf("got %d rows, want %d", i, len(want))
	}
}

// V2-H18: per-turn phase timings must persist so the >60s long tail can be
// attributed from the ledger.
func TestLogUsageStoresTimings(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	if err := s.LogUsage(1, 100, 10, 5, 0, 0, 0.5, 1, "interactive", "claude-opus-4-8", 1200, 8500, 72000, 4200); err != nil {
		t.Fatal(err)
	}
	var queue, ttft, dur int64
	if err := s.db.QueryRow(`SELECT queue_ms, ttft_ms, duration_ms FROM usage WHERE session_id = 100`).
		Scan(&queue, &ttft, &dur); err != nil {
		t.Fatal(err)
	}
	if queue != 1200 || ttft != 8500 || dur != 72000 {
		t.Fatalf("got queue=%d ttft=%d dur=%d", queue, ttft, dur)
	}
}
