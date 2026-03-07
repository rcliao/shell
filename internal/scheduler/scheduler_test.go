package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

type mockStore struct {
	mu        sync.Mutex
	schedules []ScheduleEntry
	disabled  map[int64]bool
	nextRuns  map[int64]time.Time
}

func newMockStore(entries []ScheduleEntry) *mockStore {
	return &mockStore{
		schedules: entries,
		disabled:  make(map[int64]bool),
		nextRuns:  make(map[int64]time.Time),
	}
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

func TestSchedulerOneShotDisables(t *testing.T) {
	store := newMockStore([]ScheduleEntry{
		{ID: 1, ChatID: 100, Message: "hello", Type: "once", Mode: "notify", Timezone: "UTC"},
	})

	var notified []string
	s := New(store, func(chatID int64, msg string) {
		notified = append(notified, msg)
	}, nil, "UTC")

	s.tick()

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

	s.tick()

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
	s := New(store, nil, func(chatID int64, msg string) {
		prompted = append(prompted, msg)
	}, "UTC")

	s.tick()

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
