package store

import (
	"testing"
	"time"
)

// Explicit dedup keys (P2): a project's research schedule is registered with
// dedup_key "project:<slug>" so it can later be found and disabled/re-enabled
// without storing its row id.

func TestUpsertScheduleHonorsExplicitDedupKey(t *testing.T) {
	s := testStore(t)

	sched := &Schedule{
		ChatID: 42, Label: "project research: demo", Message: `{"kind":"project.event","payload":{}}`,
		Schedule: "0 9 * * 1", Timezone: "UTC", Type: "cron", Mode: "event",
		NextRunAt: time.Now().UTC().Add(time.Hour), Enabled: true,
		DedupKey: "project:demo",
	}
	id, created, err := s.UpsertScheduleByKey(sched)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first registration should create")
	}
	got, err := s.GetScheduleByID(id)
	if err != nil || got == nil {
		t.Fatalf("read back: %v", err)
	}
	if got.DedupKey != "project:demo" {
		t.Errorf("dedup_key = %q, want the explicit key, not a content hash", got.DedupKey)
	}

	// Same explicit key again → existing row, no duplicate.
	again := &Schedule{
		ChatID: 42, Label: "project research: demo", Message: `{"kind":"project.event","payload":{}}`,
		Schedule: "0 9 * * 1", Timezone: "UTC", Type: "cron", Mode: "event",
		NextRunAt: time.Now().UTC().Add(2 * time.Hour), Enabled: true,
		DedupKey: "project:demo",
	}
	id2, created2, err := s.UpsertScheduleByKey(again)
	if err != nil {
		t.Fatal(err)
	}
	if created2 || id2 != id {
		t.Errorf("re-registration: created=%t id=%d, want existing row %d", created2, id2, id)
	}
}

func TestUpsertScheduleStillDerivesKeyWhenUnset(t *testing.T) {
	s := testStore(t)

	sched := &Schedule{
		ChatID: 42, Label: "reminder", Message: "water the plants",
		Schedule: "0 9 * * *", Timezone: "UTC", Type: "cron", Mode: "notify",
		NextRunAt: time.Now().UTC().Add(time.Hour), Enabled: true,
	}
	if _, _, err := s.UpsertScheduleByKey(sched); err != nil {
		t.Fatal(err)
	}
	want := ScheduleDedupKey(42, "cron", "0 9 * * *", "water the plants")
	if sched.DedupKey != want {
		t.Errorf("dedup_key = %q, want derived %q", sched.DedupKey, want)
	}
}

func TestFindScheduleByDedupKey(t *testing.T) {
	s := testStore(t)

	if sc, err := s.FindScheduleByDedupKey("project:missing"); err != nil || sc != nil {
		t.Fatalf("missing key: sc=%v err=%v, want nil/nil", sc, err)
	}

	sched := &Schedule{
		ChatID: 42, Label: "project research: demo", Message: `{"kind":"project.event","payload":{}}`,
		Schedule: "0 9 * * 1", Timezone: "UTC", Type: "cron", Mode: "event",
		NextRunAt: time.Now().UTC().Add(time.Hour), Enabled: true,
		DedupKey: "project:demo",
	}
	id, _, err := s.UpsertScheduleByKey(sched)
	if err != nil {
		t.Fatal(err)
	}

	sc, err := s.FindScheduleByDedupKey("project:demo")
	if err != nil {
		t.Fatal(err)
	}
	if sc == nil || sc.ID != id || !sc.Enabled {
		t.Fatalf("found = %+v, want enabled row %d", sc, id)
	}

	// Disable, then find again: the disabled row is still discoverable (for
	// re-enable on project re-activation).
	if err := s.PauseSchedule(id, "project_archived"); err != nil {
		t.Fatal(err)
	}
	sc, err = s.FindScheduleByDedupKey("project:demo")
	if err != nil {
		t.Fatal(err)
	}
	if sc == nil || sc.ID != id || sc.Enabled || sc.PausedReason != "project_archived" {
		t.Fatalf("found after pause = %+v, want disabled row %d with reason", sc, id)
	}
}
