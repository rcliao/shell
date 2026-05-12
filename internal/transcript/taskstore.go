// Package transcript — taskstore.go provides a shared task store for
// multi-agent task decomposition and delegation. Both self-tasks and
// cross-agent delegation use the same store. An events table serves as
// a lightweight internal message bus for async notifications.
package transcript

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// generateID returns a short random hex ID for tasks.
func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// TaskStore is the shared task database. Multiple agent daemons open
// the same file and coordinate via SQLite WAL locking.
type TaskStore struct {
	db *sql.DB
}

// OpenTaskStore opens (or creates) the shared task database.
func OpenTaskStore(path string) (*TaskStore, error) {
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open task store: %w", err)
	}
	if err := migrateTaskStore(db); err != nil {
		db.Close()
		return nil, err
	}
	return &TaskStore{db: db}, nil
}

func migrateTaskStore(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS tasks (
			id              TEXT PRIMARY KEY,
			chat_id         INTEGER NOT NULL DEFAULT 0,
			goal_id         TEXT NOT NULL DEFAULT '',
			from_agent      TEXT NOT NULL,
			to_agent        TEXT NOT NULL,
			description     TEXT NOT NULL DEFAULT '',
			context         TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL DEFAULT 'pending',
			result          TEXT NOT NULL DEFAULT '',
			telegram_msg_id INTEGER NOT NULL DEFAULT 0,
			ttl_minutes     INTEGER NOT NULL DEFAULT 60,
			created_at      DATETIME NOT NULL DEFAULT (datetime('now')),
			updated_at      DATETIME NOT NULL DEFAULT (datetime('now')),
			completed_at    DATETIME
		);
		CREATE INDEX IF NOT EXISTS idx_tasks_to_status ON tasks(to_agent, status);
		CREATE INDEX IF NOT EXISTS idx_tasks_goal ON tasks(goal_id);
		CREATE INDEX IF NOT EXISTS idx_tasks_chat ON tasks(chat_id, status);

		CREATE TABLE IF NOT EXISTS events (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			target      TEXT NOT NULL,
			event_type  TEXT NOT NULL,
			payload     TEXT NOT NULL DEFAULT '{}',
			created_at  DATETIME NOT NULL DEFAULT (datetime('now')),
			consumed_at DATETIME
		);
		CREATE INDEX IF NOT EXISTS idx_events_target ON events(target, consumed_at);
	`)
	return err
}

// --- Task CRUD ---

// CreateTask creates a new task and publishes a task.created event.
func (s *TaskStore) CreateTask(t Task) (string, error) {
	if t.ID == "" {
		t.ID = generateID()
	}
	now := time.Now()
	if t.TTLMinutes <= 0 {
		t.TTLMinutes = 60
	}
	_, err := s.db.Exec(`
		INSERT INTO tasks (id, chat_id, goal_id, from_agent, to_agent, description, context, status, ttl_minutes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ChatID, t.GoalID, t.FromAgent, t.ToAgent, t.Description, t.Context,
		TaskPending, t.TTLMinutes, now, now,
	)
	if err != nil {
		return "", err
	}

	// Publish event for target agent.
	payload, _ := json.Marshal(map[string]any{
		"task_id":     t.ID,
		"chat_id":     t.ChatID,
		"from_agent":  t.FromAgent,
		"description": t.Description,
		"goal_id":     t.GoalID,
	})
	s.PublishEvent(t.ToAgent, "task.created", string(payload))

	return t.ID, nil
}

// CompleteTask marks a task completed with a result and publishes events.
func (s *TaskStore) CompleteTask(taskID, result string) error {
	now := time.Now()
	res, err := s.db.Exec(`
		UPDATE tasks SET status = ?, result = ?, updated_at = ?, completed_at = ?
		WHERE id = ? AND status IN (?, ?)`,
		TaskCompleted, result, now, now, taskID, TaskPending, TaskWorking,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("task %s not found or already terminal", taskID)
	}

	// Notify originating agent.
	t, _ := s.GetTask(taskID)
	if t != nil {
		payload, _ := json.Marshal(map[string]any{
			"task_id":    t.ID,
			"chat_id":    t.ChatID,
			"from_agent": t.FromAgent,
			"to_agent":   t.ToAgent,
			"result":     truncate(result, 200),
		})
		s.PublishEvent(t.FromAgent, "task.completed", string(payload))
	}
	return nil
}

// FailTask marks a task as failed and notifies the originator.
func (s *TaskStore) FailTask(taskID, reason string) error {
	now := time.Now()
	res, err := s.db.Exec(`
		UPDATE tasks SET status = ?, result = ?, updated_at = ?, completed_at = ?
		WHERE id = ? AND status IN (?, ?)`,
		TaskFailed, reason, now, now, taskID, TaskPending, TaskWorking,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("task %s not found or already terminal", taskID)
	}

	t, _ := s.GetTask(taskID)
	if t != nil {
		payload, _ := json.Marshal(map[string]any{
			"task_id":    t.ID,
			"chat_id":    t.ChatID,
			"from_agent": t.FromAgent,
			"reason":     truncate(reason, 200),
		})
		s.PublishEvent(t.FromAgent, "task.failed", string(payload))
	}
	return nil
}

// UpdateTaskStatus updates status (e.g., to "working").
func (s *TaskStore) UpdateTaskStatus(taskID, status string) error {
	_, err := s.db.Exec(`UPDATE tasks SET status = ?, updated_at = datetime('now') WHERE id = ?`,
		status, taskID)
	return err
}

// SetTelegramMsgID stores the Telegram message ID for editable status messages.
func (s *TaskStore) SetTelegramMsgID(taskID string, msgID int) error {
	_, err := s.db.Exec(`UPDATE tasks SET telegram_msg_id = ? WHERE id = ?`, msgID, taskID)
	return err
}

// GetTask returns a task by ID, or nil if not found.
func (s *TaskStore) GetTask(taskID string) (*Task, error) {
	row := s.db.QueryRow(`
		SELECT id, chat_id, goal_id, from_agent, to_agent, description, context,
		       status, result, telegram_msg_id, ttl_minutes, created_at, updated_at, completed_at
		FROM tasks WHERE id = ?`, taskID)
	return scanTask(row)
}

// PendingTasksFor returns pending/working tasks for this agent.
func (s *TaskStore) PendingTasksFor(agentUsername string) ([]Task, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, goal_id, from_agent, to_agent, description, context,
		       status, result, telegram_msg_id, ttl_minutes, created_at, updated_at, completed_at
		FROM tasks
		WHERE LOWER(to_agent) = LOWER(?) AND status IN (?, ?)
		ORDER BY created_at ASC`,
		agentUsername, TaskPending, TaskWorking)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

// RecentTasks returns recent tasks (any status) for context awareness.
func (s *TaskStore) RecentTasks(limit int) ([]Task, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, goal_id, from_agent, to_agent, description, context,
		       status, result, telegram_msg_id, ttl_minutes, created_at, updated_at, completed_at
		FROM tasks
		ORDER BY created_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks, err := scanTasks(rows)
	if err != nil {
		return nil, err
	}
	// Reverse to oldest-first.
	for i, j := 0, len(tasks)-1; i < j; i, j = i+1, j-1 {
		tasks[i], tasks[j] = tasks[j], tasks[i]
	}
	return tasks, nil
}

// ExpireOverdueTasks fails tasks that have exceeded their TTL and publishes events.
func (s *TaskStore) ExpireOverdueTasks() (int, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, goal_id, from_agent, to_agent, description, context,
		       status, result, telegram_msg_id, ttl_minutes, created_at, updated_at, completed_at
		FROM tasks
		WHERE status IN (?, ?)
		  AND datetime(created_at, '+' || ttl_minutes || ' minutes') < datetime('now')`,
		TaskPending, TaskWorking)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	expired, err := scanTasks(rows)
	if err != nil {
		return 0, err
	}

	for _, t := range expired {
		s.FailTask(t.ID, "TTL expired")
	}
	return len(expired), nil
}

// --- Events (internal message bus) ---

// PublishEvent writes an event for a target agent.
func (s *TaskStore) PublishEvent(target, eventType, payload string) error {
	_, err := s.db.Exec(`
		INSERT INTO events (target, event_type, payload) VALUES (?, ?, ?)`,
		target, eventType, payload)
	return err
}

// ConsumeEvents returns and marks consumed all pending events for this agent.
func (s *TaskStore) ConsumeEvents(agentUsername string) ([]Event, error) {
	now := time.Now()
	rows, err := s.db.Query(`
		SELECT id, target, event_type, payload, created_at
		FROM events
		WHERE (LOWER(target) = LOWER(?) OR target = '*') AND consumed_at IS NULL
		ORDER BY id ASC`,
		agentUsername)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []Event
	var ids []int64
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Target, &e.EventType, &e.Payload, &e.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, e)
		ids = append(ids, e.ID)
	}

	// Mark consumed.
	for _, id := range ids {
		s.db.Exec(`UPDATE events SET consumed_at = ? WHERE id = ?`, now, id)
	}
	return events, nil
}

// --- Types ---

// Event is an internal message bus entry.
type Event struct {
	ID        int64
	Target    string
	EventType string
	Payload   string
	CreatedAt time.Time
}

// --- Formatting ---

// FormatPendingTasksForAgent renders pending tasks for injection into agent context.
func FormatPendingTasksForAgent(tasks []Task) string {
	if len(tasks) == 0 {
		return ""
	}
	var sb fmt.Stringer = nil
	_ = sb
	var b []byte
	b = append(b, "\n[Pending tasks assigned to you]\n"...)
	for _, t := range tasks {
		line := fmt.Sprintf("- Task %s", t.ID)
		if t.FromAgent != t.ToAgent {
			line += fmt.Sprintf(" (from %s)", t.FromAgent)
		} else {
			line += " (self-task)"
		}
		line += fmt.Sprintf(": %s", t.Description)
		if t.Context != "" {
			line += fmt.Sprintf("\n  Context: %s", t.Context)
		}
		if t.GoalID != "" {
			line += fmt.Sprintf("\n  Goal: %s", t.GoalID)
		}
		b = append(b, line...)
		b = append(b, '\n')
	}
	b = append(b, "Process these tasks using: scripts/shell-task complete --id <TASK_ID> --result \"your findings\"\n"...)
	b = append(b, "[End pending tasks]\n"...)
	return string(b)
}

// FormatTaskActivity renders recent task activity for heartbeat context.
func FormatTaskActivity(tasks []Task, selfUsername string) string {
	if len(tasks) == 0 {
		return ""
	}
	var b []byte
	b = append(b, "\n[Recent task activity]\n"...)
	for _, t := range tasks {
		arrow := fmt.Sprintf("%s → %s", t.FromAgent, t.ToAgent)
		if t.FromAgent == t.ToAgent {
			arrow = t.FromAgent + " (self)"
		}
		switch t.Status {
		case TaskCompleted:
			result := truncate(t.Result, 150)
			b = append(b, fmt.Sprintf("- ✅ %s (%s): %s → %s\n", t.ID, arrow, t.Description, result)...)
		case TaskFailed:
			b = append(b, fmt.Sprintf("- ❌ %s (%s): %s → %s\n", t.ID, arrow, t.Description, t.Result)...)
		case TaskPending, TaskWorking:
			b = append(b, fmt.Sprintf("- ⏳ %s (%s) %s: %s\n", t.ID, arrow, t.Status, t.Description)...)
		}
	}
	b = append(b, "[End task activity]\n"...)
	return string(b)
}

// --- Helpers ---

func scanTask(row *sql.Row) (*Task, error) {
	var t Task
	var completedAt sql.NullTime
	err := row.Scan(&t.ID, &t.ChatID, &t.GoalID, &t.FromAgent, &t.ToAgent,
		&t.Description, &t.Context, &t.Status, &t.Result,
		&t.TelegramMsgID, &t.TTLMinutes, &t.CreatedAt, &t.UpdatedAt, &completedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if completedAt.Valid {
		t.CompletedAt = &completedAt.Time
	}
	return &t, nil
}

func scanTasks(rows *sql.Rows) ([]Task, error) {
	var tasks []Task
	for rows.Next() {
		var t Task
		var completedAt sql.NullTime
		if err := rows.Scan(&t.ID, &t.ChatID, &t.GoalID, &t.FromAgent, &t.ToAgent,
			&t.Description, &t.Context, &t.Status, &t.Result,
			&t.TelegramMsgID, &t.TTLMinutes, &t.CreatedAt, &t.UpdatedAt, &completedAt); err != nil {
			return nil, err
		}
		if completedAt.Valid {
			t.CompletedAt = &completedAt.Time
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// CloseTaskStore closes the database.
func (s *TaskStore) CloseTaskStore() error {
	return s.db.Close()
}
