package rpc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/store"
)

func newSchedTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Server{store: st, timezone: "UTC"}, st
}

func postSchedules(t *testing.T, s *Server, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/schedules", bytes.NewReader(b))
	w := httptest.NewRecorder()
	s.handleSchedules(w, req)

	var out map[string]any
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return w.Code, out
}

func seedSchedule(t *testing.T, st *store.Store, label string, next time.Time) int64 {
	t.Helper()
	sched := &store.Schedule{
		ChatID: 42, Label: label, Message: label, Schedule: "0 9 * * *",
		Timezone: "UTC", Type: "cron", Mode: "notify", NextRunAt: next, Enabled: true,
	}
	id, _, err := st.UpsertScheduleByKey(sched)
	if err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	return id
}

// describe is the feedback loop the agent never had: it must surface whether
// the schedule has actually run, not just when it will next fire.
func TestDescribeReportsRunHistory(t *testing.T) {
	s, st := newSchedTestServer(t)
	id := seedSchedule(t, st, "morning briefing", time.Now().Add(time.Hour))

	due := time.Now().Add(-2 * time.Minute).UTC()
	runID, err := st.StartJobRun(store.JobRun{
		ScheduleID: id, ScheduledAt: &due, FiredAt: time.Now().Add(-time.Minute).UTC(), Attempt: 1,
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if err := st.FinishJobRun(runID, store.OutcomeFiredOK, ""); err != nil {
		t.Fatalf("finish run: %v", err)
	}

	code, out := postSchedules(t, s, map[string]any{"action": "describe", "id": id})
	if code != http.StatusOK {
		t.Fatalf("describe returned %d: %v", code, out)
	}
	if out["label"] != "morning briefing" {
		t.Errorf("label = %v", out["label"])
	}
	if out["last_success"] == "never" {
		t.Error("last_success should be set after a successful run")
	}

	runs, ok := out["runs"].([]any)
	if !ok || len(runs) != 1 {
		t.Fatalf("want 1 run in history, got %v", out["runs"])
	}
	run := runs[0].(map[string]any)
	if run["outcome"] != store.OutcomeFiredOK {
		t.Errorf("outcome = %v", run["outcome"])
	}
	// queued_for is the number that exposes a job waiting behind other work.
	if _, has := run["queued_for"]; !has {
		t.Error("run history must report queue delay; that is the whole point of the fired/finished split")
	}
}

// A schedule that has never fired must say so plainly rather than looking
// healthy — silence is the failure mode that motivated the ledger.
func TestDescribeNeverRan(t *testing.T) {
	s, st := newSchedTestServer(t)
	id := seedSchedule(t, st, "never ran", time.Now().Add(time.Hour))

	_, out := postSchedules(t, s, map[string]any{"action": "describe", "id": id})
	if out["last_success"] != "never" {
		t.Errorf("last_success = %v, want \"never\"", out["last_success"])
	}
	if runs, _ := out["runs"].([]any); len(runs) != 0 {
		t.Errorf("want empty run history, got %v", runs)
	}
}

func TestCancelDisablesAndReadsBack(t *testing.T) {
	s, st := newSchedTestServer(t)
	id := seedSchedule(t, st, "cancel me", time.Now().Add(time.Hour))

	code, out := postSchedules(t, s, map[string]any{"action": "cancel", "id": id})
	if code != http.StatusOK {
		t.Fatalf("cancel returned %d: %v", code, out)
	}
	if out["status"] != "cancelled" {
		t.Errorf("status = %v", out["status"])
	}

	after, err := st.GetScheduleByID(id)
	if err != nil || after == nil {
		t.Fatalf("read back: %v", err)
	}
	if after.Enabled {
		t.Error("schedule should be disabled after cancel")
	}
}

// trigger moves the next run into the past so the scheduler picks it up; it
// must not invoke the job itself, since the scheduler owns overlap and the ledger.
func TestTriggerMakesScheduleDue(t *testing.T) {
	s, st := newSchedTestServer(t)
	id := seedSchedule(t, st, "trigger me", time.Now().Add(24*time.Hour))

	code, out := postSchedules(t, s, map[string]any{"action": "trigger", "id": id})
	if code != http.StatusOK {
		t.Fatalf("trigger returned %d: %v", code, out)
	}
	if out["status"] != "queued" {
		t.Errorf("status = %v, want queued", out["status"])
	}

	due, err := st.GetDueSchedules(time.Now().UTC())
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	var found bool
	for _, d := range due {
		if d.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("triggered schedule should be due on the next tick")
	}
}

func TestTriggerRefusesDisabledSchedule(t *testing.T) {
	s, st := newSchedTestServer(t)
	id := seedSchedule(t, st, "paused", time.Now().Add(time.Hour))
	if err := st.PauseSchedule(id, store.PauseInvalidCron); err != nil {
		t.Fatalf("pause: %v", err)
	}

	code, out := postSchedules(t, s, map[string]any{"action": "trigger", "id": id})
	if code != http.StatusBadRequest {
		t.Fatalf("want 400 for a disabled schedule, got %d: %v", code, out)
	}
}

func TestDescribeUnknownIDIsNotFound(t *testing.T) {
	s, _ := newSchedTestServer(t)
	code, _ := postSchedules(t, s, map[string]any{"action": "describe", "id": 9999})
	if code != http.StatusNotFound {
		t.Errorf("want 404 for an unknown schedule, got %d", code)
	}
}

// list defaults to live schedules only; paused rows would otherwise bury the
// answer to "what is actually running?".
func TestListExcludesDisabledByDefault(t *testing.T) {
	s, st := newSchedTestServer(t)
	live := seedSchedule(t, st, "live", time.Now().Add(time.Hour))
	dead := seedSchedule(t, st, "dead", time.Now().Add(2*time.Hour))
	if err := st.DisableSchedule(dead); err != nil {
		t.Fatalf("disable: %v", err)
	}

	_, out := postSchedules(t, s, map[string]any{"action": "list"})
	scheds, _ := out["schedules"].([]any)
	if len(scheds) != 1 {
		t.Fatalf("want only the live schedule, got %v", out["schedules"])
	}
	if int64(scheds[0].(map[string]any)["id"].(float64)) != live {
		t.Errorf("wrong schedule returned: %v", scheds[0])
	}

	_, all := postSchedules(t, s, map[string]any{"action": "list", "all": true})
	if got, _ := all["schedules"].([]any); len(got) != 2 {
		t.Errorf("all=true should include disabled rows, got %d", len(got))
	}
}
