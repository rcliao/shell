package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// TopicThread is the per-(chat_id, topic) running state introduced in
// cycle 67. Holds rolling summary + open commitments for thread-state
// injection into Channel B.
//
// Distinct from the topic REGISTRY (which lives in ghost ns loop:topics
// and catalogs which topics exist): this is operational state.
type TopicThread struct {
	ID              int64
	ChatID          int64
	Topic           string
	Summary         string
	OpenCommitments []Commitment // parsed from JSON column
	LastTurnAt      *time.Time
	LastTurnMsgID   int64
	TurnCount       int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Commitment is one open follow-up tracked inside a thread.
// Cycle 67 ships the data shape + manual-population path; cycle 68+
// auto-extracts these from agent responses.
type Commitment struct {
	Action  string    `json:"action"`             // free-form description
	DueAt   time.Time `json:"due_at,omitempty"`   // when to revisit
	Status  string    `json:"status"`             // open | done | overdue | cancelled
	Source  string    `json:"source,omitempty"`   // which turn/msg established this
	CreatedAt time.Time `json:"created_at"`
}

// IsOverdue reports whether a commitment is past its due date and still open.
func (c Commitment) IsOverdue() bool {
	if c.Status != "open" {
		return false
	}
	if c.DueAt.IsZero() {
		return false
	}
	return time.Now().After(c.DueAt)
}

// GetTopicThread returns the thread for (chat_id, topic), or nil if absent.
func (s *Store) GetTopicThread(chatID int64, topic string) (*TopicThread, error) {
	row := s.db.QueryRow(`
		SELECT id, chat_id, topic, summary, open_commitments,
		       last_turn_at, last_turn_msg_id, turn_count,
		       created_at, updated_at
		FROM topic_threads
		WHERE chat_id = ? AND topic = ?`, chatID, topic)
	var t TopicThread
	var commJSON string
	var lastTurnAt sql.NullTime
	err := row.Scan(&t.ID, &t.ChatID, &t.Topic, &t.Summary, &commJSON,
		&lastTurnAt, &t.LastTurnMsgID, &t.TurnCount, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lastTurnAt.Valid {
		t.LastTurnAt = &lastTurnAt.Time
	}
	if commJSON != "" {
		if err := json.Unmarshal([]byte(commJSON), &t.OpenCommitments); err != nil {
			// non-fatal — log via caller; return with empty list
			t.OpenCommitments = nil
		}
	}
	return &t, nil
}

// UpsertTopicThread creates or updates the thread row for (chat_id, topic).
// Bumps turn_count + last_turn_at + last_turn_msg_id on each call.
//
// Summary and commitments are passed through unchanged — the writer is
// responsible for rolling them. (Cycle 68's job: auto-extract.)
func (s *Store) UpsertTopicThread(t TopicThread) (*TopicThread, error) {
	if t.Topic == "" {
		return nil, fmt.Errorf("topic required")
	}
	commJSON, _ := json.Marshal(t.OpenCommitments)
	if len(commJSON) == 0 {
		commJSON = []byte("[]")
	}
	now := time.Now()
	if t.LastTurnAt == nil {
		t.LastTurnAt = &now
	}

	_, err := s.db.Exec(`
		INSERT INTO topic_threads
		  (chat_id, topic, summary, open_commitments, last_turn_at, last_turn_msg_id, turn_count, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, topic) DO UPDATE SET
		  summary = excluded.summary,
		  open_commitments = excluded.open_commitments,
		  last_turn_at = excluded.last_turn_at,
		  last_turn_msg_id = excluded.last_turn_msg_id,
		  turn_count = topic_threads.turn_count + 1,
		  updated_at = excluded.updated_at`,
		t.ChatID, t.Topic, t.Summary, string(commJSON),
		*t.LastTurnAt, t.LastTurnMsgID,
		max(t.TurnCount, 1), now)
	if err != nil {
		return nil, err
	}
	return s.GetTopicThread(t.ChatID, t.Topic)
}

// BumpTopicThread is the minimal write path: just update last-turn and
// increment count, leave summary/commitments untouched. Used by the
// bridge per turn as the lightweight default. Returns the updated row.
func (s *Store) BumpTopicThread(chatID int64, topic string, msgID int64) (*TopicThread, error) {
	if topic == "" {
		return nil, fmt.Errorf("topic required")
	}
	existing, err := s.GetTopicThread(chatID, topic)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if existing == nil {
		// Create with empty state.
		_, err := s.db.Exec(`
			INSERT INTO topic_threads
			  (chat_id, topic, summary, open_commitments, last_turn_at, last_turn_msg_id, turn_count)
			VALUES (?, ?, '', '[]', ?, ?, 1)`,
			chatID, topic, now, msgID)
		if err != nil {
			return nil, err
		}
	} else {
		_, err := s.db.Exec(`
			UPDATE topic_threads SET
			  last_turn_at = ?,
			  last_turn_msg_id = ?,
			  turn_count = turn_count + 1,
			  updated_at = ?
			WHERE id = ?`,
			now, msgID, now, existing.ID)
		if err != nil {
			return nil, err
		}
	}
	return s.GetTopicThread(chatID, topic)
}

// LogTopicTurn appends a row to the turn log for diagnostic / reconstruction.
// Lightweight — keeps the snippet to 200 chars.
func (s *Store) LogTopicTurn(threadID, msgID int64, role, snippet string) error {
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	_, err := s.db.Exec(`
		INSERT INTO topic_turn_log (thread_id, msg_id, role, snippet)
		VALUES (?, ?, ?, ?)`, threadID, msgID, role, snippet)
	return err
}

// ListOverdueCommitments returns commitments past their due date across
// all threads for a chat. Used by future heartbeats / surfacing UX.
func (s *Store) ListOverdueCommitments(chatID int64) ([]Commitment, error) {
	threads, err := s.ListTopicThreads(chatID)
	if err != nil {
		return nil, err
	}
	var out []Commitment
	for _, t := range threads {
		for _, c := range t.OpenCommitments {
			if c.IsOverdue() {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// ListTopicThreads returns all threads for a chat, ordered by recency.
func (s *Store) ListTopicThreads(chatID int64) ([]TopicThread, error) {
	return s.listTopicThreadsAny(chatID)
}

// listTopicThreadsAny lists threads — chatID=0 means all chats. Used by
// dashboard which expects "all chats" as default.
func (s *Store) listTopicThreadsAny(chatID int64) ([]TopicThread, error) {
	var q string
	var args []any
	if chatID == 0 {
		q = `SELECT id, chat_id, topic, summary, open_commitments,
		       last_turn_at, last_turn_msg_id, turn_count,
		       created_at, updated_at
		     FROM topic_threads
		     ORDER BY last_turn_at DESC NULLS LAST`
	} else {
		q = `SELECT id, chat_id, topic, summary, open_commitments,
		       last_turn_at, last_turn_msg_id, turn_count,
		       created_at, updated_at
		     FROM topic_threads WHERE chat_id = ?
		     ORDER BY last_turn_at DESC NULLS LAST`
		args = []any{chatID}
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TopicThread
	for rows.Next() {
		var t TopicThread
		var commJSON string
		var lastTurnAt sql.NullTime
		if err := rows.Scan(&t.ID, &t.ChatID, &t.Topic, &t.Summary, &commJSON,
			&lastTurnAt, &t.LastTurnMsgID, &t.TurnCount, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		if lastTurnAt.Valid {
			t.LastTurnAt = &lastTurnAt.Time
		}
		if commJSON != "" {
			json.Unmarshal([]byte(commJSON), &t.OpenCommitments)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetThreadSummary updates just the summary field. Used by cycle 68's
// summarizer (auto-extracted from agent responses).
func (s *Store) SetThreadSummary(chatID int64, topic, summary string) error {
	_, err := s.db.Exec(`
		UPDATE topic_threads SET summary = ?, updated_at = CURRENT_TIMESTAMP
		WHERE chat_id = ? AND topic = ?`,
		summary, chatID, topic)
	return err
}

// AddCommitment appends a commitment to a thread's open list.
func (s *Store) AddCommitment(chatID int64, topic string, c Commitment) error {
	t, err := s.GetTopicThread(chatID, topic)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("thread (%d, %s) does not exist", chatID, topic)
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	if c.Status == "" {
		c.Status = "open"
	}
	t.OpenCommitments = append(t.OpenCommitments, c)
	commJSON, _ := json.Marshal(t.OpenCommitments)
	_, err = s.db.Exec(`
		UPDATE topic_threads SET open_commitments = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`, string(commJSON), t.ID)
	return err
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
