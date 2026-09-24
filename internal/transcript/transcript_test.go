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

	result := FormatTranscript(entries, nil, "pikamini_bot")
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

// Both daemons record every human group message; the store must keep one.
// An agent reply has no Telegram id (0) and is never deduplicated.
func TestHumanMessageRecordedOnce(t *testing.T) {
	s := tempStore(t)
	at := time.Now()
	for range 2 { // pikamini and umbreonmini both see the same message
		if err := s.Record(Entry{ChatID: -100200300, ThreadID: 7, TelegramMsgID: 555, Timestamp: at,
			SenderType: "human", SenderName: "mom", Text: "which week?"}); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		s.Record(Entry{ChatID: -100200300, ThreadID: 7, Timestamp: at, SenderType: "agent",
			AgentUsername: "a_bot", SenderName: "a_bot", Text: "same text twice is two replies"})
	}
	got, _ := s.RecentThread(-100200300, 7, 0)
	humans, agents := 0, 0
	for _, e := range got {
		if e.SenderType == "human" {
			humans++
		} else {
			agents++
		}
	}
	if humans != 1 || agents != 2 {
		t.Fatalf("humans=%d agents=%d, want 1 and 2", humans, agents)
	}
	if got[0].ThreadID != 7 || got[0].TelegramMsgID != 555 {
		t.Errorf("thread/msg id not round-tripped: %+v", got[0])
	}
}

// A turn in thread 7 must not see thread 9's conversation — that is how a
// note about a reminder in one topic ended an answer in another.
func TestRecentThreadIsScoped(t *testing.T) {
	s := tempStore(t)
	now := time.Now()
	add := func(thread int64, name, text string, d time.Duration) {
		s.Record(Entry{ChatID: 1, ThreadID: thread, Timestamp: now.Add(d), SenderType: "human", SenderName: name, Text: text})
	}
	add(7, "mom", "IPA是什麼", 0)
	add(9, "peer_bot", "Could you cancel #173?", time.Second)
	add(0, "dad", "general chat", 2*time.Second)

	got, err := s.RecentThread(1, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "IPA是什麼" {
		t.Fatalf("thread 7 = %+v, want only its own message", got)
	}
}

func TestOwnElsewhereAndFormat(t *testing.T) {
	s := tempStore(t)
	now := time.Now()
	rec := func(thread int64, user, text string, ago time.Duration) {
		s.Record(Entry{ChatID: 1, ThreadID: thread, Timestamp: now.Add(-ago), SenderType: "agent",
			AgentUsername: user, SenderName: user, Text: text})
	}
	rec(2479, "me_bot", "Oscar's: Tue–Thu 11:30–21:00", time.Hour)
	rec(2479, "peer_bot", "peer text from another thread", 50*time.Minute)
	rec(0, "me_bot", "reply in this very thread", 10*time.Minute)
	rec(11879, "me_bot", "too old to matter", 30*time.Hour)

	own, err := s.OwnElsewhere(1, 0, "ME_BOT", 3, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 1 || own[0].ThreadID != 2479 {
		t.Fatalf("own elsewhere = %+v, want only the Oscar reply from thread 2479", own)
	}

	s.Record(Entry{ChatID: 1, ThreadID: 0, Timestamp: now, SenderType: "human", SenderName: "mom", Text: "merge what you gave me"})
	thread, _ := s.RecentThread(1, 0, 0)
	block := FormatTranscript(thread, own, "me_bot")
	for _, want := range []string{"merge what you gave me", "OTHER threads", "thread 2479", "Oscar's"} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q:\n%s", want, block)
		}
	}
	for _, not := range []string{"reply in this very thread", "peer text from another thread", "too old"} {
		if strings.Contains(block, not) {
			t.Errorf("block must not contain %q:\n%s", not, block)
		}
	}
	if !strings.Contains(block, now.Local().Format("15:04")+" mom") {
		t.Errorf("thread lines should carry their local time:\n%s", block)
	}
	if FormatTranscript(nil, nil, "me_bot") != "" {
		t.Error("nothing to show must render nothing")
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
