package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/bridge"
	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

// goalFixture makes an active project and a goal schedule for it, the way
// the RPC creates one, and returns the fire's task.
func goalFixture(t *testing.T, maxRuns int) (*store.Store, int64, scheduler.LeasedTask) {
	t.Helper()
	st, _ := newResearchFixture(t)
	if _, err := st.CreateProject(store.Project{Slug: "garden", Title: "Garden", ChatID: -100200300, MessageThreadID: 7, Lang: "zh"}); err != nil {
		t.Fatal(err)
	}
	key := project.GoalDedupKey("garden", "find a drip kit")
	msg, err := project.GoalScheduleMessage("garden", -100200300, 7, key)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := st.UpsertScheduleByKey(&store.Schedule{ChatID: -100200300, Label: "goal", Message: msg, Schedule: "0 9 * * *",
		Timezone: "UTC", Type: "cron", Mode: scheduler.ModeEvent, NextRunAt: time.Now().Add(time.Hour), Enabled: true, DedupKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetScheduleGoal(id, "find a drip kit", "a kit under $40 is in the doc", "garden", maxRuns); err != nil {
		t.Fatal(err)
	}
	_, payload, err := scheduler.ParseEventMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	return st, id, scheduler.LeasedTask{ID: 1, Kind: project.EventKind, Payload: payload, Attempt: 1}
}

func TestGoalFireRunsStepInThread(t *testing.T) {
	st, id, task := goalFixture(t, 2)
	st.SetGoalNotes(id, "checked shop A: $45")
	st.RecordGoalRun(id, store.GoalRun{Outcome: store.VerdictProgress, Verdict: store.VerdictProgress, Reason: "", Evidence: "A $45"})
	var prompts []string
	var out []sent
	deps := projectResearchDeps{
		store: st,
		runTurn: func(_ context.Context, chatID, threadID int64, prompt string) (string, error) {
			if chatID != -100200300 || threadID != 7 {
				t.Errorf("turn ran on %d/%d, want the project's thread", chatID, threadID)
			}
			prompts = append(prompts, prompt)
			return "找到 B 家 $38", nil
		},
		deliver: func(chatID, threadID int64, text string, _ []bridge.LinkButton) {
			out = append(out, sent{chatID, threadID, text})
		},
	}

	res, err := deps.handleProjectEvent(context.Background(), task)
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	if !strings.Contains(res, "goal run 1/2") {
		t.Errorf("result = %q", res)
	}
	p := prompts[0]
	for _, want := range []string{"Goal schedule #", "run 1 of 2", "Goal: find a drip kit", "Done when: a kit under $40 is in the doc",
		"checked shop A: $45", "Last report", `shell_schedule(action="progress"`, "zh"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if len(out) != 1 || out[0].thread != 7 || out[0].text != "找到 B 家 $38" {
		t.Errorf("delivered = %+v", out)
	}
	if g, _ := st.GetScheduleGoal(id); g.Runs != 1 || g.Status != store.GoalOpen {
		t.Errorf("after run 1: %+v", g)
	}

	// The last run: still not done, so the goal expires with one line.
	out = nil
	if _, err := deps.handleProjectEvent(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompts[1], "This is the last run") {
		t.Errorf("last-run prompt lacks the wrap-up line:\n%s", prompts[1])
	}
	g, _ := st.GetScheduleGoal(id)
	if g.Status != store.GoalExpired || g.Enabled {
		t.Errorf("after the last run: %+v", g)
	}
	if len(out) != 2 || !strings.Contains(out[1].text, "跑了 2 次") {
		t.Errorf("expiry line = %+v", out)
	}

	// A closed goal does not run again.
	if res, _ := deps.handleProjectEvent(context.Background(), task); !strings.HasPrefix(res, "skipped") || len(prompts) != 2 {
		t.Errorf("fire after close = %q (%d turns)", res, len(prompts))
	}
}

func TestGoalFireNoopAndInactiveProject(t *testing.T) {
	st, id, task := goalFixture(t, 5)
	turns := 0
	var out []sent
	deps := projectResearchDeps{
		store:   st,
		runTurn: func(context.Context, int64, int64, string) (string, error) { turns++; return "[noop]", nil },
		deliver: func(c, th int64, text string, _ []bridge.LinkButton) { out = append(out, sent{c, th, text}) },
	}
	if _, err := deps.handleProjectEvent(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("[noop] was delivered: %+v", out)
	}
	st.UpdateProjectStatus("garden", "paused")
	if res, _ := deps.handleProjectEvent(context.Background(), task); !strings.HasPrefix(res, "skipped") || turns != 1 {
		t.Errorf("fire for a paused project = %q (%d turns)", res, turns)
	}
	if g, _ := st.GetScheduleGoal(id); g.Runs != 1 {
		t.Errorf("a skipped fire counted as a run: %+v", g)
	}
}
