package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Route decisions and labels (docs/DESIGN-ROUTER-AND-SUGGESTIONS.md, R0).
// Decisions are what a router backend chose for a message; labels are what
// the message really was about, from the strongest source available. The
// two join on (chat, thread, text hash) so one label scores every backend,
// live and replayed alike. Neither table stores message text.

const routeSchema = `
CREATE TABLE IF NOT EXISTS route_decisions (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	source      TEXT NOT NULL,
	chat_id     INTEGER NOT NULL,
	thread_id   INTEGER NOT NULL DEFAULT 0,
	msg_id      INTEGER NOT NULL DEFAULT 0,
	msg_at      DATETIME,
	text_hash   TEXT NOT NULL DEFAULT '',
	backend     TEXT NOT NULL,
	lane_prev   TEXT NOT NULL DEFAULT '',
	choice      TEXT NOT NULL DEFAULT '',
	confidence  REAL NOT NULL DEFAULT 0,
	lane        TEXT NOT NULL DEFAULT '',
	sticky      INTEGER NOT NULL DEFAULT 0,
	latency_ms  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_route_decisions_key ON route_decisions(source, backend, chat_id, thread_id, id);

CREATE TABLE IF NOT EXISTS route_labels (
	chat_id    INTEGER NOT NULL,
	thread_id  INTEGER NOT NULL DEFAULT 0,
	text_hash  TEXT NOT NULL,
	lane       TEXT NOT NULL,
	source     TEXT NOT NULL,
	sure       INTEGER NOT NULL DEFAULT 1,
	note       TEXT NOT NULL DEFAULT '',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(chat_id, thread_id, text_hash, source)
);
`

// Label sources, strongest first.
var labelRank = map[string]int{"human": 4, "agent": 3, "judge": 2, "project": 1}

// TextHash is the join key between a message, its decisions and its labels.
func TextHash(text string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return hex.EncodeToString(sum[:8])
}

// RouteDecision is one backend's routing of one message.
type RouteDecision struct {
	ID         int64
	Source     string // live | replay
	ChatID     int64
	ThreadID   int64
	MsgID      int64
	MsgAt      time.Time
	TextHash   string
	Backend    string
	LanePrev   string
	Choice     string
	Confidence float64
	Lane       string
	Sticky     bool
	LatencyMS  int64
}

// LogRouteDecision records one decision.
func (s *Store) LogRouteDecision(d RouteDecision) error {
	sticky := 0
	if d.Sticky {
		sticky = 1
	}
	_, err := s.db.Exec(`INSERT INTO route_decisions (created_at, source, chat_id, thread_id, msg_id, msg_at,
		text_hash, backend, lane_prev, choice, confidence, lane, sticky, latency_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC(), d.Source, d.ChatID, d.ThreadID, d.MsgID, d.MsgAt.UTC(), d.TextHash, d.Backend,
		d.LanePrev, d.Choice, d.Confidence, d.Lane, sticky, d.LatencyMS)
	return err
}

// LastRouteLane returns the lane of the latest decision for a backend in a
// chat thread ("" when none) — the "previous lane" of the sticky rule.
func (s *Store) LastRouteLane(source, backend string, chatID, threadID int64) (string, error) {
	var lane string
	err := s.db.QueryRow(`SELECT lane FROM route_decisions WHERE source = ? AND backend = ?
		AND chat_id = ? AND thread_id = ? ORDER BY id DESC LIMIT 1`, source, backend, chatID, threadID).Scan(&lane)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil // the normal first-message case
	}
	return lane, err
}

// ClearRouteDecisions deletes decisions of one source (a replay is rerun
// from scratch rather than appended to).
func (s *Store) ClearRouteDecisions(source string) error {
	_, err := s.db.Exec(`DELETE FROM route_decisions WHERE source = ?`, source)
	return err
}

// RouteDecisions returns decisions of a source since a cutoff, oldest first.
func (s *Store) RouteDecisions(source string, since time.Time) ([]RouteDecision, error) {
	rows, err := s.db.Query(`SELECT id, source, chat_id, thread_id, msg_id, msg_at, text_hash,
		backend, lane_prev, choice, confidence, lane, sticky, latency_ms
		FROM route_decisions WHERE source = ? AND created_at >= ? ORDER BY id`, source, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RouteDecision
	for rows.Next() {
		var d RouteDecision
		var sticky int
		var msgAt sql.NullTime
		if err := rows.Scan(&d.ID, &d.Source, &d.ChatID, &d.ThreadID, &d.MsgID, &msgAt, &d.TextHash, &d.Backend,
			&d.LanePrev, &d.Choice, &d.Confidence, &d.Lane, &sticky, &d.LatencyMS); err != nil {
			return nil, err
		}
		d.MsgAt = msgAt.Time
		d.Sticky = sticky == 1
		out = append(out, d)
	}
	return out, rows.Err()
}

// RouteLabel is what a message was really about, according to one source.
type RouteLabel struct {
	ChatID   int64
	ThreadID int64
	TextHash string
	Lane     string
	Source   string // human | agent | judge | project
	Sure     bool
	Note     string
}

// UpsertRouteLabel records (or replaces) one source's label for a message.
func (s *Store) UpsertRouteLabel(l RouteLabel) error {
	sure := 0
	if l.Sure {
		sure = 1
	}
	_, err := s.db.Exec(`INSERT INTO route_labels (chat_id, thread_id, text_hash, lane, source, sure, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, thread_id, text_hash, source) DO UPDATE SET
			lane = excluded.lane, sure = excluded.sure, note = excluded.note, created_at = excluded.created_at`,
		l.ChatID, l.ThreadID, l.TextHash, l.Lane, l.Source, sure, l.Note, time.Now().UTC())
	return err
}

// BestRouteLabels returns, per message key, the label from the strongest
// source. Key: fmt "%d/%d/%s" of chat, thread, text hash (see LabelKey).
func (s *Store) BestRouteLabels() (map[string]RouteLabel, error) {
	rows, err := s.db.Query(`SELECT chat_id, thread_id, text_hash, lane, source, sure, note FROM route_labels`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RouteLabel{}
	for rows.Next() {
		var l RouteLabel
		var sure int
		if err := rows.Scan(&l.ChatID, &l.ThreadID, &l.TextHash, &l.Lane, &l.Source, &sure, &l.Note); err != nil {
			return nil, err
		}
		l.Sure = sure == 1
		k := LabelKey(l.ChatID, l.ThreadID, l.TextHash)
		if cur, ok := out[k]; !ok || labelRank[l.Source] > labelRank[cur.Source] {
			out[k] = l
		}
	}
	return out, rows.Err()
}

// LabelKey is the join key used by BestRouteLabels.
func LabelKey(chatID, threadID int64, textHash string) string {
	return strconv.FormatInt(chatID, 10) + "/" + strconv.FormatInt(threadID, 10) + "/" + textHash
}

// UserMessage is one real message a human sent this agent.
type UserMessage struct {
	ChatID   int64
	ThreadID int64
	At       time.Time
	Text     string
}

// UserMessagesSince returns the agent's real user messages since a cutoff,
// oldest first. Left out: chat 0 (the system chat), synthetic turns (text
// starting with "["), and scheduled prompts, which are logged as user
// messages in the chat they fire in but were written by a schedule, not a
// person (matched against every schedule's message text).
func (s *Store) UserMessagesSince(since time.Time) ([]UserMessage, error) {
	rows, err := s.db.Query(`SELECT s.chat_id, s.message_thread_id, m.created_at, m.content
		FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE m.role = 'user' AND s.chat_id != 0 AND m.created_at >= ?
		  AND substr(ltrim(m.content), 1, 1) != '['
		  AND m.content NOT IN (SELECT message FROM schedules)
		ORDER BY m.created_at, m.id`, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserMessage
	for rows.Next() {
		var m UserMessage
		if err := rows.Scan(&m.ChatID, &m.ThreadID, &m.At, &m.Text); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// LastUserText returns the latest user-role message in a chat thread — the
// message an agent is answering when it labels "this" message. It is NOT
// filtered: skipping a synthetic turn (a relayed peer message, "[…") would
// silently attach the label to an older human message; callers refuse
// synthetic text instead.
func (s *Store) LastUserText(chatID, threadID int64) (string, error) {
	var text string
	err := s.db.QueryRow(`SELECT m.content FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE s.chat_id = ? AND s.message_thread_id = ? AND m.role = 'user'
		ORDER BY m.id DESC LIMIT 1`, chatID, threadID).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return text, err
}
