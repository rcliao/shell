package scheduler

import (
	"context"
	"encoding/json"
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

// mustChat decodes a leased task's fire payload and returns its chat. Leases
// are generic now — the payload is opaque bytes until a handler reads it — so
// tests that care about the fire inside must decode it the same way.
func mustChat(t *testing.T, lt *LeasedTask) int64 {
	t.Helper()
	e, err := decodeFirePayload(lt.Payload)
	if err != nil {
		t.Fatalf("undecodable fire payload %q: %v", lt.Payload, err)
	}
	return e.ChatID
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
	got, err := q.LeaseNext("boot-A", time.Hour)
	if err != nil || got == nil {
		t.Fatalf("lease returned (%v, %v), want a fire", got, err)
	}
	if func() bool { e, _ := decodeFirePayload(got.Payload); return e.ID != 7 || e.Message != "check in" }() {
		t.Fatalf("payload did not round-trip: %s", got.Payload)
	}

	// Process B boots. Nothing is leasable until it reclaims — the lease is
	// still live from the table's point of view.
	if again, err := q.LeaseNext("boot-B", time.Hour); err != nil || again != nil {
		t.Fatalf("a live lease must not be handed to a second worker (got %v, %v)", again, err)
	}

	n, err := q.ReclaimFires("boot-B")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d fires, want 1 — work abandoned by a dead process is lost", n)
	}

	replay, err := q.LeaseNext("boot-B", time.Hour)
	if err != nil || replay == nil {
		t.Fatalf("reclaimed fire was not re-leasable (got %v, %v)", replay, err)
	}
	if func() bool { e, _ := decodeFirePayload(replay.Payload); return e.Message != "check in" }() {
		t.Errorf("replayed payload lost: %s", replay.Payload)
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
	if got, err := q.LeaseNext("boot-A", time.Hour); err != nil || got != nil {
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
	if _, err := q.LeaseNext("boot-A", time.Hour); err != nil {
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

	first, err := q.LeaseNext("boot-A", time.Hour)
	if err != nil || first == nil {
		t.Fatalf("lease 1 = %v, %v", first, err)
	}
	second, err := q.LeaseNext("boot-A", time.Hour)
	if err != nil || second == nil {
		t.Fatalf("lease 2 = %v, %v", second, err)
	}
	if mustChat(t, first) == mustChat(t, second) {
		t.Fatalf("two fires leased for chat %d at once — one subprocess, two turns",
			mustChat(t, first))
	}

	// Chat 100's second fire stays queued until the first completes.
	if third, err := q.LeaseNext("boot-A", time.Hour); err != nil || third != nil {
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

// The queue is generic infrastructure; scheduled fires are one producer among
// several. A task of an unknown kind must reach its registered handler, and a
// kind with NO handler must fail loudly rather than be silently dropped or
// retried forever — an unregistered kind is a wiring bug, not a transient one.
func TestWorkerDispatchesByKind(t *testing.T) {
	q, st := queueAdapter(t)
	s := New(newMockStore(nil), nil, nil, "UTC")
	s.SetQueue(q, "boot-test")

	var got LeasedTask
	s.RegisterHandler("custom.kind", func(_ context.Context, lt LeasedTask) (string, error) {
		got = lt
		return "handled", nil
	})

	if _, _, err := st.EnqueueTask(store.Task{
		Kind: "custom.kind", IdempotencyKey: "k1", Payload: `{"hello":"world"}`,
	}); err != nil {
		t.Fatal(err)
	}
	if !s.leaseAndRunOne(context.Background()) {
		t.Fatal("worker found no work")
	}
	if got.Kind != "custom.kind" || got.Payload != `{"hello":"world"}` {
		t.Fatalf("handler got %+v, want the enqueued kind and payload", got)
	}
	tasks, err := st.ListTasks(store.TaskDone, 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("expected one done task, got %d (%v)", len(tasks), err)
	}
	if tasks[0].Result != "handled" {
		t.Errorf("result = %q, want the handler's return — that is what an assigner reads back", tasks[0].Result)
	}

	// A kind nobody registered fails immediately.
	if _, _, err := st.EnqueueTask(store.Task{Kind: "nobody.handles.this", IdempotencyKey: "k2"}); err != nil {
		t.Fatal(err)
	}
	if !s.leaseAndRunOne(context.Background()) {
		t.Fatal("worker found no work for the unhandled kind")
	}
	pending, err := st.ListTasks(store.TaskQueued, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("an unregistered kind was left queued for retry: %+v", pending)
	}
}

// Completion must be EVIDENCE, not assertion. An agent narrating "done!" means
// nothing — agents in this system have reported saving things that were never
// saved, which is why write verification exists. A queued agent task is
// complete only when the agent actually called the completion tool, which
// writes a result the worker can read back.
func TestAgentTaskNeedsEvidenceNotNarration(t *testing.T) {
	q, st := queueAdapter(t)

	enqueue := func(key, prompt string) int64 {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"prompt": prompt, "chat_id": 42})
		id, _, err := st.EnqueueTask(store.Task{
			Kind: TaskKindAgent, IdempotencyKey: key,
			Payload: string(payload), MaxAttempts: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	// A turn that runs cleanly but never records a result must FAIL. Returning
	// cleanly says the subprocess exited, not that the work happened.
	silent := New(newMockStore(nil), nil, func(context.Context, int64, string) error {
		return nil // "I did it!" — and no completion call
	}, "UTC")
	silent.SetQueue(q, "boot-a")
	silent.SetAgentTaskHandler(st)

	id := enqueue("k-silent", "tidy something")
	if !silent.leaseAndRunOne(context.Background()) {
		t.Fatal("worker found no work")
	}
	got, err := st.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.TaskFailed {
		t.Fatalf("state = %q, want failed — a turn with no completion call is not done", got.State)
	}
	if got.LastError == "" {
		t.Error("failure recorded no reason")
	}

	// A turn that DOES call complete records its result and finishes.
	honest := New(newMockStore(nil), nil, func(_ context.Context, _ int64, _ string) error {
		return st.SetTaskResult(2, "filed the thing")
	}, "UTC")
	honest.SetQueue(q, "boot-b")
	honest.SetAgentTaskHandler(st)

	id2 := enqueue("k-honest", "file the thing")
	if id2 != 2 {
		t.Fatalf("expected task id 2, got %d", id2)
	}
	if !honest.leaseAndRunOne(context.Background()) {
		t.Fatal("worker found no work for the honest task")
	}
	got2, err := st.GetTask(id2)
	if err != nil {
		t.Fatal(err)
	}
	if got2.State != store.TaskDone {
		t.Fatalf("state = %q, want done", got2.State)
	}
	if got2.Result != "filed the thing" {
		t.Errorf("result = %q — this is what an assigner reads back", got2.Result)
	}
}

// Enqueueing must wake a worker rather than leaving the fire to be discovered
// on the next poll. The in-memory dispatcher handed work straight to a mailbox
// goroutine; moving to the queue quietly added a latency FLOOR of one poll
// interval to every scheduled fire, measured at a flat 5.0s on a production
// fire on 2026-08-06.
func TestEnqueueWakesAWorkerImmediately(t *testing.T) {
	q, _ := queueAdapter(t)
	entry := fireEntry(31, 77)
	entry.Type = "cron"
	entry.Schedule = "@hourly"
	entry.NextRunAt = time.Now().UTC().Add(-time.Minute)

	fired := make(chan struct{}, 1)
	s := New(newMockStore([]ScheduleEntry{entry}), nil, func(context.Context, int64, string) error {
		select {
		case fired <- struct{}{}:
		default:
		}
		return nil
	}, "UTC")
	s.SetQueue(q, "boot-wake")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.runQueueWorkers(ctx, 1)

	// Let the STARTUP drain finish against an empty queue and park the worker on
	// its select. Without this the initial drain races the tick and picks the
	// fire up with no wake involved — which made an earlier version of this test
	// pass even with the wake deliberately disabled.
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	s.tick(ctx)

	// The next poll tick is ~4.7s away, so anything under a second can only be
	// the wake.
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatalf("fire did not start within 1s of being enqueued — it waited for the %s poll tick",
			queuePollInterval)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Errorf("fire took %s to start; the wake should make it near-immediate", elapsed)
	}

	cancel()
	s.queueWG.Wait()
}

// Partitioning follows SESSION identity, which is (chat, thread) — one Claude
// subprocess per pair. A forum group runs one subprocess per topic, so keying
// on chat alone would queue independent conversations behind each other.
func TestPartitionKeyFollowsSessionIdentity(t *testing.T) {
	const group int64 = -100200300
	if a, b := PartitionKey(group, 111), PartitionKey(group, 222); a == b {
		t.Fatalf("two forum topics share partition %q — they own separate subprocesses and must run in parallel", a)
	}
	if a, b := PartitionKey(group, 0), PartitionKey(group, 0); a != b {
		t.Fatalf("the same conversation produced two partitions, %q and %q", a, b)
	}
	// Different chats never share a partition even at the same thread id.
	if a, b := PartitionKey(1, 0), PartitionKey(2, 0); a == b {
		t.Fatalf("two chats share partition %q", a)
	}
}
