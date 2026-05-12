// Package transcript provides a shared message log for multi-agent group chats.
// All agents read/write to the same SQLite database so each agent can see the
// full conversation, including messages from peer agents.
package transcript

import (
	"database/sql"
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
// Task is a unit of work — either self-decomposition or cross-agent delegation.
type Task struct {
	ID             string
	ChatID         int64
	GoalID         string // links related tasks to a parent goal
	FromAgent      string // bot username of the requesting agent
	ToAgent        string // bot username of the target agent (can = FromAgent for self-tasks)
	Description    string
	Context        string // one-line context summary (invisible metadata)
	Status         string // pending, working, completed, failed, canceled
	Result         string
	TelegramMsgID  int    // for editable status messages
	TTLMinutes     int    // auto-fail after this many minutes (default 60)
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    *time.Time
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

// Deprecated: Task methods on Store are superseded by TaskStore.
// Kept as stubs to avoid breaking the transcript DB migration.
// All new task operations should use TaskStore.

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

// Deprecated: FormatPendingTasks and FormatRecentTasks moved to taskstore.go
// as FormatPendingTasksForAgent and FormatTaskActivity.

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
