package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Scheduler reliability ledger (V3-T1).
//
// job_runs records one row per fire ATTEMPT — including attempts that fail
// before or while spawning the subprocess. Failure-rate metrics structurally
// cannot see SILENCE: a schedule that fires and dies during spawn looks exactly
// like a schedule that never came due. This table plus schedules.last_success_at
// makes both visible, and is what the dead-man's-switch measures.

// Trigger contexts for a job run.
const (
	TriggerSchedule = "schedule" // the scheduler tick found the row due
	TriggerManual   = "manual"   // an operator/agent fired it by hand
	TriggerReplay   = "replay"   // a replayed fire after a crash/drain
)

// Job run outcomes.
const (
	OutcomeRunning           = "running" // in flight; a terminal outcome replaces it
	OutcomeFiredOK           = "fired_ok"
	OutcomeSpawnFailed       = "spawn_failed"
	OutcomeTurnFailed        = "turn_failed"
	OutcomeSkippedQuietHours = "skipped_quiet_hours"
	OutcomeSkippedDisabled   = "skipped_disabled"
	// OutcomeSkippedOverlap means the previous run of this schedule was still
	// in flight and the schedule's overlap policy said not to start another.
	OutcomeSkippedOverlap = "skipped_overlap"
	// OutcomeInterrupted marks a run that was still 'running' when the process
	// restarted. Set by ReapRunningJobRuns at startup — without it a crashed
	// turn stays 'running' forever and reads as healthy.
	OutcomeInterrupted = "interrupted"
)

// Auto-pause reasons. Machine-readable so the CLI and the agent can explain a
// dead schedule without parsing prose.
const (
	PauseInvalidCron     = "invalid_cron"
	PauseInvalidInterval = "invalid_interval"
	PauseNoNextRun       = "no_next_run"
	PauseMissingChat     = "missing_chat"
)

// JobRun is one recorded fire attempt.
type JobRun struct {
	ID             int64
	ScheduleID     int64
	TriggerContext string
	ScheduledAt    *time.Time
	// FiredAt is when the run STARTED. Rows written before the timing split
	// carry the completion moment here instead; those have FinishedAt nil.
	FiredAt    time.Time
	FinishedAt *time.Time
	DurationMS int64
	// Attempt is 1 for the first try and increments per retry of the same
	// occurrence, so a retried job reads as N rows rather than N unrelated runs.
	Attempt      int
	SessionID    *int64
	Outcome      string
	ErrorMessage string
	// Label is joined from schedules for display; empty when the schedule row
	// has since been deleted.
	Label string
}

// RecordJobRun appends a fire attempt to the ledger. A successful fire also
// stamps schedules.last_success_at — the reference point for the
// dead-man's-switch — in the same transaction, so "the ledger says it worked"
// and "the schedule looks alive" can never disagree.
func (s *Store) RecordJobRun(run JobRun) error {
	if run.Outcome == "" {
		return fmt.Errorf("job run outcome is required")
	}
	if run.TriggerContext == "" {
		run.TriggerContext = TriggerSchedule
	}
	if run.FiredAt.IsZero() {
		run.FiredAt = time.Now().UTC()
	}

	if run.Attempt <= 0 {
		run.Attempt = 1
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO job_runs (schedule_id, trigger_context, scheduled_at, fired_at, finished_at, duration_ms, attempt, session_id, outcome, error_message)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, run.ScheduleID, run.TriggerContext, run.ScheduledAt, run.FiredAt, run.FinishedAt, run.DurationMS,
		run.Attempt, run.SessionID, run.Outcome, run.ErrorMessage); err != nil {
		return err
	}

	if run.Outcome == OutcomeFiredOK {
		if _, err := tx.Exec(`UPDATE schedules SET last_success_at = ? WHERE id = ?`, run.FiredAt, run.ScheduleID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// StartJobRun opens a run row at the moment the job actually starts and returns
// its id for FinishJobRun. Recording the start separately is what makes queue
// delay measurable: with a single row written at completion, a job that waited
// four minutes behind another job is indistinguishable from one that took four
// minutes to run.
//
// An open row also makes a crash visible — see ReapRunningJobRuns.
func (s *Store) StartJobRun(run JobRun) (int64, error) {
	if run.TriggerContext == "" {
		run.TriggerContext = TriggerSchedule
	}
	if run.FiredAt.IsZero() {
		run.FiredAt = time.Now().UTC()
	}
	if run.Attempt <= 0 {
		run.Attempt = 1
	}
	res, err := s.db.Exec(`
		INSERT INTO job_runs (schedule_id, trigger_context, scheduled_at, fired_at, attempt, outcome, error_message)
		VALUES (?, ?, ?, ?, ?, ?, '')
	`, run.ScheduleID, run.TriggerContext, run.ScheduledAt, run.FiredAt, run.Attempt, OutcomeRunning)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishJobRun closes an open run with its terminal outcome and duration, and
// stamps schedules.last_success_at on success — in one transaction, so "the
// ledger says it worked" and "the schedule looks alive" cannot disagree.
//
// scheduleID and firedAt are passed in rather than read back inside the
// transaction, and that is load-bearing. The first version opened with a SELECT
// and then UPDATEd; a deferred SQLite transaction that upgrades from read to
// write returns SQLITE_BUSY IMMEDIATELY when another connection has written in
// the meantime — busy_timeout does not apply to the upgrade. This lands exactly
// at turn end, where the usage row, memory reflection and session compaction
// all write at once, and it cost a successful heartbeat its terminal row on
// 7/30: the run stayed 'running', got reaped as 'interrupted' at the next
// restart, and last_success_at was never stamped. Starting with the UPDATE
// takes the write lock up front, so busy_timeout governs the wait.
func (s *Store) FinishJobRun(runID int64, outcome, errMsg string) error {
	return s.finishJobRun(runID, 0, time.Time{}, outcome, errMsg)
}

// FinishJobRunAt is FinishJobRun with the run's identity supplied by the
// caller, avoiding the lookup entirely.
func (s *Store) FinishJobRunAt(runID, scheduleID int64, firedAt time.Time, outcome, errMsg string) error {
	return s.finishJobRun(runID, scheduleID, firedAt, outcome, errMsg)
}

func (s *Store) finishJobRun(runID, scheduleID int64, firedAt time.Time, outcome, errMsg string) error {
	if outcome == "" || outcome == OutcomeRunning {
		return fmt.Errorf("finish requires a terminal outcome, got %q", outcome)
	}

	// Fall back to a lookup only when the caller could not supply the values.
	// Done OUTSIDE the transaction so the write below never upgrades a read.
	if scheduleID == 0 || firedAt.IsZero() {
		if err := s.db.QueryRow(`SELECT schedule_id, fired_at FROM job_runs WHERE id = ?`, runID).
			Scan(&scheduleID, &firedAt); err != nil {
			return err
		}
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 250 * time.Millisecond)
		}
		if lastErr = s.finishJobRunOnce(runID, scheduleID, firedAt, outcome, errMsg); lastErr == nil {
			return nil
		}
		if !isBusy(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

func (s *Store) finishJobRunOnce(runID, scheduleID int64, firedAt time.Time, outcome, errMsg string) error {
	now := time.Now().UTC()
	durationMS := now.Sub(firedAt).Milliseconds()
	if durationMS < 0 {
		durationMS = 0
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Write first: this statement takes the write lock, so a concurrent writer
	// makes us WAIT (busy_timeout) instead of failing outright.
	if _, err := tx.Exec(`
		UPDATE job_runs SET outcome = ?, error_message = ?, finished_at = ?, duration_ms = ?
		WHERE id = ?
	`, outcome, errMsg, now, durationMS, runID); err != nil {
		return err
	}
	if outcome == OutcomeFiredOK {
		if _, err := tx.Exec(`UPDATE schedules SET last_success_at = ? WHERE id = ?`, now, scheduleID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// isBusy reports whether an error is SQLite's "database is locked" contention
// signal, which is worth retrying, as opposed to a real failure.
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "sqlite_busy")
}

// ReapRunningJobRuns closes runs left open by a crash or restart. Called once at
// startup: a row still marked 'running' cannot be in flight, because the process
// that owned it is gone. Without this a crashed turn stays 'running' forever and
// every "is anything stuck?" check reads it as healthy.
//
// Returns the number of rows reaped.
func (s *Store) ReapRunningJobRuns() (int, error) {
	res, err := s.db.Exec(`
		UPDATE job_runs
		SET outcome = ?, error_message = 'daemon restarted while this run was in flight', finished_at = ?
		WHERE outcome = ?
	`, OutcomeInterrupted, time.Now().UTC(), OutcomeRunning)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ListJobRuns returns the most recent fire attempts, newest first.
// scheduleID = 0 means all schedules.
func (s *Store) ListJobRuns(scheduleID int64, limit int) ([]JobRun, error) {
	if limit <= 0 {
		limit = 20
	}
	query := `
		SELECT r.id, r.schedule_id, r.trigger_context, r.scheduled_at, r.fired_at,
		       r.finished_at, r.duration_ms, r.attempt,
		       r.session_id, r.outcome, r.error_message, COALESCE(s.label, '')
		FROM job_runs r
		LEFT JOIN schedules s ON s.id = r.schedule_id
	`
	args := []any{}
	if scheduleID != 0 {
		query += ` WHERE r.schedule_id = ?`
		args = append(args, scheduleID)
	}
	query += ` ORDER BY r.fired_at DESC, r.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []JobRun
	for rows.Next() {
		var r JobRun
		var scheduledAt, finishedAt sql.NullTime
		var sessionID sql.NullInt64
		if err := rows.Scan(&r.ID, &r.ScheduleID, &r.TriggerContext, &scheduledAt, &r.FiredAt,
			&finishedAt, &r.DurationMS, &r.Attempt,
			&sessionID, &r.Outcome, &r.ErrorMessage, &r.Label); err != nil {
			return nil, err
		}
		if scheduledAt.Valid {
			t := scheduledAt.Time
			r.ScheduledAt = &t
		}
		if finishedAt.Valid {
			t := finishedAt.Time
			r.FinishedAt = &t
		}
		if sessionID.Valid {
			v := sessionID.Int64
			r.SessionID = &v
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// ScheduleDedupKey derives the stable idempotency key for a registration:
// (chat_id, type, schedule-expr, message-hash). Same request twice → same key
// → one row. This is the structural fix for the duplicate-schedule class that
// previously had to be cleaned up by disabling rows by hand.
//
// The message is whitespace-normalized before hashing so cosmetic differences
// in how the agent re-types the same reminder still collide.
func ScheduleDedupKey(chatID int64, schedType, expr, message string) string {
	norm := strings.Join(strings.Fields(message), " ")
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s\x00%s",
		chatID, strings.TrimSpace(schedType), strings.TrimSpace(expr), norm)))
	return hex.EncodeToString(sum[:])[:32]
}

// UpsertScheduleByKey registers a schedule idempotently. When an ENABLED row
// with the same dedup key already exists it is returned untouched and
// created=false; otherwise a new row is inserted and created=true.
//
// Only enabled rows participate: a one-shot that already fired (and was
// disabled) must not block creating the same reminder again tomorrow.
//
// WRITE FIRST, exactly as in the task queue. An earlier version opened a
// transaction, SELECTed for the dedup key, then INSERTed. SQLite refuses to
// upgrade a read transaction to a write one under contention — it returns
// SQLITE_BUSY immediately, ignoring busy_timeout — so a concurrent writer could
// make this path fail outright. It is the path the user's reminders are created
// on, and a failure there costs the family a reminder, so the read is gone: the
// partial unique index (dedup_key, enabled = 1) does the deduplication and the
// SELECT only runs after a conflict has already told us a row exists.
func (s *Store) UpsertScheduleByKey(sched *Schedule) (id int64, created bool, err error) {
	key := ScheduleDedupKey(sched.ChatID, sched.Type, sched.Schedule, sched.Message)
	sched.DedupKey = key

	enabled := 0
	if sched.Enabled {
		enabled = 1
	}
	res, err := s.db.Exec(`
		INSERT INTO schedules (chat_id, label, message, schedule, timezone, type, mode,
			next_run_at, expected_next_at, enabled, dedup_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(dedup_key) WHERE dedup_key != '' AND enabled = 1 DO NOTHING
	`, sched.ChatID, sched.Label, sched.Message, sched.Schedule, sched.Timezone, sched.Type,
		sched.Mode, sched.NextRunAt, sched.NextRunAt, enabled, key)
	if err != nil {
		return 0, false, err
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		newID, err := res.LastInsertId()
		if err != nil {
			return 0, false, err
		}
		sched.ID = newID
		return newID, true, nil
	}

	// Conflict: an enabled row with this key already exists. Return it untouched.
	var existingID int64
	var existingNext time.Time
	err = s.db.QueryRow(`
		SELECT id, next_run_at FROM schedules
		WHERE dedup_key = ? AND enabled = 1 ORDER BY id LIMIT 1
	`, key).Scan(&existingID, &existingNext)
	if err != nil {
		if err == sql.ErrNoRows {
			// The conflicting row was disabled between the insert and this read.
			// Rare and self-healing: the caller can register again.
			return 0, false, fmt.Errorf("schedule %q vanished between insert and lookup", key)
		}
		return 0, false, err
	}
	sched.ID = existingID
	sched.NextRunAt = existingNext
	return existingID, false, nil
}

// PauseSchedule disables a schedule and records WHY in machine-readable form.
// Used for unrecoverable config errors (bad cron, unparseable interval, missing
// chat) so the scheduler stops retrying forever and the failure stays
// explainable instead of showing up as an unexplained silent row.
func (s *Store) PauseSchedule(id int64, reason string) error {
	_, err := s.db.Exec(`UPDATE schedules SET enabled = 0, paused_reason = ? WHERE id = ?`, reason, id)
	return err
}

// EnableScheduleFrom re-enables a schedule with next_run_at recomputed from NOW
// forward and any auto-pause reason cleared. Missed occurrences are NEVER
// replayed — a schedule paused for a week must not fire seven times on resume.
func (s *Store) EnableScheduleFrom(id int64, nextRun time.Time) error {
	_, err := s.db.Exec(`
		UPDATE schedules SET enabled = 1, paused_reason = '', next_run_at = ?, expected_next_at = ?
		WHERE id = ?
	`, nextRun, nextRun, id)
	return err
}

// SetExpectedNextAt records the EXACT next occurrence for a schedule.
// next_run_at holds the jittered firing moment; this holds the truth the
// dead-man's-switch and the preview report against.
func (s *Store) SetExpectedNextAt(id int64, expected time.Time) error {
	_, err := s.db.Exec(`UPDATE schedules SET expected_next_at = ? WHERE id = ?`, expected, id)
	return err
}

// ListAllSchedules returns every schedule across chats, including the V3-T1
// reliability columns. Read-only view behind `shell schedules`.
// enabledOnly restricts to live rows.
func (s *Store) ListAllSchedules(enabledOnly bool) ([]Schedule, error) {
	query := `
		SELECT id, chat_id, label, message, schedule, timezone, type, mode,
		       next_run_at, last_run_at, enabled, created_at,
		       COALESCE(dedup_key, ''), COALESCE(paused_reason, ''),
		       expected_next_at, last_success_at
		FROM schedules
	`
	if enabledOnly {
		query += ` WHERE enabled = 1`
	}
	query += ` ORDER BY enabled DESC, next_run_at`

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Schedule
	for rows.Next() {
		sc, err := scanScheduleFull(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// GetScheduleByID returns a schedule by ID (no chat scoping), including the
// reliability columns. Returns nil when the row does not exist.
func (s *Store) GetScheduleByID(id int64) (*Schedule, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, label, message, schedule, timezone, type, mode,
		       next_run_at, last_run_at, enabled, created_at,
		       COALESCE(dedup_key, ''), COALESCE(paused_reason, ''),
		       expected_next_at, last_success_at
		FROM schedules WHERE id = ?
	`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	sc, err := scanScheduleFull(rows)
	if err != nil {
		return nil, err
	}
	return &sc, nil
}

// scanScheduleFull scans the full column list shared by ListAllSchedules and
// GetScheduleByID.
func scanScheduleFull(rows *sql.Rows) (Schedule, error) {
	var sc Schedule
	var lastRun, expectedNext, lastSuccess sql.NullTime
	var enabled int
	if err := rows.Scan(&sc.ID, &sc.ChatID, &sc.Label, &sc.Message, &sc.Schedule, &sc.Timezone,
		&sc.Type, &sc.Mode, &sc.NextRunAt, &lastRun, &enabled, &sc.CreatedAt,
		&sc.DedupKey, &sc.PausedReason, &expectedNext, &lastSuccess); err != nil {
		return sc, err
	}
	sc.Enabled = enabled == 1
	if lastRun.Valid {
		t := lastRun.Time
		sc.LastRunAt = &t
	}
	if expectedNext.Valid {
		t := expectedNext.Time
		sc.ExpectedNextAt = &t
	}
	if lastSuccess.Valid {
		t := lastSuccess.Time
		sc.LastSuccessAt = &t
	}
	return sc, nil
}
