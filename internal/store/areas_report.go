package store

import "time"

// Focus measures for project areas (docs/DESIGN-PROJECT-AREAS.md): is the
// work happening in the projects' own places? Read-only.

// HumanMessagesInThread counts people's messages in one thread's session
// since a time. Synthetic prompts (heartbeats, schedules, peer relays) start
// with "[" and are not counted.
func (s *Store) HumanMessagesInThread(chatID, threadID int64, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM messages m JOIN sessions se ON se.id = m.session_id
		WHERE se.chat_id = ? AND se.message_thread_id = ? AND m.role = 'user'
		  AND substr(m.content, 1, 1) != '['
		  AND datetime(m.created_at) >= datetime(?)`,
		chatID, threadID, since.UTC()).Scan(&n)
	return n, err
}

// LaneRoutedElsewhere counts messages the lane router sent to a project's
// lane from anywhere other than the project's own thread since a time: the
// project talked about outside its post.
func (s *Store) LaneRoutedElsewhere(chatID int64, lane string, ownThread int64, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM route_decisions
		WHERE source = 'lane' AND chat_id = ? AND lane = ? AND thread_id != ?
		  AND datetime(created_at) >= datetime(?)`,
		chatID, lane, ownThread, since.UTC()).Scan(&n)
	return n, err
}

// LastHumanTouch is when a person last engaged with a project: the latest of
// a Notion edit (LastHumanActivityAt), a person's message in the project's
// own thread, and a message the lane router sent to it from elsewhere. The
// field alone is written only by the Notion poll, so on its own it calls a
// busy project idle. nil when none of them ever happened.
func (s *Store) LastHumanTouch(p Project) *time.Time {
	latest := p.LastHumanActivityAt
	consider := func(t time.Time) {
		if !t.IsZero() && (latest == nil || t.After(*latest)) {
			tt := t
			latest = &tt
		}
	}
	// ORDER BY + LIMIT, not MAX(): MAX loses the DATETIME type on scan.
	if p.MessageThreadID != 0 {
		var t time.Time
		if err := s.db.QueryRow(`
			SELECT m.created_at FROM messages m JOIN sessions se ON se.id = m.session_id
			WHERE se.chat_id = ? AND se.message_thread_id = ? AND m.role = 'user'
			  AND substr(m.content, 1, 1) != '['
			ORDER BY m.id DESC LIMIT 1`, p.ChatID, p.MessageThreadID).Scan(&t); err == nil {
			consider(t)
		}
	}
	var t time.Time
	if err := s.db.QueryRow(`
		SELECT created_at FROM route_decisions
		WHERE source = 'lane' AND chat_id = ? AND lane = ?
		ORDER BY id DESC LIMIT 1`, p.ChatID, p.Slug).Scan(&t); err == nil {
		consider(t)
	}
	return latest
}
