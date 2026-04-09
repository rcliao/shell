// Package transcript provides a shared message log for multi-agent group chats.
// All agents read/write to the same SQLite database so each agent can see the
// full conversation, including messages from peer agents.
package transcript

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the shared transcript database. Multiple agent daemons open the same
// DB file and coordinate via SQLite's built-in locking.
type Store struct {
	db *sql.DB
}

// Entry is a single message in the group transcript.
type Entry struct {
	ID            int64
	ChatID        int64
	TelegramMsgID int
	Timestamp     time.Time
	SenderType    string // "human" or "agent"
	SenderName    string // display name (e.g. "mami", "pikamini")
	AgentUsername string // bot username for agent messages, empty for humans
	Text          string
	ReplyToMsgID  int // 0 if not a reply
}

// Task is a delegated unit of work between agents (A2A-inspired).
type Task struct {
	ID          string
	ChatID      int64
	FromAgent   string // bot username of the requesting agent
	ToAgent     string // bot username of the target agent
	Description string
	Status      string // pending, working, completed, failed, canceled
	Result      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Task status constants.
const (
	TaskPending   = "pending"
	TaskWorking   = "working"
	TaskCompleted = "completed"
	TaskFailed    = "failed"
	TaskCanceled  = "canceled"
)

// Open opens (or creates) the shared transcript database.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open transcript db: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS messages (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id         INTEGER NOT NULL,
			telegram_msg_id INTEGER NOT NULL DEFAULT 0,
			timestamp       DATETIME NOT NULL DEFAULT (datetime('now')),
			sender_type     TEXT NOT NULL DEFAULT 'human',
			sender_name     TEXT NOT NULL DEFAULT '',
			agent_username  TEXT NOT NULL DEFAULT '',
			text            TEXT NOT NULL DEFAULT '',
			reply_to_msg_id INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_messages_chat_ts ON messages(chat_id, timestamp);
		CREATE INDEX IF NOT EXISTS idx_messages_chat_msgid ON messages(chat_id, telegram_msg_id);

		CREATE TABLE IF NOT EXISTS tasks (
			id          TEXT PRIMARY KEY,
			chat_id     INTEGER NOT NULL,
			from_agent  TEXT NOT NULL,
			to_agent    TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			status      TEXT NOT NULL DEFAULT 'pending',
			result      TEXT NOT NULL DEFAULT '',
			created_at  DATETIME NOT NULL DEFAULT (datetime('now')),
			updated_at  DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE INDEX IF NOT EXISTS idx_tasks_chat_status ON tasks(chat_id, status);
		CREATE INDEX IF NOT EXISTS idx_tasks_to_agent ON tasks(to_agent, status);
	`)
	return err
}

// Record writes a message to the shared transcript.
func (s *Store) Record(e Entry) error {
	_, err := s.db.Exec(`
		INSERT INTO messages (chat_id, telegram_msg_id, timestamp, sender_type, sender_name, agent_username, text, reply_to_msg_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ChatID, e.TelegramMsgID, e.Timestamp, e.SenderType, e.SenderName, e.AgentUsername, e.Text, e.ReplyToMsgID,
	)
	return err
}

// Recent returns the most recent messages for a chat, ordered oldest-first,
// limited by count. Use this for building transcript context.
func (s *Store) Recent(chatID int64, limit int) ([]Entry, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, telegram_msg_id, timestamp, sender_type, sender_name, agent_username, text, reply_to_msg_id
		FROM messages
		WHERE chat_id = ?
		ORDER BY timestamp DESC, id DESC
		LIMIT ?`, chatID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.ChatID, &e.TelegramMsgID, &e.Timestamp, &e.SenderType, &e.SenderName, &e.AgentUsername, &e.Text, &e.ReplyToMsgID); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	// Reverse to oldest-first.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries, nil
}

// RecentByTokenBudget returns recent messages up to an approximate token budget.
// Uses a rough 4 chars ≈ 1 token heuristic.
func (s *Store) RecentByTokenBudget(chatID int64, tokenBudget int) ([]Entry, error) {
	// Fetch a generous number then trim to budget.
	entries, err := s.Recent(chatID, 200)
	if err != nil {
		return nil, err
	}
	if tokenBudget <= 0 {
		return entries, nil
	}

	// Walk backwards from most recent, accumulating tokens.
	charBudget := tokenBudget * 4
	total := 0
	start := len(entries)
	for i := len(entries) - 1; i >= 0; i-- {
		cost := len(entries[i].SenderName) + len(entries[i].Text) + 10 // overhead for formatting
		if total+cost > charBudget {
			break
		}
		total += cost
		start = i
	}
	return entries[start:], nil
}

// --- Task delegation (A2A-inspired) ---

// generateID returns a short random hex ID for tasks.
func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// CreateTask creates a new delegated task and returns its ID.
func (s *Store) CreateTask(chatID int64, fromAgent, toAgent, description string) (string, error) {
	id := generateID()
	now := time.Now()
	_, err := s.db.Exec(`
		INSERT INTO tasks (id, chat_id, from_agent, to_agent, description, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatID, fromAgent, toAgent, description, TaskPending, now, now,
	)
	return id, err
}

// UpdateTaskStatus updates a task's status.
func (s *Store) UpdateTaskStatus(taskID, status string) error {
	_, err := s.db.Exec(`UPDATE tasks SET status = ?, updated_at = datetime('now') WHERE id = ?`, status, taskID)
	return err
}

// CompleteTask marks a task as completed with a result.
func (s *Store) CompleteTask(taskID, result string) error {
	_, err := s.db.Exec(`UPDATE tasks SET status = ?, result = ?, updated_at = datetime('now') WHERE id = ?`,
		TaskCompleted, result, taskID)
	return err
}

// FailTask marks a task as failed with a reason.
func (s *Store) FailTask(taskID, reason string) error {
	_, err := s.db.Exec(`UPDATE tasks SET status = ?, result = ?, updated_at = datetime('now') WHERE id = ?`,
		TaskFailed, reason, taskID)
	return err
}

// PendingTasksFor returns all pending/working tasks addressed to this agent in a chat.
func (s *Store) PendingTasksFor(chatID int64, agentUsername string) ([]Task, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, from_agent, to_agent, description, status, result, created_at, updated_at
		FROM tasks
		WHERE chat_id = ? AND LOWER(to_agent) = LOWER(?) AND status IN (?, ?)
		ORDER BY created_at ASC`,
		chatID, agentUsername, TaskPending, TaskWorking)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

// RecentTasksInChat returns recent tasks in a chat (any status), for transcript context.
func (s *Store) RecentTasksInChat(chatID int64, limit int) ([]Task, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, from_agent, to_agent, description, status, result, created_at, updated_at
		FROM tasks
		WHERE chat_id = ?
		ORDER BY created_at DESC
		LIMIT ?`,
		chatID, limit)
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

// GetTask returns a task by ID, or nil if not found.
func (s *Store) GetTask(taskID string) (*Task, error) {
	row := s.db.QueryRow(`
		SELECT id, chat_id, from_agent, to_agent, description, status, result, created_at, updated_at
		FROM tasks WHERE id = ?`, taskID)
	var t Task
	err := row.Scan(&t.ID, &t.ChatID, &t.FromAgent, &t.ToAgent, &t.Description, &t.Status, &t.Result, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func scanTasks(rows *sql.Rows) ([]Task, error) {
	var tasks []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.ChatID, &t.FromAgent, &t.ToAgent, &t.Description, &t.Status, &t.Result, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

// --- Formatting ---

// FormatTranscript renders entries as a readable transcript block for injection
// into the agent's context. selfUsername is this agent's bot username (excluded
// from the transcript since the agent already sees its own messages in session).
func FormatTranscript(entries []Entry, selfUsername string) string {
	if len(entries) == 0 {
		return ""
	}
	selfLower := strings.ToLower(selfUsername)
	var sb strings.Builder
	sb.WriteString("\n[Group conversation transcript — recent messages from other participants]\n")
	included := 0
	for _, e := range entries {
		// Skip own messages — agent already has these in session history.
		if e.SenderType == "agent" && strings.ToLower(e.AgentUsername) == selfLower {
			continue
		}
		name := e.SenderName
		if name == "" {
			name = e.AgentUsername
		}
		fmt.Fprintf(&sb, "[%s]: %s\n", name, e.Text)
		included++
	}
	if included == 0 {
		return ""
	}
	sb.WriteString("[End transcript]\n")
	return sb.String()
}

// FormatPendingTasks renders pending tasks for this agent as a context block.
func FormatPendingTasks(tasks []Task) string {
	if len(tasks) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n[Pending tasks assigned to you]\n")
	for _, t := range tasks {
		fmt.Fprintf(&sb, "- Task %s (from %s, status: %s): %s\n", t.ID, t.FromAgent, t.Status, t.Description)
	}
	sb.WriteString("Process these tasks and respond with [task-result id=TASK_ID]your result[/task-result] for each.\n")
	sb.WriteString("[End pending tasks]\n")
	return sb.String()
}

// FormatRecentTasks renders recent task activity for general awareness.
func FormatRecentTasks(tasks []Task, selfUsername string) string {
	if len(tasks) == 0 {
		return ""
	}
	selfLower := strings.ToLower(selfUsername)
	var sb strings.Builder
	sb.WriteString("\n[Recent task activity]\n")
	for _, t := range tasks {
		arrow := fmt.Sprintf("%s → %s", t.FromAgent, t.ToAgent)
		switch t.Status {
		case TaskCompleted:
			result := t.Result
			if len(result) > 200 {
				result = result[:200] + "..."
			}
			fmt.Fprintf(&sb, "- Task %s (%s) COMPLETED: %s → Result: %s\n", t.ID, arrow, t.Description, result)
		case TaskFailed:
			fmt.Fprintf(&sb, "- Task %s (%s) FAILED: %s → %s\n", t.ID, arrow, t.Description, t.Result)
		case TaskPending, TaskWorking:
			if strings.ToLower(t.ToAgent) == selfLower {
				continue // already shown in pending tasks
			}
			fmt.Fprintf(&sb, "- Task %s (%s) %s: %s\n", t.ID, arrow, strings.ToUpper(t.Status), t.Description)
		}
	}
	sb.WriteString("[End task activity]\n")
	return sb.String()
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
