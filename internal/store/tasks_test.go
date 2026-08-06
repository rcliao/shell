package store

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func taskStore(t *testing.T) *Store {
	t.Helper()
	return openTempStore(t)
}

func enqueue(t *testing.T, s *Store, kind, key, partition string) int64 {
	t.Helper()
	id, _, err := s.EnqueueTask(Task{Kind: kind, IdempotencyKey: key, PartitionKey: partition})
	if err != nil {
		t.Fatalf("enqueue %s: %v", key, err)
	}
	return id
}

// Enqueue is exactly-once. The same key twice is one task — the property that
// makes a retried agent turn safe to re-run.
func TestEnqueueIsIdempotent(t *testing.T) {
	s := taskStore(t)

	id1, created1, err := s.EnqueueTask(Task{Kind: "chat_turn", IdempotencyKey: "telegram:42:7"})
	if err != nil || !created1 {
		t.Fatalf("first enqueue: id=%d created=%v err=%v", id1, created1, err)
	}
	id2, created2, err := s.EnqueueTask(Task{Kind: "chat_turn", IdempotencyKey: "telegram:42:7"})
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if created2 {
		t.Error("second enqueue with the same key must not create a task")
	}
	if id2 != id1 {
		t.Errorf("second enqueue returned id %d, want the existing %d", id2, id1)
	}
}

// A redelivery after the work already ran must not re-run it.
func TestEnqueueOfCompletedKeyDoesNotRequeue(t *testing.T) {
	s := taskStore(t)
	id := enqueue(t, s, "chat_turn", "telegram:42:8", "")

	if _, err := s.LeaseTask("boot-1", time.Minute); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := s.CompleteTask(id, "answered"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if _, created, err := s.EnqueueTask(Task{Kind: "chat_turn", IdempotencyKey: "telegram:42:8"}); err != nil || created {
		t.Fatalf("a completed key must not requeue: created=%v err=%v", created, err)
	}
	got, err := s.GetTask(id)
	if err != nil || got.State != TaskDone {
		t.Fatalf("task state = %v (err %v), want done", got, err)
	}
}

func TestEnqueueRequiresKindAndKey(t *testing.T) {
	s := taskStore(t)
	if _, _, err := s.EnqueueTask(Task{IdempotencyKey: "k"}); err == nil {
		t.Error("a task without a kind must be rejected — nothing could handle it")
	}
	if _, _, err := s.EnqueueTask(Task{Kind: "chat_turn"}); err == nil {
		t.Error("a task without an idempotency key must be rejected — it could not dedup")
	}
}

// The core concurrency contract: same partition serializes, different
// partitions run together. This replaces the handler's chat mutex and the
// scheduler's mailboxes with one mechanism.
func TestPartitionSerializesAndIsolates(t *testing.T) {
	s := taskStore(t)
	enqueue(t, s, "chat_turn", "a", "chat:1:0")
	enqueue(t, s, "chat_turn", "b", "chat:1:0") // same partition — must wait
	enqueue(t, s, "chat_turn", "c", "chat:2:0") // different — must be available

	first, err := s.LeaseTask("boot-1", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first lease: %v (nil=%v)", err, first == nil)
	}
	if first.PartitionKey != "chat:1:0" {
		t.Fatalf("expected the oldest task first, got %q", first.PartitionKey)
	}

	second, err := s.LeaseTask("boot-1", time.Minute)
	if err != nil || second == nil {
		t.Fatalf("second lease: %v", err)
	}
	if second.PartitionKey == "chat:1:0" {
		t.Error("a second task in a busy partition must not be leased")
	}
	if second.PartitionKey != "chat:2:0" {
		t.Errorf("an idle partition must remain available, got %q", second.PartitionKey)
	}

	// chat:1:0 opens up only when its in-flight task finishes.
	if third, err := s.LeaseTask("boot-1", time.Minute); err != nil || third != nil {
		t.Fatalf("no task should be leasable while both partitions are busy, got %v", third)
	}
	if err := s.CompleteTask(first.ID, "done"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	third, err := s.LeaseTask("boot-1", time.Minute)
	if err != nil || third == nil || third.PartitionKey != "chat:1:0" {
		t.Fatalf("partition should reopen after completion, got %v (err %v)", third, err)
	}
}

// An empty partition key means no serialization constraint at all.
func TestUnpartitionedTasksRunConcurrently(t *testing.T) {
	s := taskStore(t)
	enqueue(t, s, "maintenance", "m1", "")
	enqueue(t, s, "maintenance", "m2", "")

	for i := 0; i < 2; i++ {
		got, err := s.LeaseTask("boot-1", time.Minute)
		if err != nil || got == nil {
			t.Fatalf("unpartitioned lease %d: %v (nil=%v)", i, err, got == nil)
		}
	}
}

// not_before is what the retired delegation system could not express: its
// hardcoded 60-minute TTL, swept every minute, always expired a 24h gate.
func TestNotBeforeHidesUntilDue(t *testing.T) {
	s := taskStore(t)
	future := time.Now().UTC().Add(time.Hour)
	if _, _, err := s.EnqueueTask(Task{
		Kind: "agent_prompt", IdempotencyKey: "later", NotBefore: &future,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	got, err := s.LeaseTask("boot-1", time.Minute)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if got != nil {
		t.Fatal("a task with a future not_before must not be leasable")
	}

	past := time.Now().UTC().Add(-time.Minute)
	if _, err := s.db.Exec(`UPDATE tasks SET not_before = ? WHERE idempotency_key = 'later'`, past); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got, err = s.LeaseTask("boot-1", time.Minute); err != nil || got == nil {
		t.Fatalf("a due task must be leasable: %v (nil=%v)", err, got == nil)
	}
}

// Failure returns work to the queue until attempts run out, then stops — a
// permanently broken task must not consume workers forever.
func TestFailRetriesThenGivesUp(t *testing.T) {
	s := taskStore(t)
	id, _, err := s.EnqueueTask(Task{Kind: "flaky", IdempotencyKey: "f1", MaxAttempts: 2})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if _, err := s.LeaseTask("boot-1", time.Minute); err != nil {
		t.Fatalf("lease 1: %v", err)
	}
	terminal, err := s.FailTask(id, "boom")
	if err != nil || terminal {
		t.Fatalf("first failure should be retryable: terminal=%v err=%v", terminal, err)
	}
	got, _ := s.GetTask(id)
	if got.State != TaskQueued {
		t.Fatalf("state after retryable failure = %q, want queued", got.State)
	}

	if _, err := s.LeaseTask("boot-1", time.Minute); err != nil {
		t.Fatalf("lease 2: %v", err)
	}
	terminal, err = s.FailTask(id, "boom again")
	if err != nil || !terminal {
		t.Fatalf("second failure should be terminal: terminal=%v err=%v", terminal, err)
	}
	got, _ = s.GetTask(id)
	if got.State != TaskFailed {
		t.Fatalf("state after exhausting attempts = %q, want failed", got.State)
	}
	if next, err := s.LeaseTask("boot-1", time.Minute); err != nil || next != nil {
		t.Fatal("a terminally failed task must not be leasable again")
	}
}

// A lease held by a dead process is reclaimable. This is the point of leases:
// "owner is gone" is knowable, where "has it been long enough?" is a guess.
func TestReclaimReturnsLeasesFromDeadOwners(t *testing.T) {
	s := taskStore(t)
	enqueue(t, s, "chat_turn", "orphan", "chat:9:0")

	if _, err := s.LeaseTask("boot-OLD", time.Hour); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if got, err := s.LeaseTask("boot-NEW", time.Minute); err != nil || got != nil {
		t.Fatal("the partition should be blocked while the old lease stands")
	}

	n, err := s.ReclaimTasks("boot-NEW")
	if err != nil || n != 1 {
		t.Fatalf("reclaim n=%d err=%v, want 1", n, err)
	}
	got, err := s.LeaseTask("boot-NEW", time.Minute)
	if err != nil || got == nil {
		t.Fatalf("reclaimed task must be leasable: %v (nil=%v)", err, got == nil)
	}
	if got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 — a reclaim is a retry and must count", got.Attempts)
	}
}

// A live owner's unexpired lease must survive its own reclaim sweep, or a
// restart would yank work out from under itself.
func TestReclaimSparesCurrentOwnersLiveLease(t *testing.T) {
	s := taskStore(t)
	enqueue(t, s, "chat_turn", "mine", "chat:9:0")
	if _, err := s.LeaseTask("boot-1", time.Hour); err != nil {
		t.Fatalf("lease: %v", err)
	}

	n, err := s.ReclaimTasks("boot-1")
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if n != 0 {
		t.Fatalf("reclaimed %d live leases from the current owner, want 0", n)
	}
}

// An expired lease is reclaimable even from the same owner — that covers a
// handler that hung rather than a process that died.
func TestReclaimTakesExpiredLeasesFromSameOwner(t *testing.T) {
	s := taskStore(t)
	enqueue(t, s, "chat_turn", "hung", "chat:9:0")
	if _, err := s.LeaseTask("boot-1", time.Hour); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE tasks SET leased_until = ?`,
		time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("expire: %v", err)
	}

	n, err := s.ReclaimTasks("boot-1")
	if err != nil || n != 1 {
		t.Fatalf("expired lease reclaim n=%d err=%v, want 1", n, err)
	}
}

func TestDeriveIdempotencyKeyIsStableAndDistinct(t *testing.T) {
	a := DeriveIdempotencyKey("agent_prompt", "check the plants", "2026-08-02T09:00Z")
	b := DeriveIdempotencyKey("agent_prompt", "check   the\tplants", "2026-08-02T09:00Z")
	if a != b {
		t.Error("whitespace differences must not produce a different key — a retyped prompt is the same work")
	}
	c := DeriveIdempotencyKey("agent_prompt", "check the plants", "2026-08-03T09:00Z")
	if a == c {
		t.Error("a different not_before must produce a different key — tomorrow's check is new work")
	}
}

func TestListTasksFiltersByState(t *testing.T) {
	s := taskStore(t)
	id := enqueue(t, s, "k", "one", "")
	enqueue(t, s, "k", "two", "p2")
	if _, err := s.LeaseTask("boot-1", time.Minute); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := s.CompleteTask(id, "ok"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	done, err := s.ListTasks(TaskDone, 10)
	if err != nil || len(done) != 1 || done[0].Result != "ok" {
		t.Fatalf("done list = %+v (err %v)", done, err)
	}
	all, err := s.ListTasks("", 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("unfiltered list returned %d, want 2 (err %v)", len(all), err)
	}
}

// The legacy-table guard must never destroy data. The drop is safe only
// because the old table is provably empty on both live agents (no
// sqlite_sequence row means no row was ever inserted); a DB that somehow used
// it must keep its rows and fail loudly instead.
func TestLegacyTaskTableWithRowsIsNotDropped(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/legacy.db"

	// Build a DB carrying the pre-queue shape with a row in it.
	seed, err := openRawForTest(path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	if _, err := seed.Exec(`CREATE TABLE tasks (
		id INTEGER PRIMARY KEY AUTOINCREMENT, chat_id INTEGER NOT NULL,
		description TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, completed_at DATETIME)`); err != nil {
		t.Fatalf("seed schema: %v", err)
	}
	if _, err := seed.Exec(`INSERT INTO tasks (chat_id, description) VALUES (1, 'do not lose me')`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	seed.Close()

	s, err := Open(path)
	if err != nil {
		// Migration failing loudly is an acceptable outcome; silent data loss is not.
		t.Logf("Open returned %v — acceptable so long as the row survived", err)
	} else {
		defer s.Close()
	}

	check, err := openRawForTest(path)
	if err != nil {
		t.Fatalf("verify open: %v", err)
	}
	defer check.Close()
	var desc string
	if err := check.QueryRow(`SELECT description FROM tasks WHERE id = 1`).Scan(&desc); err != nil {
		t.Fatalf("the legacy row was destroyed: %v", err)
	}
	if desc != "do not lose me" {
		t.Fatalf("legacy row altered: %q", desc)
	}
}

// Contention regression. The first implementation opened a transaction, read
// for the idempotency key, then inserted — and leased the same way. A soak
// against a copy of a live DB failed 107 of 320 concurrent enqueues with
// SQLITE_BUSY and LOST 3 of 40 distinct tasks: SQLite answers a read-to-write
// upgrade with an immediate busy error rather than honoring busy_timeout, and
// that holds however short the transaction is.
//
// Both paths are now single write statements (INSERT ... ON CONFLICT, and
// UPDATE ... WHERE id = (SELECT ...) RETURNING). This test fails loudly if
// either reverts to read-then-write.
func TestConcurrentEnqueueAndLeaseLoseNothing(t *testing.T) {
	s := taskStore(t)

	const producers, distinct, partitions = 24, 60, 4
	var created, deduped, enqErr, leaseErr atomic.Int64

	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < distinct; i++ {
				_, isNew, err := s.EnqueueTask(Task{
					Kind:           "soak",
					IdempotencyKey: fmt.Sprintf("soak:%d", i),
					PartitionKey:   fmt.Sprintf("part:%d", i%partitions),
				})
				switch {
				case err != nil:
					enqErr.Add(1)
				case isNew:
					created.Add(1)
				default:
					deduped.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if got := created.Load(); got != distinct {
		t.Fatalf("created %d of %d distinct tasks (errors=%d) — concurrent enqueue is losing work",
			got, distinct, enqErr.Load())
	}
	if enqErr.Load() != 0 {
		t.Errorf("%d enqueue errors under contention, want 0", enqErr.Load())
	}
	if want := int64(producers*distinct) - int64(distinct); deduped.Load() != want {
		t.Errorf("deduped %d, want %d — idempotency is not holding under contention", deduped.Load(), want)
	}

	// Drain concurrently, asserting the partition contract throughout.
	var mu sync.Mutex
	held := map[string]int{}
	var maxPer, done atomic.Int64

	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			owner := fmt.Sprintf("boot-%d", w)
			idle := 0
			for {
				task, err := s.LeaseTask(owner, 30*time.Second)
				if err != nil {
					leaseErr.Add(1)
					return
				}
				if task == nil {
					if idle++; idle > 3 {
						return
					}
					time.Sleep(2 * time.Millisecond)
					continue
				}
				idle = 0
				mu.Lock()
				held[task.PartitionKey]++
				if int64(held[task.PartitionKey]) > maxPer.Load() {
					maxPer.Store(int64(held[task.PartitionKey]))
				}
				mu.Unlock()

				mu.Lock()
				held[task.PartitionKey]--
				mu.Unlock()

				if err := s.CompleteTask(task.ID, "ok"); err != nil {
					leaseErr.Add(1)
					return
				}
				done.Add(1)
			}
		}(w)
	}
	wg.Wait()

	if leaseErr.Load() != 0 {
		t.Errorf("%d lease/complete errors under contention, want 0", leaseErr.Load())
	}
	if got := done.Load(); got != distinct {
		t.Errorf("completed %d of %d tasks", got, distinct)
	}
	if maxPer.Load() > 1 {
		t.Errorf("%d tasks ran concurrently in one partition — serialization broken", maxPer.Load())
	}
	if left, err := s.ListTasks(TaskQueued, 5); err != nil || len(left) != 0 {
		t.Errorf("%d tasks left queued after drain (err %v)", len(left), err)
	}
}

// Retention must sweep finished work and leave pending work alone. The sweep
// silently did neither for days: it was still written against the retired
// /task backlog's columns, so it failed with "no such column: status" every
// six hours and the queue accumulated without bound while looking maintained.
func TestCleanupRemovesOnlyFinishedTasks(t *testing.T) {
	s := taskStore(t)
	old := time.Now().UTC().Add(-72 * time.Hour)

	mk := func(key, state string, done *time.Time) int64 {
		t.Helper()
		id, _, err := s.EnqueueTask(Task{Kind: "k", IdempotencyKey: key})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE tasks SET state = ?, done_at = ? WHERE id = ?`, state, done, id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	oldDone := mk("done-old", TaskDone, &old)
	oldFailed := mk("failed-old", TaskFailed, &old)
	oldExpired := mk("expired-old", TaskExpired, &old)
	// A queued task is FUTURE work however old the row: one with not_before set
	// to next month is waiting, not stale.
	queued := mk("queued-old", TaskQueued, nil)
	leased := mk("leased-old", TaskLeased, nil)
	recent := time.Now().UTC().Add(-time.Minute)
	fresh := mk("done-fresh", TaskDone, &recent)

	n, err := s.CleanupCompletedTasks(24 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup failed: %v — this is the regression, the sweep must run at all", err)
	}
	if n != 3 {
		t.Fatalf("swept %d tasks, want 3 (done, failed, expired)", n)
	}
	for _, id := range []int64{oldDone, oldFailed, oldExpired} {
		if got, _ := s.GetTask(id); got != nil {
			t.Errorf("task %d survived the sweep", id)
		}
	}
	for _, id := range []int64{queued, leased, fresh} {
		if got, _ := s.GetTask(id); got == nil {
			t.Errorf("task %d was swept but should have been kept", id)
		}
	}
}
