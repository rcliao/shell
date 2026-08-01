package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type mockStore struct {
	mu           sync.Mutex
	schedules    []ScheduleEntry
	disabled     map[int64]bool
	nextRuns     map[int64]time.Time
	expectedNext map[int64]time.Time
	pauseReasons map[int64]string
	hbCounts     map[int64]int // schedule id → persisted heartbeat count (survives "restart")
	runs         []JobRun
}

func newMockStore(entries []ScheduleEntry) *mockStore {
	return &mockStore{
		schedules:    entries,
		disabled:     make(map[int64]bool),
		nextRuns:     make(map[int64]time.Time),
		expectedNext: make(map[int64]time.Time),
		pauseReasons: make(map[int64]string),
		hbCounts:     make(map[int64]int),
	}
}

func (m *mockStore) SetExpectedNextAt(id int64, expected time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expectedNext[id] = expected
	return nil
}

func (m *mockStore) PauseSchedule(id int64, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disabled[id] = true
	m.pauseReasons[id] = reason
	return nil
}

func (m *mockStore) RecordJobRun(run JobRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs = append(m.runs, run)
	return nil
}

// StartJobRun/FinishJobRun mirror the two-phase ledger: the row is appended
// open and its outcome patched in place, so tests see exactly one row per
// attempt with the terminal outcome — same shape as the real store.
func (m *mockStore) StartJobRun(run JobRun) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run.Outcome = "running"
	m.runs = append(m.runs, run)
	return int64(len(m.runs)), nil
}

func (m *mockStore) FinishJobRun(runID, scheduleID int64, firedAt time.Time, outcome, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := int(runID) - 1
	if idx < 0 || idx >= len(m.runs) {
		return errors.New("no such run")
	}
	m.runs[idx].Outcome = outcome
	m.runs[idx].ErrorMessage = errMsg
	return nil
}

// outcomes returns the recorded job-run outcomes for a schedule.
func (m *mockStore) outcomes(id int64) []JobRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []JobRun
	for _, r := range m.runs {
		if r.ScheduleID == id {
			out = append(out, r)
		}
	}
	return out
}

func (m *mockStore) GetDueSchedules(now time.Time) ([]ScheduleEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var due []ScheduleEntry
	for _, s := range m.schedules {
		if !m.disabled[s.ID] {
			due = append(due, s)
		}
	}
	return due, nil
}

func (m *mockStore) UpdateScheduleNextRun(id int64, nextRun time.Time, lastRun time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextRuns[id] = nextRun
	return nil
}

func (m *mockStore) DisableSchedule(id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disabled[id] = true
	return nil
}

// BumpHeartbeatCount simulates the persisted counter. Because hbCounts lives on
// the store (not the Scheduler), a fresh Scheduler sharing this store continues
// the count — modeling survival across a daemon restart.
func (m *mockStore) BumpHeartbeatCount(id int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hbCounts[id]++
	return m.hbCounts[id], nil
}

// Deep reflection fires every Nth heartbeat, and that cadence must SURVIVE a
// daemon restart. Because the count is persisted on the store, a fresh Scheduler
// (in-memory counter reset to 0) sharing the same store continues counting — so
// the 3rd heartbeat is deep even though a "restart" happened after the 2nd.
// Before the fix, the restart reset the counter and deep reflection never fired.
func TestDeepReflectionCadenceSurvivesRestart(t *testing.T) {
	store := newMockStore(nil)
	hb := ScheduleEntry{ID: 7, ChatID: 100, Message: "check", Type: "heartbeat", Mode: "prompt", Timezone: "UTC"}

	fired := 0
	var deepAt []int // 1-based heartbeat ordinals where a deep reflection fired
	newSched := func() *Scheduler {
		s := New(store, nil, nil, "UTC")
		s.SetQuietHours(0, 0) // disable quiet-hours suppression for the test
		s.SetDeepReflectInterval(3)
		s.SetHeartbeatPrompt(func(_ context.Context, chatID int64, msg string) (string, error) {
			fired++
			if strings.HasPrefix(msg, "[Heartbeat:deep] ") {
				deepAt = append(deepAt, fired)
			}
			return "", nil
		})
		return s
	}

	s := newSched()
	s.execute(context.Background(), hb, 0) // count 1 → light
	s.execute(context.Background(), hb, 0) // count 2 → light

	// Simulate a daemon restart: brand-new Scheduler (in-memory counter = 0),
	// same store (persisted count = 2).
	s = newSched()
	s.execute(context.Background(), hb, 0) // persisted count 3 → MUST be deep

	if len(deepAt) != 1 || deepAt[0] != 3 {
		t.Fatalf("deep reflection should fire on the 3rd persisted heartbeat across a restart; got deepAt=%v (fired=%d)", deepAt, fired)
	}
}

func TestSchedulerOneShotDisables(t *testing.T) {
	store := newMockStore([]ScheduleEntry{
		{ID: 1, ChatID: 100, Message: "hello", Type: "once", Mode: "notify", Timezone: "UTC"},
	})

	var notified []string
	s := New(store, func(chatID int64, msg string) {
		notified = append(notified, msg)
	}, nil, "UTC")

	tickSync(t, s)

	if len(notified) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notified))
	}
	if !store.disabled[1] {
		t.Error("expected one-shot to be disabled")
	}
}

func TestSchedulerCronAdvances(t *testing.T) {
	store := newMockStore([]ScheduleEntry{
		{ID: 2, ChatID: 200, Message: "check", Schedule: "*/5 * * * *", Type: "cron", Mode: "notify", Timezone: "UTC"},
	})

	var notified int
	s := New(store, func(chatID int64, msg string) {
		notified++
	}, nil, "UTC")

	tickSync(t, s)

	if notified != 1 {
		t.Fatalf("expected 1 notification, got %d", notified)
	}
	if store.disabled[2] {
		t.Error("cron should not be disabled")
	}
	if _, ok := store.nextRuns[2]; !ok {
		t.Error("expected next_run to be updated")
	}
}

func TestSchedulerPromptMode(t *testing.T) {
	store := newMockStore([]ScheduleEntry{
		{ID: 3, ChatID: 300, Message: "prompt me", Type: "once", Mode: "prompt", Timezone: "UTC"},
	})

	var prompted []string
	s := New(store, nil, func(_ context.Context, chatID int64, msg string) error {
		prompted = append(prompted, msg)
		return nil
	}, "UTC")

	tickSync(t, s)

	if len(prompted) != 1 {
		t.Fatalf("expected 1 prompt, got %d", len(prompted))
	}
}

func TestSchedulerRunCancellation(t *testing.T) {
	store := newMockStore(nil)
	s := New(store, func(int64, string) {}, nil, "UTC")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop after context cancellation")
	}
}

// tickSync runs one tick and waits for every dispatched job to finish. Fires now
// run on per-chat worker goroutines, so a test that asserts immediately after
// tick() would race the work it is asserting on.
func tickSync(t *testing.T, s *Scheduler) {
	t.Helper()
	s.tick(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for !s.dispatch.idle() {
		if time.Now().After(deadline) {
			t.Fatal("dispatched jobs did not drain within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}
