package store

import (
	"database/sql"
	"time"
)

// Conversation is the per-chat sticky pointer to the "currently active"
// topic thread. Cycle 145 experiment: instead of classifying every turn
// synchronously, follow the previous turn's thread by default. Drift
// detection moves the pointer; the foreground just dereferences it.
//
// This cycle is observability-only — the existing classifier still runs
// and we just RECORD which thread it chose into current_thread_id. After
// 7d of data we'll compare: how often would "deref previous pointer" have
// matched the classifier's choice? If high (which the inertia hypothesis
// predicts), the sync classifier can move off the user-response path.
type Conversation struct {
	ChatID            int64
	CurrentThreadID   sql.NullInt64
	CurrentTopic      string
	LastDriftCheckAt  *time.Time
	TurnsSinceCheck   int
	ColdStart         bool
	UpdatedAt         time.Time
}

// GetConversation returns the sticky-pointer row for a chat, or nil if no
// row exists yet (cold start).
func (s *Store) GetConversation(chatID int64) (*Conversation, error) {
	row := s.db.QueryRow(`
		SELECT chat_id, current_thread_id, current_topic,
		       last_drift_check_at, turns_since_check, cold_start, updated_at
		FROM conversations
		WHERE chat_id = ?`, chatID)
	var c Conversation
	var cold int
	if err := row.Scan(&c.ChatID, &c.CurrentThreadID, &c.CurrentTopic,
		&c.LastDriftCheckAt, &c.TurnsSinceCheck, &cold, &c.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	c.ColdStart = cold != 0
	return &c, nil
}

// UpsertConversation records the current sticky thread for a chat after a
// classifier run. Cycle 145: cold_start flips to 0 once we've had any
// non-empty topic. turns_since_check increments per call; reset to 0
// when current_thread_id changes (indicating a drift event we now want
// to recheck against).
func (s *Store) UpsertConversation(chatID int64, threadID int64, topic string) error {
	// Read prior state so we can detect whether this is a drift.
	prior, err := s.GetConversation(chatID)
	if err != nil {
		return err
	}

	var newTurns int
	if prior != nil && prior.CurrentThreadID.Valid && prior.CurrentThreadID.Int64 == threadID {
		// Same thread continuing — increment the counter.
		newTurns = prior.TurnsSinceCheck + 1
	} else {
		// First time, or drift — reset counter.
		newTurns = 0
	}

	_, err = s.db.Exec(`
		INSERT INTO conversations
		  (chat_id, current_thread_id, current_topic,
		   turns_since_check, cold_start, updated_at)
		VALUES (?, ?, ?, ?, 0, CURRENT_TIMESTAMP)
		ON CONFLICT(chat_id) DO UPDATE SET
		  current_thread_id = excluded.current_thread_id,
		  current_topic     = excluded.current_topic,
		  turns_since_check = excluded.turns_since_check,
		  cold_start        = 0,
		  updated_at        = CURRENT_TIMESTAMP`,
		chatID, threadID, topic, newTurns)
	return err
}

// StickyMatched reports whether the prior sticky pointer (if any) would
// have produced the same thread as the classifier chose this turn. The
// audit signal for cycle 145: high matched-rate validates the inertia
// hypothesis and justifies removing the sync classifier from the user
// path.
func (s *Store) StickyMatched(chatID int64, classifierThreadID int64) (matched bool, hadPrior bool, err error) {
	prior, err := s.GetConversation(chatID)
	if err != nil {
		return false, false, err
	}
	if prior == nil || !prior.CurrentThreadID.Valid {
		return false, false, nil
	}
	return prior.CurrentThreadID.Int64 == classifierThreadID, true, nil
}
