package store

import (
	"fmt"
	"sort"
	"time"
)

// Turn latency baseline.
//
// Every latency regression this project has had was noticed in production, by
// someone feeling that replies got slower, and then argued about from memory.
// The e2e columns on message_map have recorded the owner-experienced timings
// since V2-H33, but nothing reads them back, so "is this slower than before?"
// has never had an answer.
//
// This is that answer: a distribution over a stated window, so a refactor can
// be judged against what the turn path actually did beforehand.

// LatencyStats is a percentile summary of one timing column.
type LatencyStats struct {
	Metric string
	Count  int
	P50    int64
	P95    int64
	Max    int64
}

// TurnLatency summarises recent turns.
//
// Zero values are excluded rather than counted as instant: a row written before
// the timing columns existed, or by a path that never stamped them, would
// otherwise drag every percentile toward zero and make any later comparison
// flattering. An honest baseline has to describe only the turns it actually
// measured.
func (s *Store) TurnLatency(window time.Duration) ([]LatencyStats, error) {
	cutoff := time.Now().Add(-window).UTC()
	rows, err := s.db.Query(`
		SELECT e2e_recv_lag_ms, e2e_lock_wait_ms, e2e_first_visible_ms, e2e_total_ms
		FROM message_map
		WHERE created_at >= ? AND e2e_total_ms > 0`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols := map[string][]int64{}
	order := []string{"recv_lag", "lock_wait", "first_visible", "total"}
	for rows.Next() {
		var recv, lock, first, total int64
		if err := rows.Scan(&recv, &lock, &first, &total); err != nil {
			return nil, err
		}
		cols["recv_lag"] = append(cols["recv_lag"], recv)
		cols["lock_wait"] = append(cols["lock_wait"], lock)
		cols["first_visible"] = append(cols["first_visible"], first)
		cols["total"] = append(cols["total"], total)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]LatencyStats, 0, len(order))
	for _, name := range order {
		out = append(out, summarise(name, cols[name]))
	}
	return out, nil
}

// summarise computes count/p50/p95/max over one column.
//
// Nearest-rank percentiles, not interpolated. At the sample sizes here — a few
// hundred turns — an interpolated p95 invents a number no turn ever took, and
// this is meant to be compared against a later measurement rather than to be
// statistically elegant.
func summarise(metric string, v []int64) LatencyStats {
	st := LatencyStats{Metric: metric, Count: len(v)}
	if len(v) == 0 {
		return st
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	st.P50 = v[percentileIndex(len(v), 50)]
	st.P95 = v[percentileIndex(len(v), 95)]
	st.Max = v[len(v)-1]
	return st
}

// percentileIndex returns the nearest-rank index for p in [0,100].
func percentileIndex(n, p int) int {
	if n == 0 {
		return 0
	}
	i := (p*n + 99) / 100 // ceil(p*n/100)
	if i < 1 {
		i = 1
	}
	if i > n {
		i = n
	}
	return i - 1
}

// String renders one row for the CLI.
func (l LatencyStats) String() string {
	return fmt.Sprintf("%-14s n=%-5d p50=%-7s p95=%-7s max=%s",
		l.Metric, l.Count, ms(l.P50), ms(l.P95), ms(l.Max))
}

func ms(v int64) string {
	if v < 1000 {
		return fmt.Sprintf("%dms", v)
	}
	return fmt.Sprintf("%.1fs", float64(v)/1000)
}
