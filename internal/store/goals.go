package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Goal schedules (docs/DESIGN-AGENT-LOOPS.md step 2). A goal rides on an
// ordinary schedule row: the schedule keeps firing, retrying and keeping its
// job_runs ledger, and the goal columns say what it is for and when it is
// done. The ledger rows a goal adds (progress reports, judged claims) carry
// verdict / reason / evidence and never count as fires.

// Goal statuses. A schedule with goal = ” has goal_status = ”.
const (
	GoalOpen       = "open"
	GoalDone       = "done"
	GoalImpossible = "impossible"
	GoalExpired    = "expired"
	GoalCancelled  = "cancelled"
)

// Goal ledger verdicts on job_runs.
const (
	VerdictProgress = "progress"
	VerdictMet      = "met"
	VerdictNotMet   = "not_met"
)

// TriggerGoal marks a job_runs row written by the goal itself (a progress
// report or a judged done claim), not by a fire.
const TriggerGoal = "goal"

// DefaultGoalMaxRuns bounds a goal created without max_runs.
const DefaultGoalMaxRuns = 10

// ScheduleGoal is the goal view of one schedule row.
type ScheduleGoal struct {
	ScheduleID   int64
	ChatID       int64
	Label        string
	Schedule     string
	Enabled      bool
	NextRunAt    time.Time
	Goal         string
	DoneCriteria string
	Notes        string
	Project      string
	MaxRuns      int
	Runs         int
	Status       string
}

// GoalRun is one goal ledger row.
type GoalRun struct {
	At       time.Time
	Outcome  string
	Verdict  string
	Reason   string
	Evidence string
	CostUSD  float64
}

const goalColumns = `id, chat_id, label, schedule, enabled, next_run_at, goal, done_criteria,
	goal_notes, goal_project, max_runs, goal_runs, goal_status`

func scanGoal(row interface{ Scan(...any) error }) (*ScheduleGoal, error) {
	var g ScheduleGoal
	var enabled int
	if err := row.Scan(&g.ScheduleID, &g.ChatID, &g.Label, &g.Schedule, &enabled, &g.NextRunAt,
		&g.Goal, &g.DoneCriteria, &g.Notes, &g.Project, &g.MaxRuns, &g.Runs, &g.Status); err != nil {
		return nil, err
	}
	g.Enabled = enabled == 1
	return &g, nil
}

// SetScheduleGoal attaches a goal to an existing schedule and opens it.
func (s *Store) SetScheduleGoal(id int64, goal, criteria, project string, maxRuns int) error {
	if goal == "" || criteria == "" {
		return fmt.Errorf("a goal needs both goal and done_criteria")
	}
	if maxRuns <= 0 {
		maxRuns = DefaultGoalMaxRuns
	}
	_, err := s.db.Exec(`UPDATE schedules SET goal = ?, done_criteria = ?, goal_project = ?,
		max_runs = ?, goal_runs = 0, goal_status = ? WHERE id = ?`,
		goal, criteria, project, maxRuns, GoalOpen, id)
	return err
}

// GetScheduleGoal returns the schedule's goal view; nil when the row does
// not exist or carries no goal.
func (s *Store) GetScheduleGoal(id int64) (*ScheduleGoal, error) {
	g, err := scanGoal(s.db.QueryRow(`SELECT `+goalColumns+` FROM schedules WHERE id = ? AND goal != ''`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return g, err
}

// OpenGoals returns the open goals, for one project or (project "") all.
func (s *Store) OpenGoals(project string) ([]ScheduleGoal, error) {
	q := `SELECT ` + goalColumns + ` FROM schedules WHERE goal != '' AND goal_status = ?`
	args := []any{GoalOpen}
	if project != "" {
		q += ` AND goal_project = ?`
		args = append(args, project)
	}
	rows, err := s.db.Query(q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduleGoal
	for rows.Next() {
		g, err := scanGoal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

// BumpGoalRun counts one fire of an open goal and returns the new count.
func (s *Store) BumpGoalRun(id int64) (int, error) {
	if _, err := s.db.Exec(`UPDATE schedules SET goal_runs = goal_runs + 1 WHERE id = ? AND goal_status = ?`, id, GoalOpen); err != nil {
		return 0, err
	}
	var n int
	err := s.db.QueryRow(`SELECT goal_runs FROM schedules WHERE id = ?`, id).Scan(&n)
	return n, err
}

// SetGoalNotes replaces the goal's working notes: what the next fire needs
// to know (what was checked, what was found, what to try next).
func (s *Store) SetGoalNotes(id int64, notes string) error {
	_, err := s.db.Exec(`UPDATE schedules SET goal_notes = ? WHERE id = ? AND goal != ''`, notes, id)
	return err
}

// CloseGoal ends an open goal with status and disables its schedule, with
// paused_reason "goal_<status>". Closing a goal that is not open is an error.
func (s *Store) CloseGoal(id int64, status string) error {
	res, err := s.db.Exec(`UPDATE schedules SET goal_status = ?, enabled = 0, paused_reason = ?
		WHERE id = ? AND goal_status = ?`, status, "goal_"+status, id, GoalOpen)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("schedule #%d has no open goal", id)
	}
	return nil
}

// RecordGoalRun appends a goal ledger row (progress report or judged claim).
// It does not stamp last_success_at: it is not a fire.
func (s *Store) RecordGoalRun(id int64, r GoalRun) error {
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	_, err := s.db.Exec(`INSERT INTO job_runs (schedule_id, trigger_context, fired_at, finished_at,
		outcome, verdict, reason, evidence, judge_cost_usd) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, TriggerGoal, r.At, r.At, r.Outcome, r.Verdict, r.Reason, r.Evidence, r.CostUSD)
	return err
}

// LastGoalRun returns the goal's latest ledger row; nil when there is none.
func (s *Store) LastGoalRun(id int64) (*GoalRun, error) {
	var r GoalRun
	err := s.db.QueryRow(`SELECT fired_at, outcome, verdict, reason, evidence, judge_cost_usd
		FROM job_runs WHERE schedule_id = ? AND trigger_context = ? ORDER BY id DESC LIMIT 1`,
		id, TriggerGoal).Scan(&r.At, &r.Outcome, &r.Verdict, &r.Reason, &r.Evidence, &r.CostUSD)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}
