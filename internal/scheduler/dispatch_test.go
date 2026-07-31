package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The defect this whole change exists for: a slow job must not hold up jobs for
// OTHER chats. Before dispatch, two schedules due together ran back to back on
// the tick goroutine — in production a job due at 09:00:09 was recorded 89
// seconds late because it waited out the one due at 09:00:00.
func TestSlowJobDoesNotBlockOtherChats(t *testing.T) {
	st := newMockStore([]ScheduleEntry{
		{ID: 1, ChatID: 100, Message: "slow", Schedule: "@hourly", Type: "cron", Mode: "prompt", Timezone: "UTC"},
		{ID: 2, ChatID: 200, Message: "fast", Schedule: "@hourly", Type: "cron", Mode: "prompt", Timezone: "UTC"},
	})

	release := make(chan struct{})
	fastDone := make(chan struct{})
	s := New(st, nil, func(_ context.Context, chatID int64, _ string) error {
		if chatID == 100 {
			<-release // hold the slow chat open
			return nil
		}
		close(fastDone)
		return nil
	}, "UTC")

	s.tick(context.Background())

	select {
	case <-fastDone:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("a job on chat 200 was blocked behind an in-flight job on chat 100")
	}
	close(release)
}

// The other half of the contract: within ONE chat, jobs must NOT overlap. Each
// chat is backed by a single Claude subprocess, so concurrent turns there
// contend for it.
func TestJobsSerializeWithinAChat(t *testing.T) {
	st := newMockStore(nil)

	var mu sync.Mutex
	var concurrent, maxConcurrent int
	s := New(st, nil, func(context.Context, int64, string) error {
		mu.Lock()
		concurrent++
		if concurrent > maxConcurrent {
			maxConcurrent = concurrent
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		concurrent--
		mu.Unlock()
		return nil
	}, "UTC")

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		entry := ScheduleEntry{ID: int64(100 + i), ChatID: 42, Message: "m", Type: "once", Mode: "prompt"}
		go func() {
			defer wg.Done()
			s.dispatch.submit(ctx, entry, OverlapAllow, s.runJob)
		}()
	}
	wg.Wait()

	waitFor(t, "jobs to drain", s.dispatch.idle)

	mu.Lock()
	defer mu.Unlock()
	if maxConcurrent > 1 {
		t.Fatalf("jobs for one chat ran %d at a time; they share one Claude subprocess and must serialize", maxConcurrent)
	}
}

// Skip is the heartbeat default: a beat that comes due while the previous one
// is still running is dropped, not queued. A doubled heartbeat is chat noise.
func TestOverlapSkipDropsSecondFire(t *testing.T) {
	st := newMockStore(nil)
	release := make(chan struct{})
	var started atomic.Int32

	s := New(st, nil, func(context.Context, int64, string) error {
		started.Add(1)
		<-release
		return nil
	}, "UTC")
	// Heartbeats are suppressed during quiet hours, so a test that fires one
	// must disable them — otherwise it passes by day and hangs by night.
	s.SetQuietHours(0, 0)

	ctx := context.Background()
	entry := ScheduleEntry{ID: 9, ChatID: 7, Message: "beat", Type: "heartbeat", Mode: "prompt"}

	if ok := s.dispatch.submit(ctx, entry, OverlapSkip, s.runJob); !ok {
		t.Fatal("first fire should be accepted")
	}
	waitFor(t, "first fire to occupy the worker", func() bool { return started.Load() > 0 })
	if ok := s.dispatch.submit(ctx, entry, OverlapSkip, s.runJob); ok {
		t.Error("second fire should have been skipped while the first is in flight")
	}

	close(release)
	waitFor(t, "dispatch to drain", s.dispatch.idle)
	if n := started.Load(); n != 1 {
		t.Errorf("skip policy ran %d jobs, want 1", n)
	}
}

// BufferOne lets exactly one fire wait behind the running one; a third is
// refused so a slow schedule cannot build a backlog it will never drain.
func TestOverlapBufferOneAllowsExactlyOneWaiter(t *testing.T) {
	st := newMockStore(nil)
	release := make(chan struct{})
	var started atomic.Int32

	s := New(st, nil, func(context.Context, int64, string) error {
		started.Add(1)
		<-release
		return nil
	}, "UTC")

	ctx := context.Background()
	entry := ScheduleEntry{ID: 10, ChatID: 8, Message: "remind", Type: "cron", Mode: "prompt"}

	if !s.dispatch.submit(ctx, entry, OverlapBufferOne, s.runJob) {
		t.Fatal("first fire should be accepted")
	}
	waitFor(t, "first fire to occupy the worker", func() bool { return started.Load() > 0 })
	if !s.dispatch.submit(ctx, entry, OverlapBufferOne, s.runJob) {
		t.Error("second fire should buffer behind the first")
	}
	if s.dispatch.submit(ctx, entry, OverlapBufferOne, s.runJob) {
		t.Error("third fire should be refused — buffer_one means one waiter")
	}

	close(release)
	waitFor(t, "dispatch to drain", s.dispatch.idle)
}

// An overlap-declined fire must leave a ledger row. A silently dropped fire is
// indistinguishable from a schedule that never came due — the exact failure
// mode the ledger exists to eliminate.
func TestSkippedOverlapIsRecorded(t *testing.T) {
	st := newMockStore([]ScheduleEntry{
		{ID: 11, ChatID: 9, Message: "beat", Schedule: "1h", Type: "heartbeat", Mode: "prompt", Timezone: "UTC"},
	})
	release := make(chan struct{})
	var started atomic.Int32
	s := New(st, nil, nil, "UTC")
	s.SetQuietHours(0, 0)
	s.SetHeartbeatPrompt(func(context.Context, int64, string) (string, error) {
		started.Add(1)
		<-release
		return "something to say", nil
	})

	ctx := context.Background()
	s.tick(ctx) // first fire occupies the worker
	waitFor(t, "first fire to occupy the worker", func() bool { return started.Load() > 0 })
	s.tick(ctx) // second fire is declined by the skip policy

	close(release)
	waitFor(t, "dispatch to drain", s.dispatch.idle)

	var skipped int
	for _, r := range st.outcomes(11) {
		if r.Outcome == OutcomeSkippedOverlap {
			skipped++
		}
	}
	if skipped != 1 {
		t.Fatalf("want 1 skipped_overlap row, got %d in %+v", skipped, st.outcomes(11))
	}
}

// A wedged turn must not hold its chat's queue forever.
func TestJobTimeoutCancelsTheTurn(t *testing.T) {
	st := newMockStore(nil)
	s := New(st, nil, func(ctx context.Context, _ int64, _ string) error {
		<-ctx.Done() // a handler that respects cancellation
		return ctx.Err()
	}, "UTC")
	s.SetJobTimeout(50 * time.Millisecond)
	s.SetRetryPolicy(RetryPolicy{MaxAttempts: 1})

	done := make(chan struct{})
	go func() {
		s.runJob(context.Background(), ScheduleEntry{ID: 12, ChatID: 5, Message: "hang", Type: "once", Mode: "prompt"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("job timeout did not cancel a hung turn")
	}

	runs := st.outcomes(12)
	if len(runs) != 1 || runs[0].Outcome != OutcomeTurnFailed {
		t.Fatalf("want one turn_failed row from the timeout, got %+v", runs)
	}
}

// Backoff grows geometrically and then holds at the ceiling.
func TestRetryBackoffProgression(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 5, InitialInterval: time.Second, BackoffCoeff: 3, MaxInterval: 10 * time.Second}
	want := []time.Duration{0, 0, time.Second, 3 * time.Second, 9 * time.Second, 10 * time.Second}
	for attempt := 0; attempt <= 5; attempt++ {
		if got := p.Backoff(attempt); got != want[attempt] {
			t.Errorf("Backoff(%d) = %s, want %s", attempt, got, want[attempt])
		}
	}
}

func TestOverlapPolicyDefaults(t *testing.T) {
	if got := ParseOverlapPolicy("", "heartbeat"); got != OverlapSkip {
		t.Errorf("heartbeat default = %q, want skip", got)
	}
	if got := ParseOverlapPolicy("", "cron"); got != OverlapBufferOne {
		t.Errorf("cron default = %q, want buffer_one", got)
	}
	if got := ParseOverlapPolicy("garbage", "cron"); got != OverlapBufferOne {
		t.Errorf("unrecognized policy must fall back to the type default, got %q", got)
	}
	if got := ParseOverlapPolicy("ALLOW", "heartbeat"); got != OverlapAllow {
		t.Errorf("policy parsing should be case-insensitive, got %q", got)
	}
}

// waitFor blocks until cond holds, failing the test rather than hanging. Every
// wait in this file is bounded: a condition that never becomes true is a bug to
// report, not a reason to burn the package timeout.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
