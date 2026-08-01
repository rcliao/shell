package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Durable task queue — see docs/TASKS.md for the design and its rationale.
//
// This file is the storage layer only. Nothing enqueues or leases through it
// yet; wiring happens in later steps so the semantics can be tested before any
// live path depends on them.
//
// The queue is generic work, not chat messages: a task carries a Kind (which
// selects a handler), an opaque JSON Payload the queue never interprets, and a
// PartitionKey that is the ONLY concurrency concept — tasks sharing a key run
// one at a time and in order, tasks with different keys run concurrently, and
// an empty key means no constraint at all.

// Task states.
const (
	TaskQueued = "queued"
	TaskLeased = "leased"
	TaskDone   = "done"
	TaskFailed = "failed" // terminal: attempts exhausted or permanently unprocessable
)

// Task sources — who enqueued the work.
const (
	TaskSourceTelegram  = "telegram"
	TaskSourceScheduler = "scheduler"
	TaskSourceA2A       = "a2a"
	TaskSourceAgent     = "agent"
)

// Task is one unit of durable work.
type Task struct {
	ID   int64
	Kind string
	// Source records who enqueued it, for accounting. It never selects the
	// handler — Kind does. Two sources can enqueue the same kind.
	Source string
	// IdempotencyKey makes enqueue exactly-once. Required and unique: a second
	// enqueue with the same key returns the existing task untouched.
	IdempotencyKey string
	// PartitionKey serializes tasks against each other. Empty = unconstrained.
	PartitionKey string
	Payload      string // JSON, handler-defined
	State        string
	Attempts     int
	MaxAttempts  int
	// NotBefore delays visibility. This is what the retired delegation system
	// could not express: it hardcoded a 60-minute TTL swept every minute, so a
	// "check this tomorrow at 09:00" task always expired before it was due.
	NotBefore   *time.Time
	LeaseOwner  string
	LeasedUntil *time.Time
	EnqueuedAt  time.Time
	StartedAt   *time.Time
	DoneAt      *time.Time
	Result      string // handler-defined; what the assigner reads back
	LastError   string
}

const defaultMaxAttempts = 3

// DeriveIdempotencyKey builds a stable key from parts. Callers with a natural
// key (a Telegram message id, a schedule occurrence) should pass it; callers
// without one — an agent assigning itself work — get a content hash so a
// retried turn does not enqueue the same work twice.
func DeriveIdempotencyKey(parts ...string) string {
	norm := make([]string, 0, len(parts))
	for _, p := range parts {
		norm = append(norm, strings.Join(strings.Fields(p), " "))
	}
	sum := sha256.Sum256([]byte(strings.Join(norm, "\x00")))
	return hex.EncodeToString(sum[:])[:32]
}

// EnqueueTask registers work idempotently. When a task with the same
// idempotency key already exists it is returned as-is with created=false —
// including when it has already run, so a redelivery never re-executes.
func (s *Store) EnqueueTask(t Task) (id int64, created bool, err error) {
	if t.Kind == "" {
		return 0, false, fmt.Errorf("task kind is required")
	}
	if t.IdempotencyKey == "" {
		return 0, false, fmt.Errorf("task idempotency key is required")
	}
	if t.MaxAttempts <= 0 {
		t.MaxAttempts = defaultMaxAttempts
	}
	if t.Source == "" {
		t.Source = TaskSourceAgent
	}
	if t.Payload == "" {
		t.Payload = "{}"
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	// Write-first would be ideal, but enqueue genuinely needs to know whether
	// the key exists. The read is cheap and this transaction is short; the
	// SQLITE_BUSY hazard documented on FinishJobRun applies to long
	// read-then-write spans, not to an immediate insert-or-return.
	var existing int64
	switch err := tx.QueryRow(
		`SELECT id FROM tasks WHERE idempotency_key = ?`, t.IdempotencyKey).Scan(&existing); {
	case err == nil:
		return existing, false, tx.Commit()
	case err == sql.ErrNoRows:
	default:
		return 0, false, err
	}

	res, err := tx.Exec(`
		INSERT INTO tasks (kind, source, idempotency_key, partition_key, payload,
		                   state, attempts, max_attempts, not_before, enqueued_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?)
	`, t.Kind, t.Source, t.IdempotencyKey, t.PartitionKey, t.Payload,
		TaskQueued, t.MaxAttempts, t.NotBefore, time.Now().UTC())
	if err != nil {
		return 0, false, err
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	return newID, true, tx.Commit()
}

// LeaseTask claims the next ready task and marks it in flight, returning nil
// when nothing is available.
//
// Readiness has three conditions, and the partition rule is the subtle one: a
// task is skipped when ANOTHER task sharing its partition key is currently
// leased. That is what serializes a chat's turns without an in-memory mutex,
// and it is why partition_key rather than chat_id is the queue's only
// concurrency concept.
func (s *Store) LeaseTask(owner string, leaseFor time.Duration) (*Task, error) {
	if owner == "" {
		return nil, fmt.Errorf("lease owner is required")
	}
	now := time.Now().UTC()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var id int64
	err = tx.QueryRow(`
		SELECT id FROM tasks t
		WHERE t.state = ?
		  AND (t.not_before IS NULL OR t.not_before <= ?)
		  AND (t.partition_key = '' OR NOT EXISTS (
		        SELECT 1 FROM tasks b
		        WHERE b.partition_key = t.partition_key
		          AND b.state = ?
		      ))
		ORDER BY t.id LIMIT 1
	`, TaskQueued, now, TaskLeased).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}

	until := now.Add(leaseFor)
	if _, err := tx.Exec(`
		UPDATE tasks SET state = ?, lease_owner = ?, leased_until = ?,
		                 attempts = attempts + 1, started_at = coalesce(started_at, ?)
		WHERE id = ?
	`, TaskLeased, owner, until, now, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetTask(id)
}

// CompleteTask marks a leased task done with its handler result.
func (s *Store) CompleteTask(id int64, result string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`
		UPDATE tasks SET state = ?, result = ?, done_at = ?, lease_owner = '', leased_until = NULL
		WHERE id = ?
	`, TaskDone, result, now, id)
	return err
}

// FailTask records a failed attempt. The task returns to queued while attempts
// remain, and becomes terminally failed once they are exhausted — so a
// permanently broken task stops consuming workers instead of looping forever.
func (s *Store) FailTask(id int64, errMsg string) (terminal bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var attempts, maxAttempts int
	if err := tx.QueryRow(`SELECT attempts, max_attempts FROM tasks WHERE id = ?`, id).
		Scan(&attempts, &maxAttempts); err != nil {
		return false, err
	}

	terminal = attempts >= maxAttempts
	state := TaskQueued
	if terminal {
		state = TaskFailed
	}
	if _, err := tx.Exec(`
		UPDATE tasks SET state = ?, last_error = ?, lease_owner = '', leased_until = NULL,
		                 done_at = CASE WHEN ? THEN ? ELSE done_at END
		WHERE id = ?
	`, state, errMsg, terminal, time.Now().UTC(), id); err != nil {
		return false, err
	}
	return terminal, tx.Commit()
}

// ReclaimTasks returns leased tasks to the queue when their owner is gone or
// their lease expired.
//
// Called at startup with the new boot id: any lease held by a different owner
// belonged to a process that no longer exists, so it cannot still be running.
// This replaces the age heuristic used for pending turns, which is
// simultaneously too short for a slow turn and too long for a crash loop.
func (s *Store) ReclaimTasks(currentOwner string) (int, error) {
	res, err := s.db.Exec(`
		UPDATE tasks
		SET state = ?, lease_owner = '', leased_until = NULL,
		    last_error = 'lease reclaimed: owner gone or lease expired'
		WHERE state = ? AND (lease_owner != ? OR leased_until < ?)
	`, TaskQueued, TaskLeased, currentOwner, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// GetTask returns one task by id, or nil when absent.
func (s *Store) GetTask(id int64) (*Task, error) {
	rows, err := s.db.Query(taskSelect+` WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanTasks(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

// ListTasks returns tasks in a state, newest first. Empty state = all.
func (s *Store) ListTasks(state string, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 50
	}
	q := taskSelect
	args := []any{}
	if state != "" {
		q += ` WHERE state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

const taskSelect = `
	SELECT id, kind, source, idempotency_key, partition_key, payload, state,
	       attempts, max_attempts, not_before, lease_owner, leased_until,
	       enqueued_at, started_at, done_at, result, last_error
	FROM tasks`

func scanTasks(rows *sql.Rows) ([]Task, error) {
	var out []Task
	for rows.Next() {
		var t Task
		var notBefore, leasedUntil, startedAt, doneAt sql.NullTime
		if err := rows.Scan(&t.ID, &t.Kind, &t.Source, &t.IdempotencyKey, &t.PartitionKey,
			&t.Payload, &t.State, &t.Attempts, &t.MaxAttempts, &notBefore, &t.LeaseOwner,
			&leasedUntil, &t.EnqueuedAt, &startedAt, &doneAt, &t.Result, &t.LastError); err != nil {
			return nil, err
		}
		for _, p := range []struct {
			src sql.NullTime
			dst **time.Time
		}{{notBefore, &t.NotBefore}, {leasedUntil, &t.LeasedUntil}, {startedAt, &t.StartedAt}, {doneAt, &t.DoneAt}} {
			if p.src.Valid {
				v := p.src.Time
				*p.dst = &v
			}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// retireLegacyTaskTable drops the pre-queue `/task` backlog table when it is
// present, matches the old shape, and is empty. Best-effort and silent on a
// fresh DB where the table never existed.
func (s *Store) retireLegacyTaskTable() {
	var ddl string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='tasks'`).Scan(&ddl); err != nil {
		return // no tasks table yet — fresh DB
	}
	// The new schema has these; the legacy one does not.
	if strings.Contains(ddl, "idempotency_key") {
		return // already the queue
	}
	var rows int
	if err := s.db.QueryRow(`SELECT count(*) FROM tasks`).Scan(&rows); err != nil || rows > 0 {
		if rows > 0 {
			slog.Warn("legacy /task table has rows — leaving it in place; the work queue cannot claim the name",
				"rows", rows)
		}
		return
	}
	if _, err := s.db.Exec(`DROP TABLE tasks`); err != nil {
		slog.Warn("failed to retire legacy /task table", "error", err)
		return
	}
	slog.Info("retired the empty legacy /task backlog table; work queue takes the name")
}

// openRawForTest opens the DB without running migrations. Test-only helper for
// constructing legacy-shaped databases.
func openRawForTest(path string) (*sql.DB, error) {
	return sql.Open("sqlite", path)
}
