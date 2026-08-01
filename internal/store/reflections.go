package store

import (
	"database/sql"
	"time"
)

// Deep-heartbeat reflection journal.
//
// Every 6th heartbeat runs a deep reflection, and those turns are where the
// agent audits its own past behavior — the only place in this system that
// produces personality-bearing change. Until now that output was written to
// `messages` and nothing else: heartbeats run on the system chat, and
// daemon.go's onNotify returns early for it, so the reasoning was discarded
// as soon as the turn ended. Whatever the agent concluded survived only if it
// happened to call a memory tool mid-turn.
//
// This table captures the reflection itself, from the bridge, with no agent
// cooperation required. That is the point: a journal that depends on the agent
// remembering to write it is a journal with gaps exactly where the agent was
// distracted. Capture here is a side effect of the turn completing.
//
// Deliberately WRITE-ONLY for now. Nothing reads these rows back into a prompt,
// nothing promotes them into ghost, and no behavior changes. The first question
// is what deep reflection actually produces when observed over weeks — curation
// is a later, separately-decided step that should be designed against the real
// corpus rather than a guess about it.

// Reflection is one captured deep-heartbeat turn.
type Reflection struct {
	ID int64
	// JobRunID ties the reflection to its fire in the job_runs ledger, so a
	// reflection can be traced back to when it fired, how long it took and
	// whether it was a retry. Zero when the beat did not come from the
	// scheduler (a manually triggered turn).
	JobRunID   int64
	ChatID     int64
	BeatCount  int // the persisted heartbeat counter at fire time
	Model      string
	Text       string
	ToolCalls  int
	Noop       bool
	DurationMS int64
	CreatedAt  time.Time
}

// RecordReflection stores one deep-heartbeat turn. Best-effort by contract:
// a journal write must never be able to fail a heartbeat, so callers log and
// continue rather than propagating.
func (s *Store) RecordReflection(r Reflection) (int64, error) {
	noop := 0
	if r.Noop {
		noop = 1
	}
	res, err := s.db.Exec(`
		INSERT INTO reflections (job_run_id, chat_id, beat_count, model, text, tool_calls, noop, duration_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, r.JobRunID, r.ChatID, r.BeatCount, r.Model, r.Text, r.ToolCalls, noop, r.DurationMS, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListReflections returns captured reflections, newest first.
func (s *Store) ListReflections(limit int) ([]Reflection, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`
		SELECT id, job_run_id, chat_id, beat_count, model, text, tool_calls, noop, duration_ms, created_at
		FROM reflections ORDER BY id DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReflections(rows)
}

// GetReflection returns one reflection by id, or nil when absent.
func (s *Store) GetReflection(id int64) (*Reflection, error) {
	rows, err := s.db.Query(`
		SELECT id, job_run_id, chat_id, beat_count, model, text, tool_calls, noop, duration_ms, created_at
		FROM reflections WHERE id = ?
	`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanReflections(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

func scanReflections(rows *sql.Rows) ([]Reflection, error) {
	var out []Reflection
	for rows.Next() {
		var r Reflection
		var noop int
		if err := rows.Scan(&r.ID, &r.JobRunID, &r.ChatID, &r.BeatCount, &r.Model,
			&r.Text, &r.ToolCalls, &noop, &r.DurationMS, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Noop = noop == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReflectionCaptureRate compares captured reflections against deep-beat fires
// in the ledger. Capture is automatic, so this should be 1.0 — a shortfall
// means the hook is missing turns, which is the one way this design can fail
// silently.
func (s *Store) ReflectionCaptureRate(since time.Time) (captured, deepFires int, err error) {
	if err = s.db.QueryRow(
		`SELECT count(*) FROM reflections WHERE created_at >= ?`, since).Scan(&captured); err != nil {
		return 0, 0, err
	}
	// Deep fires are not labelled in job_runs, so this counts reflections
	// against ALL heartbeat fires in the window; the caller divides by the
	// deep interval. Reported raw rather than guessed at.
	err = s.db.QueryRow(`
		SELECT count(*) FROM job_runs r
		JOIN schedules s ON s.id = r.schedule_id
		WHERE s.type = 'heartbeat' AND r.outcome = 'fired_ok' AND r.fired_at >= ?
	`, since).Scan(&deepFires)
	return captured, deepFires, err
}
