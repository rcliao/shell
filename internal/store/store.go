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
	ClaudeSessionID  string
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

	CREATE INDEX IF NOT EXISTS idx_sessions_chat_id ON sessions(chat_id);
	CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id);
	`
	_, err := s.db.Exec(schema)
	return err
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
	err := row.Scan(&sess.ID, &sess.ChatID, &sess.ClaudeSessionID, &sess.Status, &sess.CreatedAt, &sess.UpdatedAt)
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

func (s *Store) DeleteSession(chatID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Delete messages first
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
		if err := rows.Scan(&sess.ID, &sess.ChatID, &sess.ClaudeSessionID, &sess.Status, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
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

func (s *Store) Close() error {
	return s.db.Close()
}
