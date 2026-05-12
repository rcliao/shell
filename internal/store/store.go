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
	ID                int64
	ChatID            int64
	MessageThreadID   int64 // Telegram forum topic ID (0 = main chat / no topic)
	ProviderSessionID string
	Status            string
	CreatedAt         time.Time
	UpdatedAt         time.Time

	// Lifecycle fields (see docs/SESSION-LIFECYCLE.md). `Generation` increments
	// on rotation; all other fields key off it. `PrefixHash` captures the frozen
	// Channel A contents (identity + skills + pinned memory snapshot) at
	// generation start — drift from the current live value triggers rotation.
	// `CompactState` is '' (idle) or 'compacting' (proactive compaction in
	// flight). `RotatePending` is a boolean flag set by soft triggers.
	Generation          int64
	PrefixHash          string
	GenerationStartedAt time.Time
	RotatePending       bool
	CompactState        string
}

// SessionSummary is the carry-forward artifact written at rotation. The
// `Summary` is the compacted conversation; `MemoryPack` is JSON with the
// top-N semantically-relevant memories selected from ghost at close time.
type SessionSummary struct {
	ID         int64
	ChatID     int64
	ThreadID   int64
	Generation int64
	ClosedAt   time.Time
	Summary    string
	MemoryPack string
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
	// Phase 1: create tables. `CREATE TABLE IF NOT EXISTS` is a no-op on
	// legacy DBs that still have the old column-less schema, which is fine —
	// Phase 2 will rebuild the sessions table to add message_thread_id.
	// Indexes that reference message_thread_id must wait until Phase 3 so they
	// don't fail on legacy DBs during the initial apply.
	tableSchema := `
	CREATE TABLE IF NOT EXISTS sessions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id INTEGER NOT NULL,
		message_thread_id INTEGER NOT NULL DEFAULT 0,
		claude_session_id TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'active',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(chat_id, message_thread_id)
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
	if _, err := s.db.Exec(tableSchema); err != nil {
		return err
	}

	// Add columns for message content (idempotent for existing databases).
	for _, col := range []string{
		"ALTER TABLE message_map ADD COLUMN user_message TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE message_map ADD COLUMN bot_response TEXT NOT NULL DEFAULT ''",
	} {
		s.db.Exec(col) // ignore "duplicate column" errors
	}

	// Phase 2: upgrade legacy sessions tables that pre-date message_thread_id.
	// Old schema had UNIQUE(chat_id); the new schema needs UNIQUE(chat_id, message_thread_id).
	// SQLite can't alter constraints in-place — rebuild the table.
	if err := s.upgradeSessionsThreadID(); err != nil {
		return fmt.Errorf("upgrade sessions for message_thread_id: %w", err)
	}

	// Phase 3: composite index now that the column is guaranteed to exist.
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_chat_thread ON sessions(chat_id, message_thread_id)`); err != nil {
		return fmt.Errorf("create idx_sessions_chat_thread: %w", err)
	}

	// Token usage tracking per exchange.
	usageSchema := `
	CREATE TABLE IF NOT EXISTS usage (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id INTEGER NOT NULL,
		session_id INTEGER NOT NULL,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens INTEGER NOT NULL DEFAULT 0,
		cost_usd REAL NOT NULL DEFAULT 0,
		num_turns INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_usage_chat_id ON usage(chat_id);
	CREATE INDEX IF NOT EXISTS idx_usage_created_at ON usage(created_at);
	`
	if _, err := s.db.Exec(usageSchema); err != nil {
		return err
	}

	// Add source column to usage table (idempotent for existing databases).
	s.db.Exec("ALTER TABLE usage ADD COLUMN source TEXT NOT NULL DEFAULT 'interactive'")

	// Phase 4: lifecycle columns on sessions (see docs/SESSION-LIFECYCLE.md).
	// Idempotent — duplicate-column errors are ignored on legacy DBs.
	for _, col := range []string{
		"ALTER TABLE sessions ADD COLUMN generation INTEGER NOT NULL DEFAULT 1",
		"ALTER TABLE sessions ADD COLUMN prefix_hash TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE sessions ADD COLUMN generation_started_at DATETIME",
		"ALTER TABLE sessions ADD COLUMN rotate_pending INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE sessions ADD COLUMN compact_state TEXT NOT NULL DEFAULT ''",
	} {
		s.db.Exec(col)
	}
	// Backfill generation_started_at for rows that predate the column.
	s.db.Exec(`UPDATE sessions SET generation_started_at = created_at WHERE generation_started_at IS NULL`)

	// Session summaries: one row per closed generation, used as the
	// carry-forward pack when the next generation starts.
	summarySchema := `
	CREATE TABLE IF NOT EXISTS session_summaries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id INTEGER NOT NULL,
		message_thread_id INTEGER NOT NULL DEFAULT 0,
		generation INTEGER NOT NULL,
		closed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		summary TEXT NOT NULL,
		memory_pack TEXT NOT NULL DEFAULT '',
		UNIQUE(chat_id, message_thread_id, generation)
	);
	CREATE INDEX IF NOT EXISTS idx_session_summaries_key
		ON session_summaries(chat_id, message_thread_id, generation DESC);
	`
	if _, err := s.db.Exec(summarySchema); err != nil {
		return err
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

// upgradeSessionsThreadID adds the message_thread_id column and rebuilds the
// sessions table with a composite UNIQUE(chat_id, message_thread_id) constraint
// when upgrading from pre-thread-support databases.
func (s *Store) upgradeSessionsThreadID() error {
	// Does message_thread_id already exist?
	rows, err := s.db.Query(`PRAGMA table_info(sessions)`)
	if err != nil {
		return err
	}
	hasThreadID := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "message_thread_id" {
			hasThreadID = true
		}
	}
	rows.Close()
	if hasThreadID {
		return nil // nothing to do — fresh installs and already-migrated DBs
	}

	// Rebuild table inside a transaction.
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	steps := []string{
		`CREATE TABLE sessions_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id INTEGER NOT NULL,
			message_thread_id INTEGER NOT NULL DEFAULT 0,
			claude_session_id TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(chat_id, message_thread_id)
		)`,
		`INSERT INTO sessions_new (id, chat_id, message_thread_id, claude_session_id, status, created_at, updated_at)
			SELECT id, chat_id, 0, claude_session_id, status, created_at, updated_at FROM sessions`,
		`DROP TABLE sessions`,
		`ALTER TABLE sessions_new RENAME TO sessions`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_chat_id ON sessions(chat_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_chat_thread ON sessions(chat_id, message_thread_id)`,
	}
	for _, stmt := range steps {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt[:min(60, len(stmt))], err)
		}
	}
	return tx.Commit()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *Store) SaveSession(chatID, threadID int64, claudeSessionID string) error {
	// Preserve lifecycle fields on upsert — only the conversation UUID,
	// status, and updated_at advance when a turn writes back. Generation /
	// prefix_hash / compact_state are owned by the rotation + compaction
	// paths and must not be clobbered by regular SaveSession calls.
	_, err := s.db.Exec(`
		INSERT INTO sessions (chat_id, message_thread_id, claude_session_id, status,
			created_at, updated_at, generation, generation_started_at)
		VALUES (?, ?, ?, 'active', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 1, CURRENT_TIMESTAMP)
		ON CONFLICT(chat_id, message_thread_id) DO UPDATE SET
			claude_session_id = excluded.claude_session_id,
			status = 'active',
			updated_at = CURRENT_TIMESTAMP
	`, chatID, threadID, claudeSessionID)
	return err
}

// SetPrefixHash records the hash of Channel A contents (identity + skills +
// pinned memory snapshot) at generation start. Channel B diff detection
// compares live pinned-memory hash against this value.
func (s *Store) SetPrefixHash(chatID, threadID int64, hash string) error {
	_, err := s.db.Exec(`
		UPDATE sessions SET prefix_hash = ?, updated_at = CURRENT_TIMESTAMP
		WHERE chat_id = ? AND message_thread_id = ?
	`, hash, chatID, threadID)
	return err
}

// BumpGeneration increments the generation counter, resets generation_started_at,
// clears the Claude session UUID (so the next turn starts fresh), stamps a new
// prefix_hash, and clears rotate_pending + compact_state. Returns the new
// generation number.
func (s *Store) BumpGeneration(chatID, threadID int64, newPrefixHash string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var gen int64
	err = tx.QueryRow(`
		SELECT generation FROM sessions WHERE chat_id = ? AND message_thread_id = ?
	`, chatID, threadID).Scan(&gen)
	if err != nil {
		return 0, err
	}
	gen++

	_, err = tx.Exec(`
		UPDATE sessions SET
			generation = ?,
			generation_started_at = CURRENT_TIMESTAMP,
			claude_session_id = '',
			prefix_hash = ?,
			rotate_pending = 0,
			compact_state = '',
			updated_at = CURRENT_TIMESTAMP
		WHERE chat_id = ? AND message_thread_id = ?
	`, gen, newPrefixHash, chatID, threadID)
	if err != nil {
		return 0, err
	}
	return gen, tx.Commit()
}

// SetRotatePending flags a session for rotation on its next turn. Soft
// triggers (7-day age, pinned delta over budget) call this; the bridge
// checks the flag before each Send and calls BumpGeneration if set.
func (s *Store) SetRotatePending(chatID, threadID int64, pending bool) error {
	v := 0
	if pending {
		v = 1
	}
	_, err := s.db.Exec(`
		UPDATE sessions SET rotate_pending = ?, updated_at = CURRENT_TIMESTAMP
		WHERE chat_id = ? AND message_thread_id = ?
	`, v, chatID, threadID)
	return err
}

// SetCompactState records the proactive-compaction state machine transition:
// '' (idle) or 'compacting' (background /compact in flight).
func (s *Store) SetCompactState(chatID, threadID int64, state string) error {
	_, err := s.db.Exec(`
		UPDATE sessions SET compact_state = ?, updated_at = CURRENT_TIMESTAMP
		WHERE chat_id = ? AND message_thread_id = ?
	`, state, chatID, threadID)
	return err
}

// SaveSessionSummary writes the carry-forward artifact for a closed generation.
// Called by rotateSession() just before BumpGeneration. memoryPack is a JSON
// blob (schema owned by bridge); empty string is valid.
func (s *Store) SaveSessionSummary(chatID, threadID, generation int64, summary, memoryPack string) error {
	_, err := s.db.Exec(`
		INSERT INTO session_summaries (chat_id, message_thread_id, generation, summary, memory_pack)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, message_thread_id, generation) DO UPDATE SET
			summary = excluded.summary,
			memory_pack = excluded.memory_pack,
			closed_at = CURRENT_TIMESTAMP
	`, chatID, threadID, generation, summary, memoryPack)
	return err
}

// GetLatestSessionSummary returns the most recently closed generation's
// summary for a chat+thread, or nil if no prior generation exists.
// Used on fresh-session turns after rotation to build the carry-forward pack.
func (s *Store) GetLatestSessionSummary(chatID, threadID int64) (*SessionSummary, error) {
	row := s.db.QueryRow(`
		SELECT id, chat_id, message_thread_id, generation, closed_at, summary, memory_pack
		FROM session_summaries
		WHERE chat_id = ? AND message_thread_id = ?
		ORDER BY generation DESC LIMIT 1
	`, chatID, threadID)

	var sm SessionSummary
	err := row.Scan(&sm.ID, &sm.ChatID, &sm.ThreadID, &sm.Generation, &sm.ClosedAt, &sm.Summary, &sm.MemoryPack)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sm, nil
}

// SessionThreadID returns the message_thread_id for a session row by its
// primary key, or 0 if the row doesn't exist. Used by reactions to resolve
// which topic a previously-mapped exchange belongs to.
func (s *Store) SessionThreadID(sessionID int64) int64 {
	var threadID int64
	err := s.db.QueryRow(`SELECT message_thread_id FROM sessions WHERE id = ?`, sessionID).Scan(&threadID)
	if err != nil {
		return 0
	}
	return threadID
}

func (s *Store) GetSession(chatID, threadID int64) (*Session, error) {
	row := s.db.QueryRow(`
		SELECT id, chat_id, message_thread_id, claude_session_id, status, created_at, updated_at,
		       generation, prefix_hash, generation_started_at, rotate_pending, compact_state
		FROM sessions WHERE chat_id = ? AND message_thread_id = ?
	`, chatID, threadID)
	return scanSession(row)
}

// scanSession extracts a Session from a row including lifecycle fields.
// Accepts anything with a Scan method matching the 12-column SELECT used by
// GetSession and ListActiveSessions.
func scanSession(row interface{ Scan(...any) error }) (*Session, error) {
	var sess Session
	var rotatePending int
	var genStarted sql.NullTime
	err := row.Scan(
		&sess.ID, &sess.ChatID, &sess.MessageThreadID, &sess.ProviderSessionID,
		&sess.Status, &sess.CreatedAt, &sess.UpdatedAt,
		&sess.Generation, &sess.PrefixHash, &genStarted, &rotatePending, &sess.CompactState,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if genStarted.Valid {
		sess.GenerationStartedAt = genStarted.Time
	} else {
		sess.GenerationStartedAt = sess.CreatedAt
	}
	sess.RotatePending = rotatePending != 0
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

// RecentExchanges returns the last N (user, bot) exchanges for a session
// ordered from oldest to newest. Used to build rotation summaries — we pull
// the tail of the conversation as a cheap mechanical summary before handing
// it to ghost for semantic enrichment.
func (s *Store) RecentExchanges(sessionID int64, limit int) ([]MessageMap, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.Query(`
		SELECT id, chat_id, user_message_id, bot_message_id, session_id,
		       user_message, bot_response, created_at
		FROM message_map
		WHERE session_id = ?
		ORDER BY id DESC
		LIMIT ?
	`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MessageMap
	for rows.Next() {
		var m MessageMap
		if err := rows.Scan(&m.ID, &m.ChatID, &m.UserMessageID, &m.BotMessageID, &m.SessionID,
			&m.UserMessage, &m.BotResponse, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	// Reverse to chronological order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
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

// DeleteSession deletes the session(s) for a chat. If threadID >= 0, only
// the session for that specific topic is deleted. Pass -1 to delete ALL
// topic sessions for the chat (used by /new and DeleteSession CLI).
func (s *Store) DeleteSession(chatID, threadID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	allTopics := threadID < 0

	var sessionFilter string
	var sessionArgs []any
	if allTopics {
		sessionFilter = `SELECT id FROM sessions WHERE chat_id = ?`
		sessionArgs = []any{chatID}
	} else {
		sessionFilter = `SELECT id FROM sessions WHERE chat_id = ? AND message_thread_id = ?`
		sessionArgs = []any{chatID, threadID}
	}

	// Delete message_map entries first
	_, err = tx.Exec(`DELETE FROM message_map WHERE session_id IN (`+sessionFilter+`)`, sessionArgs...)
	if err != nil {
		return err
	}

	// Delete messages
	_, err = tx.Exec(`DELETE FROM messages WHERE session_id IN (`+sessionFilter+`)`, sessionArgs...)
	if err != nil {
		return err
	}

	if allTopics {
		_, err = tx.Exec(`DELETE FROM sessions WHERE chat_id = ?`, chatID)
	} else {
		_, err = tx.Exec(`DELETE FROM sessions WHERE chat_id = ? AND message_thread_id = ?`, chatID, threadID)
	}
	if err != nil {
		return err
	}

	// Cascade session summaries — carry-forward artifacts are meaningless
	// once the session is gone.
	if allTopics {
		_, err = tx.Exec(`DELETE FROM session_summaries WHERE chat_id = ?`, chatID)
	} else {
		_, err = tx.Exec(`DELETE FROM session_summaries WHERE chat_id = ? AND message_thread_id = ?`, chatID, threadID)
	}
	if err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) UpdateSessionStatus(chatID, threadID int64, status string) error {
	_, err := s.db.Exec(`
		UPDATE sessions SET status = ?, updated_at = CURRENT_TIMESTAMP
		WHERE chat_id = ? AND message_thread_id = ?
	`, status, chatID, threadID)
	return err
}

func (s *Store) ListActiveSessions() ([]Session, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, message_thread_id, claude_session_id, status, created_at, updated_at,
		       generation, prefix_hash, generation_started_at, rotate_pending, compact_state
		FROM sessions WHERE status = 'active' ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		if sess != nil {
			sessions = append(sessions, *sess)
		}
	}
	return sessions, nil
}

// StaleSessionRef identifies a stale session by its (chat_id, message_thread_id) key.
type StaleSessionRef struct {
	ChatID   int64
	ThreadID int64
}

func (s *Store) StaleSessionRefs(idleDuration time.Duration) ([]StaleSessionRef, error) {
	cutoff := time.Now().Add(-idleDuration)
	rows, err := s.db.Query(`
		SELECT chat_id, message_thread_id FROM sessions
		WHERE status = 'active' AND updated_at < ?
	`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var refs []StaleSessionRef
	for rows.Next() {
		var r StaleSessionRef
		if err := rows.Scan(&r.ChatID, &r.ThreadID); err != nil {
			return nil, err
		}
		refs = append(refs, r)
	}
	return refs, nil
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

// UsageSummary contains aggregated token usage data.
type UsageSummary struct {
	TotalInputTokens         int64
	TotalOutputTokens        int64
	TotalCacheCreationTokens int64
	TotalCacheReadTokens     int64
	TotalCostUSD             float64
	TotalTurns               int64
	ExchangeCount            int64
}

// LogUsage records token usage for a single exchange.
// source identifies the origin: "interactive", "heartbeat", or "scheduler".
func (s *Store) LogUsage(chatID, sessionID int64, inputTokens, outputTokens, cacheCreation, cacheRead int, costUSD float64, numTurns int, source string) error {
	if source == "" {
		source = "interactive"
	}
	_, err := s.db.Exec(`
		INSERT INTO usage (chat_id, session_id, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, cost_usd, num_turns, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, chatID, sessionID, inputTokens, outputTokens, cacheCreation, cacheRead, costUSD, numTurns, source)
	return err
}

// GetUsageSummary returns aggregated usage for a chat, optionally filtered by time.
// If since is zero, returns all-time usage.
func (s *Store) GetUsageSummary(chatID int64, since time.Time) (*UsageSummary, error) {
	var query string
	var args []any
	if since.IsZero() {
		query = `
			SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			       COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(cache_read_tokens),0),
			       COALESCE(SUM(cost_usd),0), COALESCE(SUM(num_turns),0), COUNT(*)
			FROM usage WHERE chat_id = ?`
		args = []any{chatID}
	} else {
		query = `
			SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			       COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(cache_read_tokens),0),
			       COALESCE(SUM(cost_usd),0), COALESCE(SUM(num_turns),0), COUNT(*)
			FROM usage WHERE chat_id = ? AND created_at >= ?`
		args = []any{chatID, since}
	}

	var u UsageSummary
	err := s.db.QueryRow(query, args...).Scan(
		&u.TotalInputTokens, &u.TotalOutputTokens,
		&u.TotalCacheCreationTokens, &u.TotalCacheReadTokens,
		&u.TotalCostUSD, &u.TotalTurns, &u.ExchangeCount,
	)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUsageAllChats returns aggregated usage across all chats, optionally filtered by time.
func (s *Store) GetUsageAllChats(since time.Time) (*UsageSummary, error) {
	var query string
	var args []any
	if since.IsZero() {
		query = `
			SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			       COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(cache_read_tokens),0),
			       COALESCE(SUM(cost_usd),0), COALESCE(SUM(num_turns),0), COUNT(*)
			FROM usage`
	} else {
		query = `
			SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			       COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(cache_read_tokens),0),
			       COALESCE(SUM(cost_usd),0), COALESCE(SUM(num_turns),0), COUNT(*)
			FROM usage WHERE created_at >= ?`
		args = []any{since}
	}

	var u UsageSummary
	err := s.db.QueryRow(query, args...).Scan(
		&u.TotalInputTokens, &u.TotalOutputTokens,
		&u.TotalCacheCreationTokens, &u.TotalCacheReadTokens,
		&u.TotalCostUSD, &u.TotalTurns, &u.ExchangeCount,
	)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// CleanupOldMessages deletes messages older than the given duration.
func (s *Store) CleanupOldMessages(olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	result, err := s.db.Exec(`DELETE FROM messages WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// CleanupCompletedTasks deletes completed tasks older than the given duration.
func (s *Store) CleanupCompletedTasks(olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	result, err := s.db.Exec(`DELETE FROM tasks WHERE status = 'completed' AND completed_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// CleanupDisabledSchedules deletes disabled one-shot schedules.
func (s *Store) CleanupDisabledSchedules() (int64, error) {
	result, err := s.db.Exec(`DELETE FROM schedules WHERE enabled = 0 AND type = 'once'`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// GetMessageCount returns the number of messages for a chat since the given time.
func (s *Store) GetMessageCount(chatID int64, since time.Time) (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM messages m
		JOIN sessions s ON m.session_id = s.id
		WHERE s.chat_id = ? AND m.created_at >= ?
	`, chatID, since).Scan(&count)
	return count, err
}

// GetSessionRotations returns the number of distinct sessions with usage since the given time.
func (s *Store) GetSessionRotations(chatID int64, since time.Time) (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(DISTINCT session_id) FROM usage
		WHERE chat_id = ? AND created_at >= ?
	`, chatID, since).Scan(&count)
	return count, err
}

// GetUsageSummaryBySource returns usage grouped by source (interactive, heartbeat, scheduler).
func (s *Store) GetUsageSummaryBySource(chatID int64, since time.Time) (map[string]*UsageSummary, error) {
	rows, err := s.db.Query(`
		SELECT COALESCE(source, 'interactive'),
		       COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		       COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(cache_read_tokens),0),
		       COALESCE(SUM(cost_usd),0), COALESCE(SUM(num_turns),0), COUNT(*)
		FROM usage WHERE chat_id = ? AND created_at >= ?
		GROUP BY COALESCE(source, 'interactive')
	`, chatID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]*UsageSummary)
	for rows.Next() {
		var source string
		var u UsageSummary
		if err := rows.Scan(&source,
			&u.TotalInputTokens, &u.TotalOutputTokens,
			&u.TotalCacheCreationTokens, &u.TotalCacheReadTokens,
			&u.TotalCostUSD, &u.TotalTurns, &u.ExchangeCount,
		); err != nil {
			return nil, err
		}
		result[source] = &u
	}
	return result, nil
}

// GetActiveScheduleCount returns the number of enabled schedules for a chat.
func (s *Store) GetActiveScheduleCount(chatID int64) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM schedules WHERE chat_id = ? AND enabled = 1`, chatID).Scan(&count)
	return count, err
}

func (s *Store) Close() error {
	return s.db.Close()
}
