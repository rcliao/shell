package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func tempTaskStore(t *testing.T) *TaskStore {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenTaskStore(filepath.Join(dir, "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.CloseTaskStore() })
	return s
}

// --- Transcript Store tests ---

func TestRecordAndRecent(t *testing.T) {
	s := tempStore(t)

	for i, name := range []string{"alice", "bob", "charlie"} {
		s.Record(Entry{
			ChatID:     100,
			Timestamp:  time.Now().Add(time.Duration(i) * time.Second),
			SenderType: "human",
			SenderName: name,
			Text:       "hello from " + name,
		})
	}

	entries, err := s.Recent(100, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].SenderName != "alice" {
		t.Errorf("expected alice first, got %s", entries[0].SenderName)
	}
	if entries[2].SenderName != "charlie" {
		t.Errorf("expected charlie last, got %s", entries[2].SenderName)
	}
}

func TestRecentByTokenBudget(t *testing.T) {
	s := tempStore(t)

	for i := range 10 {
		s.Record(Entry{
			ChatID:     200,
			Timestamp:  time.Now().Add(time.Duration(i) * time.Second),
			SenderType: "human",
			SenderName: "user",
			Text:       "this is message number " + string(rune('A'+i)) + " with some padding text",
		})
	}

	entries, err := s.RecentByTokenBudget(200, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 3 || len(entries) > 8 {
		t.Errorf("expected 3-8 entries for budget=100, got %d", len(entries))
	}
}

func TestFormatTranscriptSkipsSelf(t *testing.T) {
	entries := []Entry{
		{SenderType: "human", SenderName: "alice", Text: "hi"},
		{SenderType: "agent", AgentUsername: "pikamini_bot", SenderName: "pikamini", Text: "hello back"},
		{SenderType: "agent", AgentUsername: "umbreon_bot", SenderName: "umbreon", Text: "hey all"},
	}

	result := FormatTranscript(entries, "pikamini_bot")
	if !strings.Contains(result, "[alice]: hi") {
		t.Error("expected alice's message")
	}
	if strings.Contains(result, "pikamini") {
		t.Error("should not include self messages")
	}
	if !strings.Contains(result, "[umbreon]: hey all") {
		t.Error("expected umbreon's message")
	}
}

// --- TaskStore tests ---

func TestTaskLifecycle(t *testing.T) {
	ts := tempTaskStore(t)

	// Create task.
	id, err := ts.CreateTask(Task{
		ChatID:      100,
		FromAgent:   "pikamini_bot",
		ToAgent:     "umbreon_bot",
		Description: "review this code",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("expected non-empty task ID")
	}

	// Check pending.
	pending, err := ts.PendingTasksFor("umbreon_bot")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending task, got %d", len(pending))
	}
	if pending[0].Description != "review this code" {
		t.Errorf("wrong description: %s", pending[0].Description)
	}

	// Update to working.
	if err := ts.UpdateTaskStatus(id, TaskWorking); err != nil {
		t.Fatal(err)
	}

	// Still shows as pending/working.
	pending, err = ts.PendingTasksFor("umbreon_bot")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Status != TaskWorking {
		t.Errorf("expected 1 working task, got %d", len(pending))
	}

	// Complete.
	if err := ts.CompleteTask(id, "looks good, 2 minor issues"); err != nil {
		t.Fatal(err)
	}

	// No longer pending.
	pending, err = ts.PendingTasksFor("umbreon_bot")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("expected 0 pending tasks, got %d", len(pending))
	}

	// But shows in recent.
	recent, err := ts.RecentTasks(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].Status != TaskCompleted {
		t.Fatalf("expected 1 completed task in recent, got %d", len(recent))
	}
}

func TestSelfTask(t *testing.T) {
	ts := tempTaskStore(t)

	id, err := ts.CreateTask(Task{
		ChatID:      100,
		FromAgent:   "pikamini_bot",
		ToAgent:     "pikamini_bot",
		Description: "step 1: research X",
		GoalID:      "goal_abc",
	})
	if err != nil {
		t.Fatal(err)
	}

	pending, err := ts.PendingTasksFor("pikamini_bot")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].GoalID != "goal_abc" {
		t.Errorf("expected 1 self-task with goal_id, got %d", len(pending))
	}

	// Self-task shows as self-task in formatting.
	formatted := FormatPendingTasksForAgent(pending)
	if !strings.Contains(formatted, "self-task") {
		t.Error("expected 'self-task' label")
	}

	ts.CompleteTask(id, "done")
}

func TestTaskEvents(t *testing.T) {
	ts := tempTaskStore(t)

	// Create a task — should publish event.
	_, err := ts.CreateTask(Task{
		ChatID:      100,
		FromAgent:   "pikamini_bot",
		ToAgent:     "umbreon_bot",
		Description: "verify this",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Umbreon consumes events.
	events, err := ts.ConsumeEvents("umbreon_bot")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventType != "task.created" {
		t.Errorf("expected 1 task.created event, got %d", len(events))
	}

	// Consuming again returns nothing (already consumed).
	events2, err := ts.ConsumeEvents("umbreon_bot")
	if err != nil {
		t.Fatal(err)
	}
	if len(events2) != 0 {
		t.Errorf("expected 0 events on second consume, got %d", len(events2))
	}
}

func TestTaskTTLExpiry(t *testing.T) {
	ts := tempTaskStore(t)

	// Create a task with 0 TTL (already expired).
	ts.CreateTask(Task{
		ChatID:      100,
		FromAgent:   "pikamini_bot",
		ToAgent:     "umbreon_bot",
		Description: "urgent task",
		TTLMinutes:  0, // will default to 60, so let's use the DB directly
	})

	// Manually set TTL to -1 to force expiry.
	ts.db.Exec(`UPDATE tasks SET ttl_minutes = 0, created_at = datetime('now', '-1 hour')`)

	expired, err := ts.ExpireOverdueTasks()
	if err != nil {
		t.Fatal(err)
	}
	if expired != 1 {
		t.Errorf("expected 1 expired task, got %d", expired)
	}

	// Task should be failed now.
	pending, err := ts.PendingTasksFor("umbreon_bot")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("expected 0 pending after expiry, got %d", len(pending))
	}
}

func TestConcurrentTaskStoreAccess(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "shared-tasks.db")

	ts1, err := OpenTaskStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ts1.CloseTaskStore()

	ts2, err := OpenTaskStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ts2.CloseTaskStore()

	// Agent 1 creates a task.
	id, err := ts1.CreateTask(Task{
		ChatID:      300,
		FromAgent:   "pikamini_bot",
		ToAgent:     "umbreon_bot",
		Description: "check this",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Agent 2 sees the task.
	pending, err := ts2.PendingTasksFor("umbreon_bot")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != id {
		t.Errorf("agent 2 should see task from agent 1")
	}

	// Agent 2 completes it.
	if err := ts2.CompleteTask(id, "verified"); err != nil {
		t.Fatal(err)
	}

	// Agent 1 sees the event.
	events, err := ts1.ConsumeEvents("pikamini_bot")
	if err != nil {
		t.Fatal(err)
	}
	// Should have both task.created (from_agent=pikamini) and task.completed events.
	found := false
	for _, e := range events {
		if e.EventType == "task.completed" {
			found = true
		}
	}
	if !found {
		t.Error("agent 1 should see task.completed event")
	}
}

func TestFormatPendingTasksForAgent(t *testing.T) {
	tasks := []Task{
		{ID: "abc123", FromAgent: "pikamini_bot", ToAgent: "umbreon_bot", Status: TaskPending, Description: "review this"},
	}
	result := FormatPendingTasksForAgent(tasks)
	if !strings.Contains(result, "abc123") {
		t.Error("expected task ID in output")
	}
	if !strings.Contains(result, "review this") {
		t.Error("expected description in output")
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
