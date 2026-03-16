package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Session struct {
	ID               int64
	ChatID           int64
	ProviderSessionID  string
	Status           string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type Message struct {
	ID        int64
	SessionID int64
	Role      string
	Content   string
	CreatedAt time.Time
}

// MessageMap links Telegram message IDs to session exchanges so that
// reactions on a specific bot response can be traced back to the
// originating user message and session.
type MessageMap struct {
	ID             int64
	ChatID         int64
	UserMessageID  int
	BotMessageID   int
	SessionID      int64
	UserMessage    string // original user message text
	BotResponse    string // bot response text
	CreatedAt      time.Time
}

// Schedule represents a scheduled notification or prompt.
type Schedule struct {
	ID        int64
	ChatID    int64
	Label     string
	Message   string
	Schedule  string // cron expression or ISO8601 for one-shot
	Timezone  string
	Type      string // "cron" or "once"
	Mode      string // "notify" or "prompt"
	NextRunAt time.Time
	LastRunAt *time.Time
	Enabled   bool
	CreatedAt time.Time
}

func Open(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Enable WAL mode for better concurrent access
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return s, nil
}

func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS sessions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id INTEGER UNIQUE NOT NULL,
		claude_session_id TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'active',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id INTEGER NOT NULL,
		role TEXT NOT NULL,
		content TEXT NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (session_id) REFERENCES sessions(id)
	);

	CREATE TABLE IF NOT EXISTS message_map (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id INTEGER NOT NULL,
		user_message_id INTEGER NOT NULL,
		bot_message_id INTEGER NOT NULL,
		session_id INTEGER NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (session_id) REFERENCES sessions(id)
	);

	CREATE INDEX IF NOT EXISTS idx_sessions_chat_id ON sessions(chat_id);
	CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id);
	CREATE INDEX IF NOT EXISTS idx_message_map_chat_bot ON message_map(chat_id, bot_message_id);

	CREATE TABLE IF NOT EXISTS schedules (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id     INTEGER NOT NULL,
		label       TEXT NOT NULL DEFAULT '',
		message     TEXT NOT NULL,
		schedule    TEXT NOT NULL,
		timezone    TEXT NOT NULL DEFAULT 'UTC',
		type        TEXT NOT NULL DEFAULT 'cron',
		mode        TEXT NOT NULL DEFAULT 'notify',
		next_run_at DATETIME NOT NULL,
		last_run_at DATETIME,
		enabled     INTEGER NOT NULL DEFAULT 1,
		created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_schedules_next_run ON schedules(enabled, next_run_at);
	CREATE INDEX IF NOT EXISTS idx_schedules_chat ON schedules(chat_id);
	`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}

	// Add columns for message content (idempotent for existing databases).
	for _, col := range []string{
		"ALTER TABLE message_map ADD COLUMN user_message TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE message_map ADD COLUMN bot_response TEXT NOT NULL DEFAULT ''",
	} {
		s.db.Exec(col) // ignore "duplicate column" errors
	}

	// Background task queue for heartbeat to pick up.
	taskSchema := `
	CREATE TABLE IF NOT EXISTS tasks (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id     INTEGER NOT NULL,
		description TEXT NOT NULL,
		status      TEXT NOT NULL DEFAULT 'pending',
		created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		completed_at DATETIME
	);
	CREATE INDEX IF NOT EXISTS idx_tasks_chat_status ON tasks(chat_id, status);
	`
	if _, err := s.db.Exec(taskSchema); err != nil {
		return err
	}

	return nil
}

func (s *Store) SaveSession(chatID int64, claudeSessionID string) error {
	_, err := s.db.Exec(`
		INSERT INTO sessions (chat_id, claude_session_id, status, created_at, updated_at)
		VALUES (?, ?, 'active', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(chat_id) DO UPDATE SET
			claude_session_id = excluded.claude_session_id,
			status = 'active',
			updated_at = CURRENT_TIMESTAMP
	`, chatID, claudeSessionID)
	return err
}

func (s *Store) GetSession(chatID int64) (*Session, error) {
	row := s.db.QueryRow(`
		SELECT id, chat_id, claude_session_id, status, created_at, updated_at
		FROM sessions WHERE chat_id = ?
	`, chatID)

	var sess Session
	err := row.Scan(&sess.ID, &sess.ChatID, &sess.ProviderSessionID, &sess.Status, &sess.CreatedAt, &sess.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *Store) LogMessage(sessionID int64, role, content string) error {
	_, err := s.db.Exec(`
		INSERT INTO messages (session_id, role, content) VALUES (?, ?, ?)
	`, sessionID, role, content)
	return err
}

func (s *Store) GetMessages(sessionID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT id, session_id, role, content, created_at
		FROM messages WHERE session_id = ? ORDER BY id DESC LIMIT ?
	`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}

	// Reverse to get chronological order
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// SaveMessageMap persists a mapping between a user's Telegram message and
// the bot's response message within a session, including the message content.
func (s *Store) SaveMessageMap(chatID int64, userMessageID, botMessageID int, sessionID int64, userMessage, botResponse string) error {
	_, err := s.db.Exec(`
		INSERT INTO message_map (chat_id, user_message_id, bot_message_id, session_id, user_message, bot_response)
		VALUES (?, ?, ?, ?, ?, ?)
	`, chatID, userMessageID, botMessageID, sessionID, userMessage, botResponse)
	return err
}

// GetMessageMapByBotMsg looks up a message map entry by the bot's response
// message ID within a chat. Returns nil if no mapping is found.
func (s *Store) GetMessageMapByBotMsg(chatID int64, botMessageID int) (*MessageMap, error) {
	row := s.db.QueryRow(`
		SELECT id, chat_id, user_message_id, bot_message_id, session_id, user_message, bot_response, created_at
		FROM message_map WHERE chat_id = ? AND bot_message_id = ?
	`, chatID, botMessageID)

	var m MessageMap
	err := row.Scan(&m.ID, &m.ChatID, &m.UserMessageID, &m.BotMessageID, &m.SessionID, &m.UserMessage, &m.BotResponse, &m.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// UpdateMessageMapResponse updates the bot_response text for an existing
// message_map entry. Used when regenerating a response in-place.
func (s *Store) UpdateMessageMapResponse(id int64, botResponse string) error {
	_, err := s.db.Exec("UPDATE message_map SET bot_response = ? WHERE id = ?", botResponse, id)
	return err
}

// DeleteMessageMap deletes a single message_map entry by its row ID.
func (s *Store) DeleteMessageMap(id int64) error {
	_, err := s.db.Exec("DELETE FROM message_map WHERE id = ?", id)
	return err
}

// DeleteExchangeMessages removes the most recent user+assistant message pair
// matching the given content from a session's message log.
func (s *Store) DeleteExchangeMessages(sessionID int64, userMessage, botResponse string) error {
	if userMessage != "" {
		row := s.db.QueryRow(
			"SELECT id FROM messages WHERE session_id = ? AND role = 'user' AND content = ? ORDER BY id DESC LIMIT 1",
			sessionID, userMessage,
		)
		var id int64
		if err := row.Scan(&id); err == nil {
			s.db.Exec("DELETE FROM messages WHERE id = ?", id)
		}
	}
	if botResponse != "" {
		row := s.db.QueryRow(
			"SELECT id FROM messages WHERE session_id = ? AND role = 'assistant' AND content = ? ORDER BY id DESC LIMIT 1",
			sessionID, botResponse,
		)
		var id int64
		if err := row.Scan(&id); err == nil {
			s.db.Exec("DELETE FROM messages WHERE id = ?", id)
		}
	}
	return nil
}

func (s *Store) DeleteSession(chatID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Delete message_map entries first
	_, err = tx.Exec(`
		DELETE FROM message_map WHERE session_id IN (
			SELECT id FROM sessions WHERE chat_id = ?
		)
	`, chatID)
	if err != nil {
		return err
	}

	// Delete messages
	_, err = tx.Exec(`
		DELETE FROM messages WHERE session_id IN (
			SELECT id FROM sessions WHERE chat_id = ?
		)
	`, chatID)
	if err != nil {
		return err
	}

	_, err = tx.Exec(`DELETE FROM sessions WHERE chat_id = ?`, chatID)
	if err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) UpdateSessionStatus(chatID int64, status string) error {
	_, err := s.db.Exec(`
		UPDATE sessions SET status = ?, updated_at = CURRENT_TIMESTAMP
		WHERE chat_id = ?
	`, status, chatID)
	return err
}

func (s *Store) ListActiveSessions() ([]Session, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, claude_session_id, status, created_at, updated_at
		FROM sessions WHERE status = 'active' ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.ChatID, &sess.ProviderSessionID, &sess.Status, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, nil
}

func (s *Store) StaleSessionChatIDs(idleDuration time.Duration) ([]int64, error) {
	cutoff := time.Now().Add(-idleDuration)
	rows, err := s.db.Query(`
		SELECT chat_id FROM sessions
		WHERE status = 'active' AND updated_at < ?
	`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// SaveSchedule inserts a new schedule and returns its ID.
func (s *Store) SaveSchedule(sched *Schedule) (int64, error) {
	enabled := 0
	if sched.Enabled {
		enabled = 1
	}
	result, err := s.db.Exec(`
		INSERT INTO schedules (chat_id, label, message, schedule, timezone, type, mode, next_run_at, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, sched.ChatID, sched.Label, sched.Message, sched.Schedule, sched.Timezone, sched.Type, sched.Mode, sched.NextRunAt, enabled)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// ListSchedules returns all schedules for a given chat.
func (s *Store) ListSchedules(chatID int64) ([]Schedule, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, label, message, schedule, timezone, type, mode, next_run_at, last_run_at, enabled, created_at
		FROM schedules WHERE chat_id = ? ORDER BY id
	`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var schedules []Schedule
	for rows.Next() {
		var sc Schedule
		var enabled int
		var lastRun sql.NullTime
		if err := rows.Scan(&sc.ID, &sc.ChatID, &sc.Label, &sc.Message, &sc.Schedule, &sc.Timezone, &sc.Type, &sc.Mode, &sc.NextRunAt, &lastRun, &enabled, &sc.CreatedAt); err != nil {
			return nil, err
		}
		sc.Enabled = enabled != 0
		if lastRun.Valid {
			sc.LastRunAt = &lastRun.Time
		}
		schedules = append(schedules, sc)
	}
	return schedules, nil
}

// GetDueSchedules returns enabled schedules whose next_run_at is at or before now.
func (s *Store) GetDueSchedules(now time.Time) ([]Schedule, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, label, message, schedule, timezone, type, mode, next_run_at, last_run_at, enabled, created_at
		FROM schedules WHERE enabled = 1 AND next_run_at <= ?
	`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var schedules []Schedule
	for rows.Next() {
		var sc Schedule
		var enabled int
		var lastRun sql.NullTime
		if err := rows.Scan(&sc.ID, &sc.ChatID, &sc.Label, &sc.Message, &sc.Schedule, &sc.Timezone, &sc.Type, &sc.Mode, &sc.NextRunAt, &lastRun, &enabled, &sc.CreatedAt); err != nil {
			return nil, err
		}
		sc.Enabled = enabled != 0
		if lastRun.Valid {
			sc.LastRunAt = &lastRun.Time
		}
		schedules = append(schedules, sc)
	}
	return schedules, nil
}

// UpdateScheduleNextRun updates the next and last run times for a schedule.
func (s *Store) UpdateScheduleNextRun(id int64, nextRun time.Time, lastRun time.Time) error {
	_, err := s.db.Exec(`UPDATE schedules SET next_run_at = ?, last_run_at = ? WHERE id = ?`, nextRun, lastRun, id)
	return err
}

// DisableSchedule sets enabled=0 for a schedule (used for completed one-shots).
func (s *Store) DisableSchedule(id int64) error {
	_, err := s.db.Exec(`UPDATE schedules SET enabled = 0 WHERE id = ?`, id)
	return err
}

// GetHeartbeat returns the heartbeat schedule for a chat, or nil if none exists.
func (s *Store) GetHeartbeat(chatID int64) (*Schedule, error) {
	row := s.db.QueryRow(`
		SELECT id, chat_id, label, message, schedule, timezone, type, mode, next_run_at, last_run_at, enabled, created_at
		FROM schedules WHERE chat_id = ? AND type = 'heartbeat' LIMIT 1
	`, chatID)

	var sc Schedule
	var enabled int
	var lastRun sql.NullTime
	err := row.Scan(&sc.ID, &sc.ChatID, &sc.Label, &sc.Message, &sc.Schedule, &sc.Timezone, &sc.Type, &sc.Mode, &sc.NextRunAt, &lastRun, &enabled, &sc.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sc.Enabled = enabled != 0
	if lastRun.Valid {
		sc.LastRunAt = &lastRun.Time
	}
	return &sc, nil
}

// DeleteHeartbeat removes the heartbeat schedule for a chat.
func (s *Store) DeleteHeartbeat(chatID int64) error {
	_, err := s.db.Exec(`DELETE FROM schedules WHERE chat_id = ? AND type = 'heartbeat'`, chatID)
	return err
}

// GetSchedule returns a single schedule by ID scoped to a chat, or nil if not found.
func (s *Store) GetSchedule(chatID, id int64) (*Schedule, error) {
	row := s.db.QueryRow(`
		SELECT id, chat_id, label, message, schedule, timezone, type, mode, next_run_at, last_run_at, enabled, created_at
		FROM schedules WHERE id = ? AND chat_id = ?
	`, id, chatID)

	var sc Schedule
	var enabled int
	var lastRun sql.NullTime
	err := row.Scan(&sc.ID, &sc.ChatID, &sc.Label, &sc.Message, &sc.Schedule, &sc.Timezone, &sc.Type, &sc.Mode, &sc.NextRunAt, &lastRun, &enabled, &sc.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sc.Enabled = enabled != 0
	if lastRun.Valid {
		sc.LastRunAt = &lastRun.Time
	}
	return &sc, nil
}

// EnableSchedule sets enabled=1 for a schedule.
func (s *Store) EnableSchedule(id int64) error {
	_, err := s.db.Exec(`UPDATE schedules SET enabled = 1 WHERE id = ?`, id)
	return err
}

// DeleteSchedule removes a schedule scoped to a specific chat.
func (s *Store) DeleteSchedule(chatID, id int64) error {
	result, err := s.db.Exec(`DELETE FROM schedules WHERE id = ? AND chat_id = ?`, id, chatID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("schedule #%d not found", id)
	}
	return nil
}

// Task represents a background task for heartbeat to pick up.
type Task struct {
	ID          int64
	ChatID      int64
	Description string
	Status      string // "pending", "in_progress", "completed"
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// AddTask adds a background task to the queue.
func (s *Store) AddTask(chatID int64, description string) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO tasks (chat_id, description) VALUES (?, ?)`,
		chatID, description,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// PendingTasks returns all pending tasks for a chat.
func (s *Store) PendingTasks(chatID int64) ([]Task, error) {
	rows, err := s.db.Query(
		`SELECT id, chat_id, description, status, created_at FROM tasks WHERE chat_id = ? AND status = 'pending' ORDER BY created_at ASC`,
		chatID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.ChatID, &t.Description, &t.Status, &t.CreatedAt); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

// CompleteTask marks a task as completed.
func (s *Store) CompleteTask(id int64) error {
	_, err := s.db.Exec(
		`UPDATE tasks SET status = 'completed', completed_at = CURRENT_TIMESTAMP WHERE id = ?`,
		id,
	)
	return err
}

// DeleteTask removes a task by ID scoped to a chat.
func (s *Store) DeleteTask(chatID, id int64) error {
	_, err := s.db.Exec(`DELETE FROM tasks WHERE id = ? AND chat_id = ?`, id, chatID)
	return err
}

func (s *Store) Close() error {
	return s.db.Close()
}
