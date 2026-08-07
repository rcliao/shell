package store

import (
	"fmt"
	"time"
)

// Tasks the enqueuer runs ITSELF.
//
// The queue's normal shape is enqueue-then-a-worker-leases. Telegram intake is
// deliberately different: the handler that receives a message goes on to run
// the turn in-process, because that is where the streaming placeholder, the
// coalescing waiters and the per-topic lock already live. Handing that turn to
// a worker would mean either running it twice or rewriting all of it at once.
//
// So the handler creates its task ALREADY LEASED BY ITSELF. The row is a
// durable record of a turn in flight, invisible to workers while its owner
// lives. If the process dies mid-turn, the owner no longer matches — a boot
// owner carries a start timestamp precisely so a SIGHUP exec that keeps the PID
// still counts as a new owner — and the ordinary reclaim path hands the task to
// a worker, which replays it through the transport's sink.
//
// This is what lets Telegram move onto the queue without touching the 600-line
// handler: the queue replaces the LEDGER and the REPLAY, which are the weak
// parts, and leaves execution exactly where it is.

// BeginOwnedTask records work the caller is about to run itself.
//
// Returns doneAlready when the key has already completed, which is how a
// transport redelivery is recognised and dropped — the same job the pending
// turns ledger did, now with attempt limits and expiry it never had.
func (s *Store) BeginOwnedTask(t Task, owner string, leaseFor time.Duration) (id int64, doneAlready bool, err error) {
	if owner == "" {
		return 0, false, fmt.Errorf("owned task requires an owner")
	}
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

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 50 * time.Millisecond)
		}
		id, doneAlready, err := s.beginOwnedOnce(t, owner, leaseFor)
		if err == nil {
			return id, doneAlready, nil
		}
		lastErr = err
		if !isBusy(err) {
			return 0, false, err
		}
	}
	return 0, false, lastErr
}

// beginOwnedOnce inserts the row already leased, in one write.
//
// Write-first for the same reason EnqueueTask is: a SELECT-then-INSERT inside a
// transaction gets an immediate SQLITE_BUSY on the read-to-write upgrade rather
// than honouring busy_timeout.
//
// attempts starts at 1, not 0. The inline run IS the first attempt, so a replay
// after a crash is attempt 2 and max_attempts bounds total runs the same way it
// does for a task a worker leased normally.
func (s *Store) beginOwnedOnce(t Task, owner string, leaseFor time.Duration) (int64, bool, error) {
	now := time.Now().UTC()
	res, err := s.db.Exec(`
		INSERT INTO tasks (kind, source, idempotency_key, partition_key, payload,
		                   state, attempts, max_attempts, not_before, expires_at,
		                   lease_owner, leased_until, enqueued_at, started_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(idempotency_key) DO NOTHING
	`, t.Kind, t.Source, t.IdempotencyKey, t.PartitionKey, t.Payload,
		TaskLeased, t.MaxAttempts, t.NotBefore, t.ExpiresAt,
		owner, now.Add(leaseFor), now, now)
	if err != nil {
		return 0, false, err
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		id, err := res.LastInsertId()
		return id, false, err
	}

	// Key already present. Safe to read here: no write transaction is open, so
	// there is no upgrade to fail.
	var id int64
	var state string
	if err := s.db.QueryRow(
		`SELECT id, state FROM tasks WHERE idempotency_key = ?`, t.IdempotencyKey).Scan(&id, &state); err != nil {
		return 0, false, err
	}
	return id, state == TaskDone, nil
}

// AbandonOwnedTask returns a self-run task to the queue for someone else to
// run, without spending an attempt beyond the one already counted.
//
// Only touches rows THIS owner holds. A task already reclaimed by a newer boot
// is not ours to release, and stamping it here would fight the reclaim.
func (s *Store) AbandonOwnedTask(key, reason, owner string) error {
	if key == "" {
		return fmt.Errorf("idempotency key is required")
	}
	_, err := s.db.Exec(`
		UPDATE tasks SET state = ?, lease_owner = '', leased_until = NULL, last_error = ?
		WHERE idempotency_key = ? AND state = ? AND lease_owner = ?
	`, TaskQueued, reason, key, TaskLeased, owner)
	return err
}

// CompleteOwnedTask marks a self-run task finished by its idempotency key.
//
// By key rather than by id because the caller that finishes a turn is often not
// the one that started it — the Telegram handler completes a turn from a
// different code path than the one that began it, and both know the message,
// not the row.
func (s *Store) CompleteOwnedTask(key, result string) error {
	if key == "" {
		return fmt.Errorf("idempotency key is required")
	}
	_, err := s.db.Exec(`
		UPDATE tasks SET state = ?, result = ?, done_at = ?, lease_owner = '', leased_until = NULL
		WHERE idempotency_key = ? AND state = ?
	`, TaskDone, result, time.Now().UTC(), key, TaskLeased)
	return err
}
