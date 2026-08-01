package store

import (
	"testing"
	"time"
)

// The capture contract: a deep beat's text survives the turn. Before this
// table, heartbeat output was written to `messages` and discarded — the system
// chat's notify path returns early — so the agent's self-audit existed only
// until the turn ended.
func TestRecordAndReadReflection(t *testing.T) {
	s := openTempStore(t)

	id, err := s.RecordReflection(Reflection{
		ChatID:     0,
		Model:      "claude-opus-5",
		Text:       "Reviewed yesterday's replies. I over-explained twice when a short answer was wanted.",
		ToolCalls:  12,
		DurationMS: 154000,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	got, err := s.GetReflection(id)
	if err != nil || got == nil {
		t.Fatalf("get: %v (nil=%v)", err, got == nil)
	}
	if got.ToolCalls != 12 || got.Model != "claude-opus-5" {
		t.Errorf("round-trip lost fields: %+v", got)
	}
	if got.Noop {
		t.Error("a reflection with text must not be marked noop")
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at must be stamped")
	}
}

// An errored or silent deep beat must still leave a row. A missing row and a
// silent beat would otherwise be indistinguishable — the same conflation the
// job_runs ledger exists to prevent for fires.
func TestEmptyReflectionIsRecordedAsNoop(t *testing.T) {
	s := openTempStore(t)

	if _, err := s.RecordReflection(Reflection{ChatID: 0, Text: "", Noop: true}); err != nil {
		t.Fatalf("record: %v", err)
	}
	refs, err := s.ListReflections(10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("want 1 row for a silent beat, got %d", len(refs))
	}
	if !refs[0].Noop {
		t.Error("an empty reflection must be flagged noop, not silently dropped")
	}
}

func TestListReflectionsNewestFirst(t *testing.T) {
	s := openTempStore(t)
	for _, txt := range []string{"first", "second", "third"} {
		if _, err := s.RecordReflection(Reflection{Text: txt}); err != nil {
			t.Fatalf("record %s: %v", txt, err)
		}
	}
	refs, err := s.ListReflections(2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(refs) != 2 || refs[0].Text != "third" || refs[1].Text != "second" {
		t.Fatalf("want newest-first truncation, got %+v", refs)
	}
}

// Timestamps must be queryable by SQLite's date functions — the same trap that
// made the job_runs ledger opaque to range queries.
func TestReflectionTimestampsAreQueryable(t *testing.T) {
	s := openTempStore(t)
	if _, err := s.RecordReflection(Reflection{Text: "x"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM reflections WHERE created_at > datetime('now','-1 hour')`).Scan(&n); err != nil {
		t.Fatalf("range query: %v", err)
	}
	if n != 1 {
		t.Fatalf("range query matched %d rows, want 1 — timestamps are opaque to SQLite", n)
	}
}

// Capture is automatic, so a shortfall against heartbeat fires means the hook
// is missing turns. That is the one way this design fails silently.
func TestReflectionCaptureRateCountsBoth(t *testing.T) {
	s := openTempStore(t)
	since := time.Now().UTC().Add(-time.Hour)

	sched := &Schedule{
		ChatID: 0, Label: "hb", Message: "beat", Schedule: "1h", Timezone: "UTC",
		Type: "heartbeat", Mode: "prompt", NextRunAt: time.Now().UTC(), Enabled: true,
	}
	schedID, _, err := s.UpsertScheduleByKey(sched)
	if err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	runID, err := s.StartJobRun(JobRun{ScheduleID: schedID, FiredAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if err := s.FinishJobRun(runID, OutcomeFiredOK, ""); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	if _, err := s.RecordReflection(Reflection{Text: "captured"}); err != nil {
		t.Fatalf("record: %v", err)
	}

	captured, fires, err := s.ReflectionCaptureRate(since)
	if err != nil {
		t.Fatalf("rate: %v", err)
	}
	if captured != 1 || fires != 1 {
		t.Fatalf("captured=%d fires=%d, want 1 and 1", captured, fires)
	}
}
