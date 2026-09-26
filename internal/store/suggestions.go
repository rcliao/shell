package store

import (
	"strconv"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Suggestions and feedback (docs/DESIGN-ROUTER-AND-SUGGESTIONS.md, S0).
//
// An agent files a suggestion during its weekly review with a tool call; the
// harness delivers it to the owner; the owner's answer is recorded here and
// shown to the agent at the next review. That round trip is how an agent
// learns what its human values. A table, not ghost fields: a lifecycle needs
// exact status queries, and ghost stays the memory, not the ledger.

// Suggestion statuses. proposed → delivered → accepted | declined, and
// accepted → done; withdrawn by the agent at any point before a decision.
const (
	SuggestionProposed  = "proposed"
	SuggestionDelivered = "delivered"
	SuggestionAccepted  = "accepted"
	SuggestionDeclined  = "declined"
	SuggestionDone      = "done"
	SuggestionWithdrawn = "withdrawn"
)

// Suggestion is one change an agent asked a human for.
type Suggestion struct {
	ID          int64
	CreatedAt   time.Time
	Source      string // "review" today
	Audience    string // "owner" today
	Title       string
	Evidence    string
	Change      string
	Status      string
	DeliveredAt *time.Time
	DecidedAt   *time.Time
	DecidedBy   string
	Note        string
}

const suggestionsSchema = `
CREATE TABLE IF NOT EXISTS suggestions (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	source       TEXT NOT NULL DEFAULT 'review',
	audience     TEXT NOT NULL DEFAULT 'owner',
	title        TEXT NOT NULL,
	evidence     TEXT NOT NULL DEFAULT '',
	change       TEXT NOT NULL DEFAULT '',
	status       TEXT NOT NULL DEFAULT 'proposed',
	delivered_at DATETIME,
	decided_at   DATETIME,
	decided_by   TEXT NOT NULL DEFAULT '',
	note         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_suggestions_status ON suggestions(status, created_at);

CREATE TABLE IF NOT EXISTS feedback_events (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	chat_id    INTEGER NOT NULL,
	thread_id  INTEGER NOT NULL DEFAULT 0,
	msg_id     INTEGER NOT NULL DEFAULT 0,
	kind       TEXT NOT NULL,
	value      TEXT NOT NULL DEFAULT '',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_feedback_created ON feedback_events(created_at);
`

// CreateSuggestion files a new suggestion as proposed and returns its id.
func (s *Store) CreateSuggestion(sg Suggestion) (int64, error) {
	if strings.TrimSpace(sg.Title) == "" {
		return 0, fmt.Errorf("suggestion needs a title")
	}
	if sg.Source == "" {
		sg.Source = "review"
	}
	if sg.Audience == "" {
		sg.Audience = "owner"
	}
	res, err := s.db.Exec(`
		INSERT INTO suggestions (created_at, source, audience, title, evidence, change, status)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC(), sg.Source, sg.Audience, sg.Title, sg.Evidence, sg.Change, SuggestionProposed)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const suggestionColumns = `id, created_at, source, audience, title, evidence, change, status,
	delivered_at, decided_at, decided_by, note`

// ListSuggestions returns suggestions, newest first. Empty statuses = all.
func (s *Store) ListSuggestions(statuses []string, limit int) ([]Suggestion, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT ` + suggestionColumns + ` FROM suggestions`
	var args []any
	if len(statuses) > 0 {
		q += ` WHERE status IN (?` + strings.Repeat(",?", len(statuses)-1) + `)`
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	return s.querySuggestions(q, args...)
}

// SuggestionsDecidedSince returns suggestions decided at or after a cutoff.
func (s *Store) SuggestionsDecidedSince(since time.Time) ([]Suggestion, error) {
	return s.querySuggestions(`SELECT `+suggestionColumns+` FROM suggestions
		WHERE decided_at IS NOT NULL AND decided_at >= ? ORDER BY decided_at DESC`, since.UTC())
}

// GetSuggestion returns one suggestion, or nil when the id is unknown.
func (s *Store) GetSuggestion(id int64) (*Suggestion, error) {
	out, err := s.querySuggestions(`SELECT `+suggestionColumns+` FROM suggestions WHERE id = ?`, id)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

func (s *Store) querySuggestions(q string, args ...any) ([]Suggestion, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Suggestion
	for rows.Next() {
		var sg Suggestion
		var delivered, decided sql.NullTime
		if err := rows.Scan(&sg.ID, &sg.CreatedAt, &sg.Source, &sg.Audience, &sg.Title, &sg.Evidence, &sg.Change,
			&sg.Status, &delivered, &decided, &sg.DecidedBy, &sg.Note); err != nil {
			return nil, err
		}
		if delivered.Valid {
			t := delivered.Time
			sg.DeliveredAt = &t
		}
		if decided.Valid {
			t := decided.Time
			sg.DecidedAt = &t
		}
		out = append(out, sg)
	}
	return out, rows.Err()
}

// MarkSuggestionsDelivered moves proposed suggestions to delivered.
func (s *Store) MarkSuggestionsDelivered(ids []int64) error {
	for _, id := range ids {
		if _, err := s.db.Exec(`UPDATE suggestions SET status = ?, delivered_at = ?
			WHERE id = ? AND status = ?`, SuggestionDelivered, time.Now().UTC(), id, SuggestionProposed); err != nil {
			return err
		}
	}
	return nil
}

// validTransitions guards the lifecycle: a decision is final except that an
// accepted suggestion can later be marked done.
var validTransitions = map[string][]string{
	SuggestionAccepted:  {SuggestionProposed, SuggestionDelivered},
	SuggestionDeclined:  {SuggestionProposed, SuggestionDelivered},
	SuggestionWithdrawn: {SuggestionProposed, SuggestionDelivered},
	SuggestionDone:      {SuggestionAccepted},
}

// DecideSuggestion records a decision (accepted, declined, done, withdrawn).
func (s *Store) DecideSuggestion(id int64, status, by, note string) error {
	from, ok := validTransitions[status]
	if !ok {
		return fmt.Errorf("unknown decision %q (accepted, declined, done, withdrawn)", status)
	}
	cur, err := s.GetSuggestion(id)
	if err != nil {
		return err
	}
	if cur == nil {
		return fmt.Errorf("no suggestion #%d", id)
	}
	allowed := false
	for _, f := range from {
		if cur.Status == f {
			allowed = true
		}
	}
	if !allowed {
		return fmt.Errorf("suggestion #%d is %s; cannot mark it %s", id, cur.Status, status)
	}
	_, err = s.db.Exec(`UPDATE suggestions SET status = ?, decided_at = ?, decided_by = ?, note = ? WHERE id = ?`,
		status, time.Now().UTC(), by, note, id)
	return err
}

// LogFeedback records one feedback signal (today: a reaction emoji).
func (s *Store) LogFeedback(chatID, threadID, msgID int64, kind, value string) error {
	_, err := s.db.Exec(`INSERT INTO feedback_events (chat_id, thread_id, msg_id, kind, value, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, chatID, threadID, msgID, kind, value, time.Now().UTC())
	return err
}

// FeedbackCounts returns counts per value of one kind since a cutoff.
func (s *Store) FeedbackCounts(kind string, since time.Time) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT value, COUNT(*) FROM feedback_events
		WHERE kind = ? AND created_at >= ? GROUP BY value`, kind, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var v string
		var n int
		if err := rows.Scan(&v, &n); err != nil {
			return nil, err
		}
		out[v] = n
	}
	return out, rows.Err()
}

// ChatAudience is the audience of a suggestion posted into a chat (S1).
func ChatAudience(chatID int64) string { return "chat:" + strconv.FormatInt(chatID, 10) }

// ProposedFor returns proposed (not yet delivered) suggestions for one
// audience, oldest first.
func (s *Store) ProposedFor(audience string) ([]Suggestion, error) {
	return s.querySuggestions(`SELECT `+suggestionColumns+` FROM suggestions
		WHERE status = ? AND audience = ? ORDER BY id`, SuggestionProposed, audience)
}

// OpenChatSuggestions returns suggestions delivered to a chat and not yet
// decided, at most maxAge old — the ones its turns should know are pending.
func (s *Store) OpenChatSuggestions(chatID int64, maxAge time.Duration) ([]Suggestion, error) {
	return s.querySuggestions(`SELECT `+suggestionColumns+` FROM suggestions
		WHERE status = ? AND audience = ? AND delivered_at >= ? ORDER BY id`,
		SuggestionDelivered, ChatAudience(chatID), time.Now().Add(-maxAge).UTC())
}
