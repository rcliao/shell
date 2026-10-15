package rpc

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

func newGoalServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	s, st := newScheduledProjectServer(t)
	s.quietStart, s.quietEnd = 22, 7
	if _, err := st.CreateProject(store.Project{Slug: "garden", Title: "Garden", ChatID: -100200300, MessageThreadID: 77}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateProject(store.Project{Slug: "old", Title: "Old", ChatID: -100200300, Status: "archived"}); err != nil {
		t.Fatal(err)
	}
	return s, st
}

func TestGoalScheduleCreate(t *testing.T) {
	s, st := newGoalServer(t)
	goal := func(extra map[string]any) (int, map[string]any) {
		body := map[string]any{"goal": "find a drip kit under $40", "done_criteria": "a kit under $40 is in the doc with a link",
			"project": "garden", "cron": "0 9 * * *"}
		for k, v := range extra {
			body[k] = v
		}
		return postSchedule(t, s, body)
	}

	for name, tc := range map[string]map[string]any{
		"no criteria":     {"done_criteria": ""},
		"no project":      {"project": ""},
		"unknown project": {"project": "nope"},
		"archived":        {"project": "old"},
		"no cron":         {"cron": ""},
		"quiet hours":     {"cron": "0 23 * * *"},
		"too many runs":   {"max_runs": 61},
	} {
		if code, out := goal(tc); code != http.StatusBadRequest {
			t.Errorf("%s: %d %v, want 400", name, code, out)
		}
	}
	if code, _ := postSchedule(t, s, map[string]any{"message": "m", "cron": "0 9 * * *", "type": "cron", "chat_id": 42, "done_criteria": "x"}); code != http.StatusBadRequest {
		t.Errorf("done_criteria without goal = %d, want 400", code)
	}

	code, out := goal(nil)
	if code != http.StatusOK || out["created"] != true {
		t.Fatalf("create: %d %v", code, out)
	}
	id := int64(out["id"].(float64))
	g, err := st.GetScheduleGoal(id)
	if err != nil || g == nil {
		t.Fatalf("goal row: %v %v", g, err)
	}
	if g.Status != store.GoalOpen || g.Project != "garden" || g.MaxRuns != store.DefaultGoalMaxRuns || g.ChatID != -100200300 {
		t.Errorf("goal = %+v", g)
	}
	sc, _ := st.GetScheduleByID(id)
	if sc.Mode != scheduler.ModeEvent || sc.DedupKey != project.GoalDedupKey("garden", g.Goal) {
		t.Errorf("schedule = %+v", sc)
	}
	kind, payload, err := scheduler.ParseEventMessage(sc.Message)
	if err != nil || kind != project.EventKind {
		t.Fatalf("envelope: %q %v", kind, err)
	}
	p, err := project.DecodeEventPayload(payload)
	if err != nil || p.Event != project.EventGoalDue || p.ThreadID != 77 || p.GoalKey != sc.DedupKey {
		t.Errorf("payload = %+v %v", p, err)
	}

	// The same goal again returns the running schedule.
	if code, out := goal(nil); code != http.StatusOK || out["created"] != false || int64(out["id"].(float64)) != id {
		t.Errorf("repeat create: %d %v", code, out)
	}
}

func TestGoalScheduleProgressAndCancel(t *testing.T) {
	s, st := newGoalServer(t)
	_, out := postSchedule(t, s, map[string]any{"goal": "g", "done_criteria": "c", "project": "garden", "cron": "0 9 * * *", "max_runs": 3})
	id := int64(out["id"].(float64))
	_, plain := postSchedule(t, s, map[string]any{"message": "m", "cron": "0 9 * * *", "type": "cron", "chat_id": 42})
	plainID := int64(plain["id"].(float64))

	if code, _ := postSchedules(t, s, map[string]any{"action": "progress", "id": plainID, "evidence": "e"}); code != http.StatusBadRequest {
		t.Errorf("progress on a schedule without a goal = %d, want 400", code)
	}
	if code, _ := postSchedules(t, s, map[string]any{"action": "progress", "id": id, "evidence": "  "}); code != http.StatusBadRequest {
		t.Errorf("progress without evidence = %d, want 400", code)
	}
	if code, out := postSchedules(t, s, map[string]any{"action": "progress", "id": id, "evidence": "checked 3 shops", "notes": "best: $45 at A"}); code != http.StatusOK {
		t.Fatalf("progress: %d %v", code, out)
	}
	g, _ := st.GetScheduleGoal(id)
	last, _ := st.LastGoalRun(id)
	if g.Notes != "best: $45 at A" || last == nil || last.Verdict != store.VerdictProgress || last.Evidence != "checked 3 shops" {
		t.Errorf("after progress: goal %+v last %+v", g, last)
	}
	// A progress row is not a fire: last_success_at stays unset.
	if sc, _ := st.GetScheduleByID(id); sc.LastSuccessAt != nil {
		t.Errorf("progress stamped last_success_at")
	}

	_, desc := postSchedules(t, s, map[string]any{"action": "describe", "id": id})
	if gv, ok := desc["goal"].(map[string]any); !ok || gv["status"] != store.GoalOpen || gv["notes"] != "best: $45 at A" {
		t.Errorf("describe goal = %v", desc["goal"])
	}

	if code, out := postSchedules(t, s, map[string]any{"action": "cancel", "id": id}); code != http.StatusOK {
		t.Fatalf("cancel: %d %v", code, out)
	}
	if g, _ := st.GetScheduleGoal(id); g.Status != store.GoalCancelled || g.Enabled {
		t.Errorf("after cancel: %+v", g)
	}
	if code, out := postSchedules(t, s, map[string]any{"action": "progress", "id": id, "evidence": "e"}); code != http.StatusConflict || !strings.Contains(out["error"].(string), "cancelled") {
		t.Errorf("progress on a cancelled goal = %d %v, want 409", code, out)
	}
}
