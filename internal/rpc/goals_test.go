package rpc

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/rcliao/shell/internal/judge"
	"github.com/rcliao/shell/internal/memory"
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
		"fast cron":       {"cron": "*/5 * * * *"},
		"weekly at 23":    {"cron": "0 23 * * 0"},
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

func TestGoalScheduleDoneThroughJudge(t *testing.T) {
	s, st := newGoalServer(t)
	mk := func(goal string) int64 {
		_, out := postSchedule(t, s, map[string]any{"goal": goal, "done_criteria": "c", "project": "garden", "cron": "0 9 * * *"})
		return int64(out["id"].(float64))
	}
	var next judge.Verdict
	var nextErr error
	calls := 0
	s.goalJudge = func(_ context.Context, goal, criteria, evidence string) (judge.Verdict, error) {
		calls++
		return next, nextErr
	}
	done := func(id int64, evidence string) map[string]any {
		code, out := postSchedules(t, s, map[string]any{"action": "done", "id": id, "evidence": evidence})
		if code != http.StatusOK {
			t.Fatalf("done: %d %v", code, out)
		}
		return out
	}

	// Empty evidence: not met, and the judge is not called.
	a := mk("a")
	if out := done(a, " "); out["verdict"] != judge.NotMet || calls != 0 {
		t.Errorf("empty evidence: %v (%d calls)", out, calls)
	}
	// Not met: the reason comes back and the goal keeps running.
	next = judge.Verdict{Verdict: judge.NotMet, Reason: "no link"}
	if out := done(a, "B $38"); out["verdict"] != judge.NotMet || !strings.Contains(out["result"].(string), "no link") {
		t.Errorf("not met: %v", out)
	}
	if g, _ := st.GetScheduleGoal(a); g.Status != store.GoalOpen || !g.Enabled {
		t.Errorf("not met closed the goal: %+v", g)
	}
	// Judge failures default to not met; the third in a row asks for the owner.
	next, nextErr = judge.Verdict{Verdict: judge.NotMet, Reason: "judge failed: timeout"}, errors.New("timeout")
	for i := 1; i <= 3; i++ {
		out := done(a, "B $38")
		if out["verdict"] != judge.NotMet {
			t.Fatalf("failure %d: %v", i, out)
		}
		if asks := strings.Contains(out["result"].(string), "tell the owner"); asks != (i == 3) {
			t.Errorf("failure %d: owner ask = %t: %v", i, asks, out["result"])
		}
	}
	// Met: the goal closes as done, the schedule stops, the ledger has it.
	next, nextErr = judge.Verdict{Verdict: judge.Met, Reason: "kit B linked", CostUSD: 0.01}, nil
	if out := done(a, "B $38 https://example.com/b"); out["verdict"] != judge.Met || out["status"] != store.GoalDone {
		t.Errorf("met: %v", out)
	}
	g, _ := st.GetScheduleGoal(a)
	last, _ := st.LastGoalRun(a)
	if g.Status != store.GoalDone || g.Enabled || last.Verdict != judge.Met || last.CostUSD != 0.01 {
		t.Errorf("after met: goal %+v last %+v", g, last)
	}
	if code, _ := postSchedules(t, s, map[string]any{"action": "done", "id": a, "evidence": "again"}); code != http.StatusConflict {
		t.Errorf("done on a closed goal = %d, want 409", code)
	}
	// Impossible closes it too.
	b := mk("b")
	next = judge.Verdict{Verdict: judge.Impossible, Reason: "discontinued"}
	if out := done(b, "the maker stopped selling it"); out["status"] != store.GoalImpossible {
		t.Errorf("impossible: %v", out)
	}
	// No judge configured: not met.
	s.goalJudge = nil
	c := mk("c")
	if out := done(c, "proof"); out["verdict"] != judge.NotMet {
		t.Errorf("no judge: %v", out)
	}
}

func TestGoalLessonsKeptOnClose(t *testing.T) {
	t.Setenv("GHOST_EMBED_PROVIDER", "none")
	s, _ := newGoalServer(t)
	mem, err := memory.New(t.TempDir()+"/mem.db", 2000, nil, 500, nil, 3000,
		map[string]memory.ProfileConfig{"p": {AgentNS: "agent:test"}}, map[int64]string{-100200300: "p"})
	if err != nil {
		t.Fatal(err)
	}
	s.memory = mem
	ctx := context.Background()
	lessons := func() []memory.GoalLesson {
		ls, _ := mem.GoalLessons(ctx, -100200300, "project:garden", 10)
		return ls
	}
	mk := func(goal string, maxRuns int) int64 {
		_, out := postSchedule(t, s, map[string]any{"goal": goal, "done_criteria": "c", "project": "garden", "cron": "0 9 * * *", "max_runs": maxRuns})
		return int64(out["id"].(float64))
	}
	var next judge.Verdict
	s.goalJudge = func(context.Context, string, string, string) (judge.Verdict, error) { return next, nil }

	// Not met: the goal stays open and its lessons are not kept.
	a := mk("a", 5)
	next = judge.Verdict{Verdict: judge.NotMet, Reason: "no link"}
	postSchedules(t, s, map[string]any{"action": "done", "id": a, "evidence": "B", "lessons": "too early"})
	// A progress step that is not the last run keeps none either.
	postSchedules(t, s, map[string]any{"action": "progress", "id": a, "evidence": "B", "lessons": "also too early"})
	if ls := lessons(); len(ls) != 0 {
		t.Fatalf("lessons kept before close: %+v", ls)
	}
	// Met: up to 3 lessons are kept and the result names their keys.
	next = judge.Verdict{Verdict: judge.Met, Reason: "ok"}
	_, out := postSchedules(t, s, map[string]any{"action": "done", "id": a, "evidence": "B $38 link",
		"lessons": "check the maker first\nFridays are cheaper\nskip shop C\na fourth"})
	if ls := lessons(); len(ls) != 3 || !strings.Contains(out["result"].(string), "Kept 3 lesson(s)") {
		t.Fatalf("met: lessons %+v result %v", ls, out["result"])
	}
	// The last run's progress keeps lessons: the goal is about to expire.
	b := mk("b", 1)
	_, out = postSchedules(t, s, map[string]any{"action": "progress", "id": b, "evidence": "nothing under $40", "lessons": "the $40 cap is too low"})
	if ls := lessons(); len(ls) != 4 || !strings.Contains(out["result"].(string), "Kept 1 lesson(s)") {
		t.Errorf("last run: lessons %+v result %v", ls, out["result"])
	}
	// A not-met done on the last run keeps its lessons: the goal expires next.
	c := mk("c", 1)
	next = judge.Verdict{Verdict: judge.NotMet, Reason: "no link"}
	_, out = postSchedules(t, s, map[string]any{"action": "done", "id": c, "evidence": "B", "lessons": "ask for links up front"})
	if ls := lessons(); len(ls) != 5 || !strings.Contains(out["result"].(string), "Kept 1 lesson(s)") {
		t.Errorf("not met on last run: lessons %+v result %v", ls, out["result"])
	}
}
