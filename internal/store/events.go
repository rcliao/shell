package store

import (
	"fmt"
	"strings"
	"time"
)

// External events (docs/DESIGN-HEARTBEAT-AGENDA-EVENTS.md): something that
// happened outside the chats, normalized and deduplicated, waiting for the
// agent's next heartbeat agenda. Only a one-line summary and a reference are
// kept; the agent fetches detail itself.

// Event statuses.
const (
	EventNew     = "new"
	EventSeen    = "seen"
	EventDone    = "done"
	EventIgnored = "ignored"
)

// Event is one external event.
type Event struct {
	ID         int64
	Source     string
	Kind       string
	DedupID    string
	ChatID     int64
	Summary    string
	Ref        string
	OccurredAt time.Time
	CreatedAt  time.Time
	Status     string
	Note       string
}

const eventsSchema = `
CREATE TABLE IF NOT EXISTS events (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	source      TEXT NOT NULL,
	kind        TEXT NOT NULL,
	dedup_id    TEXT NOT NULL,
	chat_id     INTEGER NOT NULL DEFAULT 0,
	summary     TEXT NOT NULL,
	ref         TEXT NOT NULL DEFAULT '',
	occurred_at DATETIME,
	created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	status      TEXT NOT NULL DEFAULT 'new',
	note        TEXT NOT NULL DEFAULT '',
	UNIQUE(source, dedup_id)
);
CREATE INDEX IF NOT EXISTS idx_events_status ON events(status, id);
`

// AddEvent records an event. A duplicate (same source and dedup id) is
// ignored: created is false.
func (s *Store) AddEvent(e Event) (id int64, created bool, err error) {
	if strings.TrimSpace(e.Source) == "" || strings.TrimSpace(e.Kind) == "" || strings.TrimSpace(e.DedupID) == "" || strings.TrimSpace(e.Summary) == "" {
		return 0, false, fmt.Errorf("event needs source, kind, dedup_id and summary")
	}
	var occurred any
	if !e.OccurredAt.IsZero() {
		occurred = e.OccurredAt.UTC()
	}
	res, err := s.db.Exec(`INSERT OR IGNORE INTO events (source, kind, dedup_id, chat_id, summary, ref, occurred_at, created_at, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, e.Source, e.Kind, e.DedupID, e.ChatID, e.Summary, e.Ref, occurred, time.Now().UTC(), EventNew)
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, false, nil
	}
	id, err = res.LastInsertId()
	return id, true, err
}

// ListEvents returns events with the given statuses (empty = all), newest first.
func (s *Store) ListEvents(statuses []string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, source, kind, dedup_id, chat_id, summary, ref, occurred_at, created_at, status, note FROM events`
	var args []any
	if len(statuses) > 0 {
		q += ` WHERE status IN (?` + strings.Repeat(",?", len(statuses)-1) + `)`
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var occurred, created any
		if err := rows.Scan(&e.ID, &e.Source, &e.Kind, &e.DedupID, &e.ChatID, &e.Summary, &e.Ref, &occurred, &created, &e.Status, &e.Note); err != nil {
			return nil, err
		}
		e.OccurredAt = asTime(occurred)
		e.CreatedAt = asTime(created)
		if e.OccurredAt.IsZero() {
			e.OccurredAt = e.CreatedAt
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkEvent sets an event's status (seen, done, ignored) with an optional note.
func (s *Store) MarkEvent(id int64, status, note string) error {
	switch status {
	case EventSeen, EventDone, EventIgnored:
	default:
		return fmt.Errorf("unknown event status %q (seen, done, ignored)", status)
	}
	res, err := s.db.Exec(`UPDATE events SET status = ?, note = CASE WHEN ? = '' THEN note ELSE ? END WHERE id = ?`, status, note, note, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no event #%d", id)
	}
	return nil
}

// MarkEventSeen marks an event seen only if it is still new: the agent may
// have closed it (done/ignored) during the very beat that showed it, and
// that decision must not be overwritten.
func (s *Store) MarkEventSeen(id int64) error {
	_, err := s.db.Exec(`UPDATE events SET status = ? WHERE id = ? AND status = ?`, EventSeen, id, EventNew)
	return err
}

func asTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		for _, layout := range []string{"2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05", time.RFC3339Nano} {
			if p, err := time.Parse(layout, t); err == nil {
				return p
			}
		}
	}
	return time.Time{}
}

// PausedSchedule is an auto-paused schedule, for the heartbeat agenda.
type PausedSchedule struct {
	ID     int64
	Label  string
	Reason string
}

// AutoPausedSchedulesSince returns schedules the scheduler paused on its own
// (paused_reason set) that were touched since a cutoff: the agent's own
// schedules that need repair, not ones someone deliberately cancelled.
func (s *Store) AutoPausedSchedulesSince(since time.Time) ([]PausedSchedule, error) {
	rows, err := s.db.Query(`SELECT id, label, paused_reason FROM schedules
		WHERE enabled = 0 AND paused_reason != ''
		  AND (last_run_at >= ? OR created_at >= ?) ORDER BY id`, since.UTC(), since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PausedSchedule
	for rows.Next() {
		var p PausedSchedule
		if err := rows.Scan(&p.ID, &p.Label, &p.Reason); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FailingTools returns tools that failed at least min times since a cutoff.
func (s *Store) FailingTools(since time.Time, min int) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT tool_name, COUNT(*) FROM tool_uses WHERE failed = 1 AND created_at >= ?
		GROUP BY tool_name HAVING COUNT(*) >= ?`, since.UTC(), min)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}
