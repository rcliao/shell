package store

import (
	"testing"
	"time"
)

// Registering the same reminder twice must produce ONE row. This is the class
// that previously created a duplicate recurring schedule which had to be
// disabled by hand.
func TestUpsertScheduleByKey_Idempotent(t *testing.T) {
	s := testStore(t)
	next := time.Now().UTC().Add(time.Hour)

	mk := func() *Schedule {
		return &Schedule{
			ChatID: 42, Label: "daily check", Message: "daily check",
			Schedule: "0 9 * * *", Timezone: "UTC", Type: "cron", Mode: "notify",
			NextRunAt: next, Enabled: true,
		}
	}

	id1, created1, err := s.UpsertScheduleByKey(mk())
	if err != nil {
		t.Fatal(err)
	}
	if !created1 {
		t.Fatal("first registration should report created=true")
	}

	id2, created2, err := s.UpsertScheduleByKey(mk())
	if err != nil {
		t.Fatal(err)
	}
	if created2 {
		t.Error("second registration of an identical payload should report created=false")
	}
	if id2 != id1 {
		t.Errorf("second registration should return the existing row: got #%d, want #%d", id2, id1)
	}

	all, err := s.ListAllSchedules(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("want exactly 1 schedule row after a duplicate registration, got %d", len(all))
	}
	if all[0].DedupKey == "" {
		t.Error("registered row should carry a dedup key")
	}
}

// Cosmetic whitespace differences in the message must still collide — the agent
// re-typing the same reminder is the common duplicate path.
func TestUpsertScheduleByKey_NormalizesMessage(t *testing.T) {
	s := testStore(t)
	next := time.Now().UTC().Add(time.Hour)
	a := &Schedule{ChatID: 7, Label: "l", Message: "take  the   meds", Schedule: "@daily",
		Timezone: "UTC", Type: "cron", Mode: "notify", NextRunAt: next, Enabled: true}
	b := &Schedule{ChatID: 7, Label: "l", Message: "take the meds\n", Schedule: "@daily",
		Timezone: "UTC", Type: "cron", Mode: "notify", NextRunAt: next, Enabled: true}

	id1, _, err := s.UpsertScheduleByKey(a)
	if err != nil {
		t.Fatal(err)
	}
	id2, created, err := s.UpsertScheduleByKey(b)
	if err != nil {
		t.Fatal(err)
	}
	if created || id1 != id2 {
		t.Errorf("whitespace-only differences must collide: id1=%d id2=%d created=%v", id1, id2, created)
	}
}

// A disabled row must not block re-registering the same reminder — otherwise a
// one-shot that already fired would poison its own key forever.
func TestUpsertScheduleByKey_DisabledRowDoesNotBlock(t *testing.T) {
	s := testStore(t)
	next := time.Now().UTC().Add(time.Hour)
	mk := func() *Schedule {
		return &Schedule{ChatID: 9, Label: "once", Message: "ping", Schedule: "21:00",
			Timezone: "UTC", Type: "once", Mode: "notify", NextRunAt: next, Enabled: true}
	}
	id1, _, err := s.UpsertScheduleByKey(mk())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DisableSchedule(id1); err != nil {
		t.Fatal(err)
	}
	id2, created, err := s.UpsertScheduleByKey(mk())
	if err != nil {
		t.Fatal(err)
	}
	if !created || id2 == id1 {
		t.Errorf("a fired-and-disabled one-shot must not block re-creation: id2=%d created=%v", id2, created)
	}
}

// Re-enabling recomputes from NOW forward and clears the auto-pause reason.
// Missed occurrences are never replayed.
func TestEnableScheduleFrom_NoBackfill(t *testing.T) {
	s := testStore(t)
	stale := time.Now().UTC().Add(-72 * time.Hour)
	id, err := s.SaveSchedule(&Schedule{
		ChatID: 5, Label: "watch", Message: "check", Schedule: "@daily", Timezone: "UTC",
		Type: "cron", Mode: "notify", NextRunAt: stale, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PauseSchedule(id, PauseInvalidCron); err != nil {
		t.Fatal(err)
	}

	sc, err := s.GetScheduleByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Enabled || sc.PausedReason != PauseInvalidCron {
		t.Fatalf("auto-pause should disable and record a reason; got enabled=%v reason=%q", sc.Enabled, sc.PausedReason)
	}

	resumeAt := time.Now().UTC().Add(30 * time.Minute)
	if err := s.EnableScheduleFrom(id, resumeAt); err != nil {
		t.Fatal(err)
	}
	sc, err = s.GetScheduleByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if !sc.Enabled {
		t.Error("resume should re-enable the schedule")
	}
	if sc.PausedReason != "" {
		t.Errorf("resume should clear paused_reason, got %q", sc.PausedReason)
	}
	if !sc.NextRunAt.After(time.Now().UTC()) {
		t.Errorf("resume must recompute next_run_at from NOW forward (no backfill), got %s", sc.NextRunAt)
	}
}

// A fire attempt is recorded on EVERY path, and only a successful one stamps
// last_success_at — the reference point for the dead-man's-switch.
func TestRecordJobRun_LedgerAndLastSuccess(t *testing.T) {
	s := testStore(t)
	id, err := s.SaveSchedule(&Schedule{
		ChatID: 3, Label: "job", Message: "m", Schedule: "@hourly", Timezone: "UTC",
		Type: "cron", Mode: "prompt", NextRunAt: time.Now().UTC(), Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.RecordJobRun(JobRun{ScheduleID: id, Outcome: OutcomeSpawnFailed, ErrorMessage: "boom"}); err != nil {
		t.Fatal(err)
	}
	sc, _ := s.GetScheduleByID(id)
	if sc.LastSuccessAt != nil {
		t.Error("a failed fire must not stamp last_success_at")
	}

	if err := s.RecordJobRun(JobRun{ScheduleID: id, Outcome: OutcomeFiredOK}); err != nil {
		t.Fatal(err)
	}
	sc, _ = s.GetScheduleByID(id)
	if sc.LastSuccessAt == nil {
		t.Error("a successful fire must stamp last_success_at")
	}

	runs, err := s.ListJobRuns(id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("want 2 ledger rows, got %d", len(runs))
	}
	if runs[0].Label != "job" {
		t.Errorf("ledger should join the schedule label, got %q", runs[0].Label)
	}
}
