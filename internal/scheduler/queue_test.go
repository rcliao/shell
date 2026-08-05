package scheduler

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/store"
)

func queueAdapter(t *testing.T) (*StoreAdapter, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return NewStoreAdapter(st), st
}

// Occurrences are anchored to the real clock, not a fixed date: expiry is
// evaluated against now, so a hardcoded 2026 timestamp would be swept as stale
// the moment the wall clock passed it.
func fireEntry(id, chat int64) ScheduleEntry {
	return ScheduleEntry{
		ID: id, ChatID: chat, Label: "beat", Message: "check in",
		Schedule: "1h", Timezone: "UTC", Type: "heartbeat", Mode: "prompt",
		NextRunAt: time.Now().UTC().Add(-time.Minute),
	}
}

// THE defect this step exists for. A fire in flight when the process dies used
// to vanish: job_runs recorded `interrupted` and nothing ever picked it up. On
// 2026-08-01 that discarded a deep reflection beat after 340 seconds of work.
//
// A queued fire must survive. The new process reclaims the dead owner's lease
// and runs the occurrence — with the ORIGINAL payload, since by then the
// schedule row has advanced past it.
func TestFireSurvivesProcessDeath(t *testing.T) {
	q, _ := queueAdapter(t)
	entry := fireEntry(7, 42)
	occurrence := entry.NextRunAt

	if _, err := q.EnqueueFire(entry, occurrence, occurrence.Add(time.Hour), fireMaxAttempts); err != nil {
		t.Fatal(err)
	}

	// Process A leases it, then dies without completing.
	got, err := q.LeaseFire("boot-A", time.Hour)
	if err != nil || got == nil {
		t.Fatalf("lease returned (%v, %v), want a fire", got, err)
	}
	if got.Entry.ID != 7 || got.Entry.Message != "check in" {
		t.Fatalf("payload did not round-trip: %+v", got.Entry)
	}

	// Process B boots. Nothing is leasable until it reclaims — the lease is
	// still live from the table's point of view.
	if again, err := q.LeaseFire("boot-B", time.Hour); err != nil || again != nil {
		t.Fatalf("a live lease must not be handed to a second worker (got %v, %v)", again, err)
	}

	n, err := q.ReclaimFires("boot-B")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d fires, want 1 — work abandoned by a dead process is lost", n)
	}

	replay, err := q.LeaseFire("boot-B", time.Hour)
	if err != nil || replay == nil {
		t.Fatalf("reclaimed fire was not re-leasable (got %v, %v)", replay, err)
	}
	if replay.Entry.Message != "check in" {
		t.Errorf("replayed payload lost: %+v", replay.Entry)
	}
	if replay.Attempt != 2 {
		t.Errorf("replay attempt = %d, want 2 — attempts must count across processes", replay.Attempt)
	}
}

// Replay has to know when to stop. A beat reclaimed long after it was due would
// report on a world that has moved on, so past its expiry it is dropped with a
// recorded outcome instead of run late.
func TestStaleFireIsDroppedNotRun(t *testing.T) {
	q, st := queueAdapter(t)
	entry := fireEntry(9, 42)
	past := time.Now().UTC().Add(-2 * time.Hour)

	if _, err := q.EnqueueFire(entry, past, past.Add(time.Minute), fireMaxAttempts); err != nil {
		t.Fatal(err)
	}

	// Even before the sweep runs, a lease must not hand out expired work — the
	// two race, and starting a stale beat is the exact failure expiry prevents.
	if got, err := q.LeaseFire("boot-A", time.Hour); err != nil || got != nil {
		t.Fatalf("leased an expired fire (%v, %v)", got, err)
	}

	n, err := q.ExpireFires()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expired %d fires, want 1", n)
	}

	counts, err := st.CountTasksByState()
	if err != nil {
		t.Fatal(err)
	}
	if counts[store.TaskExpired] != 1 {
		t.Errorf("state counts = %v, want one expired", counts)
	}
	// Expired is NOT failed: nothing went wrong, the work stopped being worth
	// doing. Conflating them would hide a real failure rate behind staleness.
	if counts[store.TaskFailed] != 0 {
		t.Errorf("stale work was recorded as a failure: %v", counts)
	}
}

// A fire still in flight must block the next occurrence of the SAME schedule
// under the skip policy — and unlike the dispatcher's in-memory maps, that has
// to hold across a restart, since the queue is now where overlap lives.
func TestOverlapIsAnsweredByTheQueue(t *testing.T) {
	q, _ := queueAdapter(t)
	entry := fireEntry(11, 42)
	first := entry.NextRunAt

	if _, err := q.EnqueueFire(entry, first, first.Add(time.Hour), fireMaxAttempts); err != nil {
		t.Fatal(err)
	}
	if n, err := q.CountActiveFires(11); err != nil || n != 1 {
		t.Fatalf("active fires = %d (%v), want 1 while queued", n, err)
	}
	if _, err := q.LeaseFire("boot-A", time.Hour); err != nil {
		t.Fatal(err)
	}
	if n, err := q.CountActiveFires(11); err != nil || n != 1 {
		t.Fatalf("active fires = %d (%v), want 1 while leased", n, err)
	}
	// A different schedule must not be counted.
	if n, err := q.CountActiveFires(12); err != nil || n != 0 {
		t.Fatalf("active fires for an unrelated schedule = %d (%v), want 0", n, err)
	}

	// Once it finishes, the next occurrence is free to queue.
	tasks, err := q.s.ListTasks(store.TaskLeased, 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("expected one leased task, got %d (%v)", len(tasks), err)
	}
	if err := q.CompleteFire(tasks[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	if n, err := q.CountActiveFires(11); err != nil || n != 0 {
		t.Fatalf("active fires after completion = %d (%v), want 0", n, err)
	}
}

// The same occurrence offered twice — a duplicate tick, or a restart replaying
// a due row — must produce ONE fire. Two DIFFERENT occurrences of the same
// schedule must both run: identical beats an hour apart are not duplicates.
func TestOccurrenceIsTheIdempotencyUnit(t *testing.T) {
	q, _ := queueAdapter(t)
	entry := fireEntry(13, 42)
	occ := entry.NextRunAt

	created, err := q.EnqueueFire(entry, occ, occ.Add(time.Hour), fireMaxAttempts)
	if err != nil || !created {
		t.Fatalf("first enqueue created=%v err=%v", created, err)
	}
	created, err = q.EnqueueFire(entry, occ, occ.Add(time.Hour), fireMaxAttempts)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("the same occurrence enqueued twice created two fires")
	}

	next := occ.Add(time.Hour)
	created, err = q.EnqueueFire(entry, next, next.Add(time.Hour), fireMaxAttempts)
	if err != nil || !created {
		t.Fatalf("a later occurrence of the same schedule was suppressed (created=%v err=%v)", created, err)
	}
}

// Partitioning is what keeps one chat's Claude subprocess from being asked to
// run two turns at once, while different chats still fire in parallel.
func TestFiresSerializePerChatAndRunAcrossChats(t *testing.T) {
	q, _ := queueAdapter(t)
	base := time.Now().UTC().Add(-time.Minute)

	for i, e := range []ScheduleEntry{fireEntry(1, 100), fireEntry(2, 100), fireEntry(3, 200)} {
		occ := base.Add(time.Duration(i) * time.Minute)
		if _, err := q.EnqueueFire(e, occ, occ.Add(time.Hour), fireMaxAttempts); err != nil {
			t.Fatal(err)
		}
	}

	first, err := q.LeaseFire("boot-A", time.Hour)
	if err != nil || first == nil {
		t.Fatalf("lease 1 = %v, %v", first, err)
	}
	second, err := q.LeaseFire("boot-A", time.Hour)
	if err != nil || second == nil {
		t.Fatalf("lease 2 = %v, %v", second, err)
	}
	if first.Entry.ChatID == second.Entry.ChatID {
		t.Fatalf("two fires leased for chat %d at once — one subprocess, two turns",
			first.Entry.ChatID)
	}

	// Chat 100's second fire stays queued until the first completes.
	if third, err := q.LeaseFire("boot-A", time.Hour); err != nil || third != nil {
		t.Fatalf("third lease = %v, %v; want nothing (both partitions busy)", third, err)
	}
}

// End to end through the scheduler: with a queue attached, a due schedule is
// enqueued by the tick and executed by a worker rather than inline.
func TestSchedulerRunsFiresThroughTheQueue(t *testing.T) {
	q, _ := queueAdapter(t)
	entry := fireEntry(21, 42)
	entry.Type = "cron"
	entry.Schedule = "@hourly"
	entry.NextRunAt = time.Now().UTC().Add(-time.Minute)

	st := newMockStore([]ScheduleEntry{entry})

	var mu sync.Mutex
	fired := 0
	s := New(st, nil, func(context.Context, int64, string) error {
		mu.Lock()
		fired++
		mu.Unlock()
		return nil
	}, "UTC")
	s.SetQueue(q, "boot-test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.tick(ctx)

	// Nothing should have run inline — the tick only enqueues.
	mu.Lock()
	inline := fired
	mu.Unlock()
	if inline != 0 {
		t.Fatalf("tick fired %d jobs inline; with a queue it must only enqueue", inline)
	}

	s.runQueueWorkers(ctx, 1)
	waitFor(t, "queued fire to run", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return fired == 1
	})

	cancel()
	s.queueWG.Wait()
}
