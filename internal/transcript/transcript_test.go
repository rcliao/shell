package transcript

import (
	"os"
	"path/filepath"
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
	// Should be oldest-first.
	if entries[0].SenderName != "alice" {
		t.Errorf("expected alice first, got %s", entries[0].SenderName)
	}
	if entries[2].SenderName != "charlie" {
		t.Errorf("expected charlie last, got %s", entries[2].SenderName)
	}
}

func TestRecentByTokenBudget(t *testing.T) {
	s := tempStore(t)

	// Write 10 messages of ~50 chars each.
	for i := range 10 {
		s.Record(Entry{
			ChatID:     200,
			Timestamp:  time.Now().Add(time.Duration(i) * time.Second),
			SenderType: "human",
			SenderName: "user",
			Text:       "this is message number " + string(rune('A'+i)) + " with some padding text",
		})
	}

	// Budget of 100 tokens ≈ 400 chars — should fit ~6 messages.
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
	if !contains(result, "[alice]: hi") {
		t.Error("expected alice's message")
	}
	if contains(result, "pikamini") {
		t.Error("should not include self messages")
	}
	if !contains(result, "[umbreon]: hey all") {
		t.Error("expected umbreon's message")
	}
}

func TestTaskLifecycle(t *testing.T) {
	s := tempStore(t)

	// Create task.
	id, err := s.CreateTask(100, "pikamini_bot", "umbreon_bot", "review this code")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("expected non-empty task ID")
	}

	// Check pending.
	pending, err := s.PendingTasksFor(100, "umbreon_bot")
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
	if err := s.UpdateTaskStatus(id, TaskWorking); err != nil {
		t.Fatal(err)
	}

	// Still shows as pending/working.
	pending, err = s.PendingTasksFor(100, "umbreon_bot")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Status != TaskWorking {
		t.Errorf("expected 1 working task, got %d", len(pending))
	}

	// Complete.
	if err := s.CompleteTask(id, "looks good, 2 minor issues"); err != nil {
		t.Fatal(err)
	}

	// No longer pending.
	pending, err = s.PendingTasksFor(100, "umbreon_bot")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("expected 0 pending tasks, got %d", len(pending))
	}

	// But shows in recent.
	recent, err := s.RecentTasksInChat(100, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].Status != TaskCompleted {
		t.Fatalf("expected 1 completed task in recent, got %d", len(recent))
	}
}

func TestFormatPendingTasks(t *testing.T) {
	tasks := []Task{
		{ID: "abc123", FromAgent: "pikamini_bot", Status: TaskPending, Description: "review this"},
	}
	result := FormatPendingTasks(tasks)
	if !contains(result, "abc123") {
		t.Error("expected task ID in output")
	}
	if !contains(result, "review this") {
		t.Error("expected description in output")
	}
}

func TestConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "shared.db")

	s1, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()

	s2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	// Agent 1 writes.
	s1.Record(Entry{ChatID: 300, Timestamp: time.Now(), SenderType: "agent", SenderName: "pika", AgentUsername: "pika_bot", Text: "hello"})

	// Agent 2 reads.
	entries, err := s2.Recent(300, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SenderName != "pika" {
		t.Errorf("agent 2 should see agent 1's message, got %d entries", len(entries))
	}

	// Agent 2 writes a task.
	id, err := s2.CreateTask(300, "umbreon_bot", "pika_bot", "check this")
	if err != nil {
		t.Fatal(err)
	}

	// Agent 1 sees the task.
	pending, err := s1.PendingTasksFor(300, "pika_bot")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != id {
		t.Errorf("agent 1 should see task from agent 2")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && containsHelper(s, sub)
}

func containsHelper(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
