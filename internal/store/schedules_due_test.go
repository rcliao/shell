package store

import (
	"testing"
	"time"
)

// Regression for V2-H51: a schedule row whose next_run_at was written in
// ISO-T format ("2026-07-22T16:00:00") never fired, because the old SQL
// `next_run_at <= ?` compared DATETIME values as strings and 'T' sorts
// after ' '. The due check now happens in Go on the parsed time, so rows
// are due regardless of which string layout they were stored with.
func TestGetDueSchedules_MixedTimestampFormats(t *testing.T) {
	s := testStore(t)

	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)

	// Row written the normal way (driver serializes time.Time).
	if _, err := s.SaveSchedule(&Schedule{
		ChatID: 1, Label: "driver-format-due", Message: "m",
		Schedule: past.Format(time.RFC3339), Timezone: "UTC",
		Type: "once", Mode: "notify", NextRunAt: past, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Row written with a raw ISO-T string literal — the format that
	// silently never matched the old SQL string comparison.
	if _, err := s.db.Exec(`
		INSERT INTO schedules (chat_id, label, message, schedule, timezone, type, mode, next_run_at, enabled)
		VALUES (2, 'isot-format-due', 'm', ?, 'UTC', 'once', 'notify', ?, 1)`,
		past.Format(time.RFC3339), past.Format("2006-01-02T15:04:05"),
	); err != nil {
		t.Fatal(err)
	}

	// Not-due row: must be excluded by the Go-side comparison.
	if _, err := s.SaveSchedule(&Schedule{
		ChatID: 3, Label: "future-not-due", Message: "m",
		Schedule: future.Format(time.RFC3339), Timezone: "UTC",
		Type: "once", Mode: "notify", NextRunAt: future, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	due, err := s.GetDueSchedules(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range due {
		got[d.Label] = true
	}
	if !got["driver-format-due"] {
		t.Error("driver-format row not returned as due")
	}
	if !got["isot-format-due"] {
		t.Error("ISO-T-format row not returned as due (V2-H51 regression)")
	}
	if got["future-not-due"] {
		t.Error("future row incorrectly returned as due")
	}
	if len(due) != 2 {
		t.Errorf("want 2 due schedules, got %d", len(due))
	}
}
