package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Browser handoffs (docs/DESIGN-BROWSER-HANDOFF.md): an agent hands its
// browser tab to a person through a tailnet-only live view, or lets them watch
// it. A row is the audit trail and what lets a restarted daemon re-publish an
// open view (the path is kept so the link it already sent keeps working).

// Browser handoff statuses. Open is the only non-final one.
const (
	HandoffOpen      = "open"
	HandoffDone      = "done"
	HandoffExpired   = "expired"
	HandoffCancelled = "cancelled"
	HandoffFailed    = "failed"
)

// Browser handoff modes.
const (
	HandoffModeHandoff = "handoff" // the person drives, taps Done, the agent resumes
	HandoffModeWatch   = "watch"   // the person watches, the agent keeps driving
)

// BrowserHandoff is one live view of an agent's browser session.
type BrowserHandoff struct {
	ID        int64
	Session   string
	Mode      string
	ChatID    int64
	ThreadID  int64 // the real (platform) thread, never a lane's synthetic one
	Reason    string
	Path      string // tailscale serve path, /h/<token>
	Link      string
	Status    string
	FinalURL  string
	EndedBy   string
	Note      string // what the person typed for the agent on Done
	CreatedAt time.Time
	ExpiresAt time.Time
	EndedAt   time.Time
}

const browserHandoffsSchema = `
CREATE TABLE IF NOT EXISTS browser_handoffs (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	session    TEXT NOT NULL,
	mode       TEXT NOT NULL,
	chat_id    INTEGER NOT NULL,
	thread_id  INTEGER NOT NULL DEFAULT 0,
	reason     TEXT NOT NULL DEFAULT '',
	path       TEXT NOT NULL UNIQUE,
	link       TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL DEFAULT 'open',
	final_url  TEXT NOT NULL DEFAULT '',
	ended_by   TEXT NOT NULL DEFAULT '',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	expires_at DATETIME NOT NULL,
	ended_at   DATETIME
);
CREATE INDEX IF NOT EXISTS idx_browser_handoffs_status ON browser_handoffs(status, id);
`

// AddBrowserHandoff records a new open handoff and returns its id.
func (s *Store) AddBrowserHandoff(h BrowserHandoff) (int64, error) {
	if h.Session == "" || h.Path == "" || h.ChatID == 0 || h.ExpiresAt.IsZero() {
		return 0, fmt.Errorf("browser handoff needs session, path, chat_id and expires_at")
	}
	if h.Mode != HandoffModeHandoff && h.Mode != HandoffModeWatch {
		return 0, fmt.Errorf("unknown browser handoff mode %q", h.Mode)
	}
	res, err := s.db.Exec(`INSERT INTO browser_handoffs (session, mode, chat_id, thread_id, reason, path, link, status, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.Session, h.Mode, h.ChatID, h.ThreadID, h.Reason, h.Path, h.Link, HandoffOpen, time.Now().UTC(), h.ExpiresAt.UTC())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetBrowserHandoffLink stores the published link of a handoff.
func (s *Store) SetBrowserHandoffLink(id int64, link string) error {
	_, err := s.db.Exec(`UPDATE browser_handoffs SET link = ? WHERE id = ?`, link, id)
	return err
}

// EndBrowserHandoff moves an open handoff to a final status. ended is false
// when it was already final — the caller lost a race (Done vs expiry) and must
// not act on it a second time. note is the person's note on Done, if any.
func (s *Store) EndBrowserHandoff(id int64, status, finalURL, endedBy, note string) (ended bool, err error) {
	switch status {
	case HandoffDone, HandoffExpired, HandoffCancelled, HandoffFailed:
	default:
		return false, fmt.Errorf("not a final browser handoff status: %q", status)
	}
	res, err := s.db.Exec(`UPDATE browser_handoffs SET status = ?, final_url = ?, ended_by = ?, note = ?, ended_at = ?
		WHERE id = ? AND status = ?`, status, finalURL, endedBy, note, time.Now().UTC(), id, HandoffOpen)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// GetBrowserHandoff returns one handoff, or nil if there is none with that id.
func (s *Store) GetBrowserHandoff(id int64) (*BrowserHandoff, error) {
	rows, err := s.queryBrowserHandoffs(`WHERE id = ?`, id)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// OpenBrowserHandoffs returns every handoff still open, oldest first.
func (s *Store) OpenBrowserHandoffs() ([]BrowserHandoff, error) {
	return s.queryBrowserHandoffs(`WHERE status = ? ORDER BY id`, HandoffOpen)
}

// RecentBrowserHandoffs returns the newest handoffs, newest first.
func (s *Store) RecentBrowserHandoffs(limit int) ([]BrowserHandoff, error) {
	if limit <= 0 {
		limit = 20
	}
	return s.queryBrowserHandoffs(`ORDER BY id DESC LIMIT ?`, limit)
}

func (s *Store) queryBrowserHandoffs(where string, args ...any) ([]BrowserHandoff, error) {
	rows, err := s.db.Query(`SELECT id, session, mode, chat_id, thread_id, reason, path, link, status,
		final_url, ended_by, note, created_at, expires_at, ended_at FROM browser_handoffs `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BrowserHandoff
	for rows.Next() {
		var h BrowserHandoff
		var created, expires, ended any
		if err := rows.Scan(&h.ID, &h.Session, &h.Mode, &h.ChatID, &h.ThreadID, &h.Reason, &h.Path, &h.Link,
			&h.Status, &h.FinalURL, &h.EndedBy, &h.Note, &created, &expires, &ended); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			return nil, err
		}
		h.CreatedAt, h.ExpiresAt, h.EndedAt = asTime(created), asTime(expires), asTime(ended)
		out = append(out, h)
	}
	return out, rows.Err()
}
