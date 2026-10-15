package rpc

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/judge"
	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

// Goal schedules (docs/DESIGN-AGENT-LOOPS.md step 2): shell_schedule(create)
// with goal + done_criteria + project makes a project goal — an event-mode
// cron schedule whose fires run one step in the project's thread — and
// shell_schedule(progress) is how each step reports what it did.

// goalMaxRunsCap bounds max_runs: a goal is a bounded loop, not a cron job.
const goalMaxRunsCap = 60

// createGoalSchedule handles POST /schedule with a goal.
func (s *Server) createGoalSchedule(w http.ResponseWriter, req ScheduleRequest) {
	goal, criteria := strings.TrimSpace(req.Goal), strings.TrimSpace(req.DoneCriteria)
	switch {
	case criteria == "":
		writeError(w, http.StatusBadRequest, "a goal needs done_criteria: the check that says it is done")
		return
	case req.Project == "":
		writeError(w, http.StatusBadRequest, "a goal needs project=<slug>: goals run in their project's thread")
		return
	case req.Cron == "":
		writeError(w, http.StatusBadRequest, "a goal needs cron: how often to take the next step")
		return
	case req.MaxRuns > goalMaxRunsCap:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("max_runs is at most %d", goalMaxRunsCap))
		return
	case s.cronParse == nil:
		writeError(w, http.StatusServiceUnavailable, "scheduler not enabled")
		return
	}
	p, err := s.store.GetProjectBySlug(req.Project)
	if err != nil || p == nil || p.Status != "active" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("no active project %q", req.Project))
		return
	}
	if p.ChatID == 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("project %q has no chat to run in", p.Slug))
		return
	}
	cronExpr, err := s.cronParse(req.Cron)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid cron: "+err.Error())
		return
	}
	tz := req.TZ
	if tz == "" {
		tz = s.timezone
	}
	loc := s.location(tz)
	now := time.Now().In(loc)
	nextRun := cronExpr.Next(now)
	if nextRun.IsZero() {
		writeError(w, http.StatusBadRequest, "cron expression has no next run time")
		return
	}
	// A goal posts in a family thread: no step may land in quiet hours. Every
	// fire in the next 8 days is checked, in the scheduler's own zone.
	qloc := s.location("")
	horizon := now.Add(8 * 24 * time.Hour)
	for t, i := nextRun, 0; !t.IsZero() && t.Before(horizon) && i < 20000; t, i = cronExpr.Next(t), i+1 {
		if s.inQuietHours(t.In(qloc)) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("cron fires at %s, inside quiet hours (%02d:00–%02d:00); pick times outside them",
				t.In(qloc).Format("Mon 15:04"), s.quietStart, s.quietEnd))
			return
		}
	}

	key := project.GoalDedupKey(p.Slug, goal)
	msg, err := project.GoalScheduleMessage(p.Slug, p.ChatID, p.MessageThreadID, key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sched := &store.Schedule{
		ChatID:    p.ChatID,
		Label:     "goal: " + clipLabel(goal, 60),
		Message:   msg,
		Schedule:  req.Cron,
		Timezone:  tz,
		Type:      "cron",
		Mode:      scheduler.ModeEvent,
		NextRunAt: nextRun.UTC(),
		Enabled:   true,
		DedupKey:  key,
	}
	id, created, err := s.store.UpsertScheduleByKey(sched)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if created {
		if err := s.store.SetScheduleGoal(id, goal, criteria, p.Slug, req.MaxRuns); err != nil {
			s.store.DisableSchedule(id)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		slog.Info("rpc: goal schedule created", "id", id, "slug", p.Slug, "cron", req.Cron)
	}
	g, err := s.store.GetScheduleGoal(id)
	if err != nil || g == nil {
		writeError(w, http.StatusInternalServerError, "goal did not take effect")
		return
	}
	writeJSON(w, map[string]any{
		"id": id, "created": created, "project": g.Project, "goal": g.Goal, "done_criteria": g.DoneCriteria,
		"max_runs": g.MaxRuns, "next_run_at": g.NextRunAt.In(loc).Format(time.RFC3339),
		"result": fmt.Sprintf("Goal schedule #%d: its first step runs %s in the project's thread, at most %d runs. Each step reports with shell_schedule(action=progress, id=%d).",
			id, g.NextRunAt.In(loc).Format("Mon Jan 2 15:04"), g.MaxRuns, id),
	})
}

// schedulesProgress records one step of a goal: the evidence goes in the
// ledger, the notes replace the goal's working notes for the next fire.
func (s *Server) schedulesProgress(w http.ResponseWriter, req SchedulesRequest) {
	if req.ID == 0 {
		writeError(w, http.StatusBadRequest, "id is required for progress")
		return
	}
	g, err := s.store.GetScheduleGoal(req.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if g == nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("schedule #%d has no goal", req.ID))
		return
	}
	if g.Status != store.GoalOpen {
		writeError(w, http.StatusConflict, fmt.Sprintf("goal #%d is %s", req.ID, g.Status))
		return
	}
	evidence := strings.TrimSpace(req.Evidence)
	if evidence == "" {
		writeError(w, http.StatusBadRequest, "evidence is required: what you did and found this step")
		return
	}
	if notes := strings.TrimSpace(req.Notes); notes != "" {
		if err := s.store.SetGoalNotes(req.ID, notes); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := s.store.RecordGoalRun(req.ID, store.GoalRun{Outcome: store.VerdictProgress, Verdict: store.VerdictProgress, Evidence: evidence}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"id": req.ID, "status": g.Status,
		"result": fmt.Sprintf("Progress recorded for goal #%d (run %d of %d).", req.ID, g.Runs, g.MaxRuns)})
}

// GoalJudgeFunc rules on a done claim. It returns a not-met verdict with an
// error when the judge itself failed.
type GoalJudgeFunc func(ctx context.Context, goal, criteria, evidence string) (judge.Verdict, error)

// goalJudgeFailLimit is how many judge failures in a row turn into an ask
// to the owner.
const goalJudgeFailLimit = 3

// schedulesDone handles a done claim: the judge rules from the evidence.
// Met closes the goal as done, impossible as impossible; not met keeps it
// running and the reason goes back to the agent. Not met is an answer, not
// an error.
func (s *Server) schedulesDone(ctx context.Context, w http.ResponseWriter, req SchedulesRequest) {
	if req.ID == 0 {
		writeError(w, http.StatusBadRequest, "id is required for done")
		return
	}
	g, err := s.store.GetScheduleGoal(req.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if g == nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("schedule #%d has no goal", req.ID))
		return
	}
	if g.Status != store.GoalOpen {
		writeError(w, http.StatusConflict, fmt.Sprintf("goal #%d is %s", req.ID, g.Status))
		return
	}
	evidence := strings.TrimSpace(req.Evidence)
	v := judge.Verdict{Verdict: judge.NotMet, Reason: "no evidence given"}
	var jerr error
	switch {
	case evidence == "":
	case s.goalJudge == nil:
		v.Reason, jerr = "judge failed: no judge configured", fmt.Errorf("no judge configured")
	default:
		v, jerr = s.goalJudge(ctx, g.Goal, g.DoneCriteria, evidence)
		if jerr != nil || v.Verdict == "" {
			v.Verdict = judge.NotMet
		}
	}
	if err := s.store.RecordGoalRun(req.ID, store.GoalRun{Outcome: "judged", Verdict: v.Verdict, Reason: v.Reason, Evidence: evidence, CostUSD: v.CostUSD}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := store.GoalOpen
	result := fmt.Sprintf("Not met: %s. The goal keeps running; keep working and report with progress.", v.Reason)
	switch v.Verdict {
	case judge.Met:
		status = store.GoalDone
		result = fmt.Sprintf("Met: %s. Goal #%d is closed and its schedule stopped. Say so in the thread in one line, and add a 決定 entry to the project doc.", v.Reason, req.ID)
	case judge.Impossible:
		status = store.GoalImpossible
		result = fmt.Sprintf("Impossible: %s. Goal #%d is closed and its schedule stopped. Tell the thread why in one line and suggest what to do instead.", v.Reason, req.ID)
	}
	if status != store.GoalOpen {
		if err := s.store.CloseGoal(req.ID, status); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if jerr != nil {
		slog.Warn("rpc: goal judge failed", "id", req.ID, "error", jerr)
		if n, _ := s.store.GoalJudgeFailures(req.ID); n >= goalJudgeFailLimit {
			result += fmt.Sprintf(" The judge has failed %d times in a row: tell the owner (shell_relay to the owner's chat) that goal #%d cannot be closed until it works.", n, req.ID)
		}
	}
	slog.Info("rpc: goal judged", "id", req.ID, "verdict", v.Verdict, "cost_usd", v.CostUSD)
	writeJSON(w, map[string]any{"id": req.ID, "verdict": v.Verdict, "reason": v.Reason, "status": status, "result": result})
}

// inQuietHours reports whether a local time is inside the configured quiet
// hours (the scheduler's window for heartbeats).
func (s *Server) inQuietHours(t time.Time) bool {
	start, end := s.quietStart, s.quietEnd
	if start == end {
		return false
	}
	h := t.Hour()
	if start > end {
		return h >= start || h < end
	}
	return h >= start && h < end
}

func clipLabel(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
