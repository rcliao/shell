package rpc

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

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
	// A goal posts in a family thread: no step may land in quiet hours.
	for t, i := nextRun, 0; i < 48 && !t.IsZero(); t, i = cronExpr.Next(t), i+1 {
		if s.inQuietHours(t.In(loc)) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("cron fires at %s, inside quiet hours (%02d:00–%02d:00); pick times outside them",
				t.In(loc).Format("Mon 15:04"), s.quietStart, s.quietEnd))
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
