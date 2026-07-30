package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Jitter window = min(15% of interval, 9min), floored at 5s.
func TestJitterWindowBounds(t *testing.T) {
	cases := []struct {
		interval time.Duration
		want     time.Duration
	}{
		{0, 5 * time.Second},                 // one-shot: floor only
		{time.Minute, 9 * time.Second},       // 15% of 1m
		{10 * time.Minute, 90 * time.Second}, // 15% of 10m
		{time.Hour, 9 * time.Minute},         // 15% of 1h = 9m, at the cap
		{24 * time.Hour, 9 * time.Minute},    // capped
		{10 * time.Second, 5 * time.Second},  // 15% = 1.5s → floored
	}
	for _, c := range cases {
		if got := jitterWindow(c.interval); got != c.want {
			t.Errorf("jitterWindow(%s) = %s, want %s", c.interval, got, c.want)
		}
	}
}

// Jitter delays the firing moment but may NEVER reach the following
// occurrence — otherwise a jittered fire could swallow or reorder the next one.
func TestJitterNeverOvershootsNextOccurrence(t *testing.T) {
	base := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	// Every-minute cron: the 9s window fits comfortably inside the interval.
	following := base.Add(time.Minute)
	for _, frac := range []float64{0, 0.25, 0.5, 0.9999} {
		fire := applyJitter(base, time.Minute, following, frac)
		if fire.Before(base) {
			t.Errorf("frac=%v: jitter must never move a fire earlier (%s < %s)", frac, fire, base)
		}
		if !fire.Before(following) {
			t.Errorf("frac=%v: jittered fire %s reached the following occurrence %s", frac, fire, following)
		}
		if d := fire.Sub(base); d > jitterWindow(time.Minute) {
			t.Errorf("frac=%v: delay %s exceeds the window %s", frac, d, jitterWindow(time.Minute))
		}
	}

	// Pathological case: interval shorter than the 5s jitter floor. The clamp
	// against `following` must still hold.
	tight := base.Add(3 * time.Second)
	fire := applyJitter(base, 3*time.Second, tight, 0.99)
	if !fire.Before(tight) {
		t.Errorf("tight interval: jittered fire %s must stay before %s", fire, tight)
	}
}

// The scheduler applies jitter to next_run_at while expected_next_at keeps the
// EXACT occurrence, so cron computation never drifts.
func TestSchedulerJitterKeepsExactExpectedNext(t *testing.T) {
	st := newMockStore(nil)
	s := New(st, func(int64, string) {}, nil, "UTC")
	s.SetJitterSource(func(int64) float64 { return 0.5 }) // deterministic

	now := time.Date(2026, 7, 29, 12, 0, 30, 0, time.UTC)
	entry := ScheduleEntry{ID: 11, ChatID: 1, Message: "m", Schedule: "*/10 * * * *",
		Type: "cron", Mode: "notify", Timezone: "UTC"}
	s.advance(entry, now)

	exact := st.expectedNext[11]
	fire := st.nextRuns[11]
	wantExact := time.Date(2026, 7, 29, 12, 10, 0, 0, time.UTC)
	if !exact.Equal(wantExact) {
		t.Errorf("expected_next_at must stay exact: got %s, want %s", exact, wantExact)
	}
	// 10-minute interval → 90s window → frac 0.5 → 45s.
	if want := exact.Add(45 * time.Second); !fire.Equal(want) {
		t.Errorf("jittered next_run_at = %s, want %s", fire, want)
	}
	if !fire.Before(exact.Add(10 * time.Minute)) {
		t.Error("jittered fire must stay before the following occurrence")
	}
}

// Same scheduler, same seed → same jitter. Determinism is required so tests
// (and replayed incidents) don't depend on wall-clock randomness.
func TestSeededJitterIsDeterministic(t *testing.T) {
	a, b := NewSeededJitter(1234), NewSeededJitter(1234)
	for i := 0; i < 5; i++ {
		x, y := a(int64(i)), b(int64(i))
		if x != y {
			t.Fatalf("seeded jitter diverged at %d: %v vs %v", i, x, y)
		}
		if x < 0 || x >= 1 {
			t.Fatalf("jitter fraction out of [0,1): %v", x)
		}
	}
}

// A fire that dies before spawning must leave a job_runs row — otherwise it is
// indistinguishable from a schedule that never came due.
func TestJobRunRecordedOnSpawnFailure(t *testing.T) {
	st := newMockStore([]ScheduleEntry{
		{ID: 21, ChatID: 100, Message: "go", Schedule: "@hourly", Type: "cron", Mode: "prompt", Timezone: "UTC"},
	})
	// No prompt handler wired at all → the fire cannot spawn.
	s := New(st, nil, nil, "UTC")
	tickSync(t, s)

	runs := st.outcomes(21)
	if len(runs) != 1 {
		t.Fatalf("want 1 ledger row for a failed fire, got %d", len(runs))
	}
	if runs[0].Outcome != OutcomeSpawnFailed {
		t.Errorf("outcome = %q, want %q", runs[0].Outcome, OutcomeSpawnFailed)
	}
}

// A turn that starts and then fails for a NON-transient reason is recorded as
// turn_failed with the error, and is not retried.
func TestJobRunRecordedOnTurnFailure(t *testing.T) {
	st := newMockStore([]ScheduleEntry{
		{ID: 22, ChatID: 100, Message: "go", Schedule: "@hourly", Type: "cron", Mode: "prompt", Timezone: "UTC"},
	})
	s := New(st, nil, func(context.Context, int64, string) error {
		return errors.New("skill returned malformed output")
	}, "UTC")
	tickSync(t, s)

	runs := st.outcomes(22)
	if len(runs) != 1 || runs[0].Outcome != OutcomeTurnFailed {
		t.Fatalf("want one turn_failed row and no retry, got %+v", runs)
	}
	if runs[0].ErrorMessage != "skill returned malformed output" {
		t.Errorf("error message not recorded: %q", runs[0].ErrorMessage)
	}
}

// A transient failure is retried up to the policy limit, and every attempt
// lands in the ledger with its own attempt number — a retried job must read as
// N attempts of one occurrence, not N unrelated runs.
func TestTransientFailureRetriesAndRecordsEachAttempt(t *testing.T) {
	st := newMockStore([]ScheduleEntry{
		{ID: 23, ChatID: 100, Message: "go", Schedule: "@hourly", Type: "cron", Mode: "prompt", Timezone: "UTC"},
	})
	var calls int
	s := New(st, nil, func(context.Context, int64, string) error {
		calls++
		if calls < 3 {
			return errors.New("429 rate limit")
		}
		return nil
	}, "UTC")
	s.SetRetryPolicy(RetryPolicy{MaxAttempts: 3, InitialInterval: time.Millisecond, BackoffCoeff: 1, MaxInterval: time.Millisecond})
	tickSync(t, s)

	runs := st.outcomes(23)
	if len(runs) != 3 {
		t.Fatalf("want 3 attempt rows, got %d: %+v", len(runs), runs)
	}
	for i, r := range runs {
		if r.Attempt != i+1 {
			t.Errorf("row %d: attempt = %d, want %d", i, r.Attempt, i+1)
		}
	}
	if runs[0].Outcome != OutcomeTurnFailed || runs[2].Outcome != OutcomeFiredOK {
		t.Errorf("want fail,fail,ok across attempts; got %s,%s,%s",
			runs[0].Outcome, runs[1].Outcome, runs[2].Outcome)
	}
}

// A permanent error must not burn the retry budget.
func TestRetryClassification(t *testing.T) {
	retryable := []string{"401 unauthorized", "429 rate limit", "upstream overloaded", "context deadline exceeded", "session busy"}
	permanent := []string{"no such skill", "invalid cron expression", "malformed json"}
	for _, m := range retryable {
		if !IsRetryable(errors.New(m)) {
			t.Errorf("%q should be retryable", m)
		}
	}
	for _, m := range permanent {
		if IsRetryable(errors.New(m)) {
			t.Errorf("%q should NOT be retryable — retrying a real bug wastes a turn", m)
		}
	}
}

// A successful notify fire is recorded too — the ledger must show both sides,
// or "no rows" would be ambiguous.
func TestJobRunRecordedOnSuccess(t *testing.T) {
	st := newMockStore([]ScheduleEntry{
		{ID: 23, ChatID: 100, Message: "hi", Type: "once", Mode: "notify", Timezone: "UTC"},
	})
	s := New(st, func(int64, string) {}, nil, "UTC")
	tickSync(t, s)

	runs := st.outcomes(23)
	if len(runs) != 1 || runs[0].Outcome != OutcomeFiredOK {
		t.Fatalf("want one fired_ok row, got %+v", runs)
	}
}

// Unrecoverable config errors auto-pause with a machine-readable reason instead
// of retrying every minute forever.
func TestAutoPauseRecordsReason(t *testing.T) {
	cases := []struct {
		name  string
		entry ScheduleEntry
		want  string
	}{
		{"bad cron", ScheduleEntry{ID: 31, ChatID: 1, Schedule: "not a cron", Type: "cron", Mode: "notify"}, PauseInvalidCron},
		{"bad interval", ScheduleEntry{ID: 32, ChatID: 1, Schedule: "banana", Type: "heartbeat", Mode: "prompt"}, PauseInvalidInterval},
	}
	for _, c := range cases {
		st := newMockStore(nil)
		s := New(st, func(int64, string) {}, nil, "UTC")
		s.advance(c.entry, time.Now().UTC())
		if !st.disabled[c.entry.ID] {
			t.Errorf("%s: schedule should be auto-paused", c.name)
		}
		if got := st.pauseReasons[c.entry.ID]; got != c.want {
			t.Errorf("%s: paused_reason = %q, want %q", c.name, got, c.want)
		}
	}
}

// A schedule with no target chat can never deliver — pause it rather than
// burning a fire attempt every minute.
func TestAutoPauseMissingChat(t *testing.T) {
	st := newMockStore(nil)
	s := New(st, func(int64, string) {}, nil, "UTC")
	// runJob, not execute: the ledger row is written by the retry wrapper, so
	// asserting on execute alone would miss the record entirely.
	s.runJob(context.Background(), ScheduleEntry{ID: 33, ChatID: 0, Message: "m", Type: "once", Mode: "notify"})

	if st.pauseReasons[33] != PauseMissingChat {
		t.Errorf("paused_reason = %q, want %q", st.pauseReasons[33], PauseMissingChat)
	}
	if runs := st.outcomes(33); len(runs) != 1 || runs[0].Outcome != OutcomeSpawnFailed {
		t.Errorf("a chat-less fire attempt must still be recorded, got %+v", runs)
	}
}

// Heartbeats run on chat 0 by design — the system chat — so the missing-chat
// guard must not touch them. Shipping without this exemption paused a live
// heartbeat on its first fire (7/29).
func TestAutoPauseMissingChat_HeartbeatExempt(t *testing.T) {
	st := newMockStore(nil)
	s := New(st, func(int64, string) {}, nil, "UTC")
	s.runJob(context.Background(), ScheduleEntry{ID: 8, ChatID: 0, Message: "beat", Type: "heartbeat", Mode: "prompt"})

	if reason, paused := st.pauseReasons[8]; paused {
		t.Errorf("heartbeat on chat 0 must not be paused, got reason %q", reason)
	}
}

// The dead-man's-switch flags a schedule whose last SUCCESSFUL run is older
// than interval x 2 — the only signal that catches silence.
func TestDeadMansSwitchThreshold(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	checks := []SilenceCheck{
		// Hourly, succeeded 30 min ago → healthy.
		{ID: 1, Label: "fresh", Type: "cron", Schedule: "@hourly", Timezone: "UTC", LastSuccessAt: now.Add(-30 * time.Minute)},
		// Hourly, succeeded 90 min ago → within 2x, still healthy.
		{ID: 2, Label: "one missed", Type: "cron", Schedule: "@hourly", Timezone: "UTC", LastSuccessAt: now.Add(-90 * time.Minute)},
		// Hourly, succeeded 5 hours ago → SILENT.
		{ID: 3, Label: "silent", Type: "cron", Schedule: "@hourly", Timezone: "UTC", LastSuccessAt: now.Add(-5 * time.Hour)},
		// Heartbeats are exempt (quiet hours + idle backoff make long legitimate
		// gaps) — never flagged, however stale.
		{ID: 4, Label: "quiet beat", Type: "heartbeat", Schedule: "30m", Timezone: "UTC", LastSuccessAt: now.Add(-3 * time.Hour)},
		// Irregular cron (weekend mornings): the 6-day gap is normal, so a
		// 4-day-old success must NOT be flagged.
		{ID: 8, Label: "weekends", Type: "cron", Schedule: "30 11 * * 0,6", Timezone: "UTC", LastSuccessAt: now.Add(-4 * 24 * time.Hour)},
		// One-shots have no interval and are never flagged.
		{ID: 5, Label: "one-shot", Type: "once", Schedule: "21:00", Timezone: "UTC", LastSuccessAt: now.Add(-100 * time.Hour)},
		// Never succeeded, created moments ago → not yet suspicious.
		{ID: 6, Label: "new", Type: "cron", Schedule: "@hourly", Timezone: "UTC", CreatedAt: now.Add(-5 * time.Minute)},
		// Never succeeded since creation a week ago → SILENT.
		{ID: 7, Label: "never fired", Type: "cron", Schedule: "@hourly", Timezone: "UTC", CreatedAt: now.Add(-7 * 24 * time.Hour)},
	}

	got := map[int64]bool{}
	for _, r := range DetectSilentSchedules(checks, now) {
		got[r.ID] = true
	}
	want := map[int64]bool{3: true, 7: true}
	for id := int64(1); id <= 8; id++ {
		if got[id] != want[id] {
			t.Errorf("schedule #%d: silent=%v, want %v", id, got[id], want[id])
		}
	}
}

// The preview is what makes a wrong expression visible at creation time.
func TestNextRunsPreview(t *testing.T) {
	from := time.Date(2026, 7, 29, 12, 30, 0, 0, time.UTC)

	runs, err := NextRuns("cron", "0 9 * * *", "UTC", time.Time{}, from, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("want 3 previewed fires, got %d", len(runs))
	}
	for i, r := range runs {
		if r.Hour() != 9 || r.Minute() != 0 {
			t.Errorf("fire %d = %s, want 09:00", i, r)
		}
		if i > 0 && !r.After(runs[i-1]) {
			t.Errorf("previewed fires must be strictly increasing: %s !> %s", r, runs[i-1])
		}
	}

	// Timezone-resolved, not UTC-shifted.
	la, err := NextRuns("cron", "0 9 * * *", "America/Los_Angeles", time.Time{}, from, 1)
	if err != nil {
		t.Fatal(err)
	}
	if la[0].Hour() != 9 {
		t.Errorf("preview must be resolved in the schedule's timezone, got %s", la[0])
	}

	// One-shot: exactly one remaining fire.
	once := from.Add(time.Hour)
	single, err := NextRuns("once", once.Format(time.RFC3339), "UTC", once, from, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(single) != 1 || !single[0].Equal(once) {
		t.Errorf("one-shot preview = %v, want [%s]", single, once)
	}
}
