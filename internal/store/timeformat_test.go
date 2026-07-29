package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestTimestampsAreSQLiteQueryable is the regression the whole change exists
// for: a timestamp written by Go must be visible to SQLite's date functions.
func TestTimestampsAreSQLiteQueryable(t *testing.T) {
	s := openTempStore(t)

	fired := time.Date(2026, 7, 29, 17, 11, 17, 0, time.UTC)
	if err := s.RecordJobRun(JobRun{ScheduleID: 1, Outcome: OutcomeFiredOK, FiredAt: fired}); err != nil {
		t.Fatalf("record: %v", err)
	}

	var got string
	if err := s.db.QueryRow(`SELECT datetime(fired_at) FROM job_runs`).Scan(&got); err != nil {
		t.Fatalf("query: %v", err)
	}
	if got != "2026-07-29 17:11:17" {
		t.Fatalf("datetime(fired_at) = %q, want a parsed timestamp (empty means SQLite could not read the format)", got)
	}

	// The retention shape that silently matched nothing before.
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM job_runs WHERE fired_at < datetime('2026-07-30')`).Scan(&n); err != nil {
		t.Fatalf("range query: %v", err)
	}
	if n != 1 {
		t.Fatalf("range query matched %d rows, want 1", n)
	}
}

func TestMigrateTimeFormatRewritesLegacyRows(t *testing.T) {
	s := openTempStore(t)

	// A row exactly as the pre-fix driver wrote it.
	if _, err := s.db.Exec(
		`INSERT INTO job_runs (schedule_id, trigger_context, fired_at, outcome, error_message)
		 VALUES (7, 'schedule', '2026-07-29 15:01:06.907072 +0000 UTC', 'spawn_failed', '')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var before string
	if err := s.db.QueryRow(`SELECT coalesce(datetime(fired_at),'')  FROM job_runs`).Scan(&before); err != nil {
		t.Fatalf("pre-check: %v", err)
	}
	if before != "" {
		t.Fatalf("legacy row unexpectedly parseable (%q) — the seed no longer reproduces the bug", before)
	}

	if err := s.migrateTimeFormat(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var after string
	if err := s.db.QueryRow(`SELECT datetime(fired_at) FROM job_runs`).Scan(&after); err != nil {
		t.Fatalf("post-query: %v", err)
	}
	if after != "2026-07-29 15:01:06" {
		t.Fatalf("after migrate datetime(fired_at) = %q, want 2026-07-29 15:01:06", after)
	}

	// Go still reads the rewritten value as a real time.
	runs, err := s.ListJobRuns(0, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 1 || !runs[0].FiredAt.Equal(time.Date(2026, 7, 29, 15, 1, 6, 907072000, time.UTC)) {
		t.Fatalf("round-trip lost the value: %+v", runs)
	}

	// Idempotent.
	if err := s.migrateTimeFormat(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var again string
	if err := s.db.QueryRow(`SELECT datetime(fired_at) FROM job_runs`).Scan(&again); err != nil || again != after {
		t.Fatalf("second migrate changed the row: %q -> %q (%v)", after, again, err)
	}
}

// TestLegacyAndNewFormatsOrderTogether guards the mixed-format window: a DB
// migrated in place holds both spellings until every row is rewritten, and
// ORDER BY must still put them in chronological order.
func TestLegacyAndNewFormatsOrderTogether(t *testing.T) {
	s := openTempStore(t)

	if _, err := s.db.Exec(
		`INSERT INTO job_runs (schedule_id, trigger_context, fired_at, outcome, error_message) VALUES
		 (1, 'schedule', '2026-07-29 10:00:00 +0000 UTC', 'fired_ok', ''),
		 (1, 'schedule', '2026-07-29 12:00:00+00:00',    'fired_ok', ''),
		 (1, 'schedule', '2026-07-29 11:00:00 +0000 UTC', 'fired_ok', '')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rows, err := s.db.Query(`SELECT substr(fired_at, 12, 5) FROM job_runs ORDER BY fired_at`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, v)
	}
	want := []string{"10:00", "11:00", "12:00"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("mixed-format ordering = %v, want %v", got, want)
		}
	}
}

func openTempStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
