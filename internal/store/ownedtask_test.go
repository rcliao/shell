package store

import (
	"testing"
	"time"
)

// A self-run task must be invisible to workers while its owner is alive —
// otherwise the handler running the turn inline and a worker leasing the same
// row would answer one message twice.
func TestOwnedTaskIsNotLeasableByItsOwner(t *testing.T) {
	s := testStore(t)
	owner := "boot-1-111"

	id, done, err := s.BeginOwnedTask(Task{
		Kind:           "message.turn",
		Source:         TaskSourceTelegram,
		IdempotencyKey: "telegram:-100200300:7",
		PartitionKey:   "chat:-100200300:0",
		Payload:        `{"text":"hi"}`,
		MaxAttempts:    2,
	}, owner, 30*time.Minute)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if done {
		t.Fatal("a brand new task reported as already done")
	}

	if got, err := s.LeaseTask(owner, time.Minute); err != nil {
		t.Fatalf("lease: %v", err)
	} else if got != nil {
		t.Fatalf("worker leased task %d while its owner was still running it", got.ID)
	}

	if err := s.CompleteOwnedTask("telegram:-100200300:7", ""); err != nil {
		t.Fatalf("complete: %v", err)
	}
	task, err := s.GetTask(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.State != TaskDone {
		t.Fatalf("state = %q, want done", task.State)
	}
}

// A redelivery of an already-answered message must be recognised and dropped.
// This is the guarantee pending_turns provided via its done flag, and losing it
// silently would mean answering the same message twice after a reconnect.
func TestOwnedTaskReportsRedelivery(t *testing.T) {
	s := testStore(t)
	owner := "boot-1-111"
	key := "telegram:-100200300:9"
	mk := func() (bool, error) {
		_, done, err := s.BeginOwnedTask(Task{
			Kind:           "message.turn",
			IdempotencyKey: key,
			PartitionKey:   "chat:-100200300:0",
			MaxAttempts:    2,
		}, owner, 30*time.Minute)
		return done, err
	}

	if done, err := mk(); err != nil || done {
		t.Fatalf("first begin: done=%v err=%v", done, err)
	}
	// Redelivered while still running: not done, so it is not dropped.
	if done, err := mk(); err != nil || done {
		t.Fatalf("in-flight redelivery: done=%v err=%v", done, err)
	}
	if err := s.CompleteOwnedTask(key, ""); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Redelivered after answering: dropped.
	if done, err := mk(); err != nil || !done {
		t.Fatalf("post-answer redelivery: done=%v err=%v, want done=true", done, err)
	}
}

// The crash case: the process that owned the turn is gone, so a NEW owner must
// be able to reclaim the row and replay it. A boot owner differs across
// restarts even when the PID is reused, which is what makes this work after a
// SIGHUP exec.
func TestOwnedTaskIsReclaimedByANewOwner(t *testing.T) {
	s := testStore(t)
	dead := "boot-1-111"
	alive := "boot-1-222"

	if _, _, err := s.BeginOwnedTask(Task{
		Kind:           "message.turn",
		IdempotencyKey: "telegram:-100200300:11",
		PartitionKey:   "chat:-100200300:0",
		MaxAttempts:    2,
	}, dead, 30*time.Minute); err != nil {
		t.Fatalf("begin: %v", err)
	}

	n, err := s.ReclaimTasks(alive)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d tasks, want 1", n)
	}

	got, err := s.LeaseTask(alive, time.Minute)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if got == nil {
		t.Fatal("reclaimed task was not leasable by the new owner")
	}
	// The inline run was attempt 1, so the replay is attempt 2 — and with
	// max_attempts 2 that is the last one.
	if got.Attempts != 2 {
		t.Fatalf("replay attempts = %d, want 2", got.Attempts)
	}
}

// Attempts must stay bounded across the inline run plus replays: a crash loop
// must not re-run a real inference forever.
func TestOwnedTaskStopsAfterItsAttemptBudget(t *testing.T) {
	s := testStore(t)
	if _, _, err := s.BeginOwnedTask(Task{
		Kind:           "message.turn",
		IdempotencyKey: "telegram:-100200300:13",
		PartitionKey:   "chat:-100200300:0",
		MaxAttempts:    2,
	}, "boot-1-111", 30*time.Minute); err != nil {
		t.Fatalf("begin: %v", err)
	}

	// Second boot reclaims and replays (attempt 2).
	if _, err := s.ReclaimTasks("boot-1-222"); err != nil {
		t.Fatalf("reclaim 2: %v", err)
	}
	if got, err := s.LeaseTask("boot-1-222", time.Minute); err != nil || got == nil {
		t.Fatalf("replay lease: task=%v err=%v", got, err)
	}

	// Third boot: the budget is spent, so it must be retired, not run again.
	if _, err := s.ReclaimTasks("boot-1-333"); err != nil {
		t.Fatalf("reclaim 3: %v", err)
	}
	if got, err := s.LeaseTask("boot-1-333", time.Minute); err != nil {
		t.Fatalf("lease 3: %v", err)
	} else if got != nil {
		t.Fatalf("task ran a third time on attempt %d, past its budget", got.Attempts)
	}
}

// An abandoned turn must become runnable again immediately — the whole point
// is that a worker replays it in seconds instead of at the next restart — and
// it must stop holding its partition while it waits.
func TestAbandonedOwnedTaskIsRunnableAgainAndFreesItsPartition(t *testing.T) {
	s := testStore(t)
	owner := "boot-1-111"
	key := "telegram:-100200300:15"

	if _, _, err := s.BeginOwnedTask(Task{
		Kind:           "message.turn",
		IdempotencyKey: key,
		PartitionKey:   "chat:-100200300:0",
		MaxAttempts:    2,
	}, owner, 30*time.Minute); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// A second task in the same conversation — a scheduled fire, say — must be
	// blocked while the turn holds the partition.
	if _, _, err := s.EnqueueTask(Task{
		Kind:           "schedule.fire",
		IdempotencyKey: "fire:1",
		PartitionKey:   "chat:-100200300:0",
	}); err != nil {
		t.Fatalf("enqueue fire: %v", err)
	}
	if got, err := s.LeaseTask(owner, time.Minute); err != nil {
		t.Fatalf("lease while held: %v", err)
	} else if got != nil {
		t.Fatalf("leased %q while the turn held the partition", got.Kind)
	}

	if err := s.AbandonOwnedTask(key, "no placeholder", owner); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	got, err := s.LeaseTask(owner, time.Minute)
	if err != nil {
		t.Fatalf("lease after abandon: %v", err)
	}
	if got == nil {
		t.Fatal("nothing leasable after abandon; the partition is still blocked")
	}
	if got.Kind != "message.turn" {
		t.Fatalf("leased %q first; the abandoned turn should be replayed ahead of later work", got.Kind)
	}
	if got.Attempts != 2 {
		t.Fatalf("replay attempts = %d, want 2 (the inline run already counted)", got.Attempts)
	}
}

// Abandon must only touch rows this owner still holds: a task already reclaimed
// by a newer boot belongs to that boot.
func TestAbandonIgnoresATaskOwnedByANewerBoot(t *testing.T) {
	s := testStore(t)
	key := "telegram:-100200300:17"
	if _, _, err := s.BeginOwnedTask(Task{
		Kind:           "message.turn",
		IdempotencyKey: key,
		PartitionKey:   "chat:-100200300:0",
		MaxAttempts:    2,
	}, "boot-1-111", 30*time.Minute); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := s.ReclaimTasks("boot-1-222"); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	leased, err := s.LeaseTask("boot-1-222", time.Minute)
	if err != nil || leased == nil {
		t.Fatalf("new boot lease: task=%v err=%v", leased, err)
	}
	// The dead boot's handler unwinding must not disturb the live replay.
	if err := s.AbandonOwnedTask(key, "stale", "boot-1-111"); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	after, err := s.GetTask(leased.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.State != TaskLeased || after.LeaseOwner != "boot-1-222" {
		t.Fatalf("state=%q owner=%q; a stale abandon stole a live replay", after.State, after.LeaseOwner)
	}
}
