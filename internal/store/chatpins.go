package store

import (
	"database/sql"
	"time"
)

// chat_pins tracks the pinned 📋 Projects home message per chat (P2, docs/
// PLAN-PROJECT-WORKSPACE.md "Project home"). One row per chat: the Telegram
// message id the home renderer edits in place. The row is bookkeeping, not
// truth — if the message was deleted out from under us, the renderer sends a
// fresh one and overwrites the row.

// ChatPin is the pinned projects-home message for one chat.
type ChatPin struct {
	ChatID        int64
	ProjectsMsgID int
	UpdatedAt     time.Time
}

// GetChatPin returns the chat's pinned projects-home message, or nil when the
// chat has never had one.
func (s *Store) GetChatPin(chatID int64) (*ChatPin, error) {
	var p ChatPin
	err := s.db.QueryRow(`
		SELECT chat_id, projects_msg_id, updated_at FROM chat_pins WHERE chat_id = ?
	`, chatID).Scan(&p.ChatID, &p.ProjectsMsgID, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SetChatPin records (or replaces) the chat's pinned projects-home message id.
func (s *Store) SetChatPin(chatID int64, msgID int) error {
	_, err := s.db.Exec(`
		INSERT INTO chat_pins (chat_id, projects_msg_id, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(chat_id) DO UPDATE SET
			projects_msg_id = excluded.projects_msg_id,
			updated_at = CURRENT_TIMESTAMP
	`, chatID, msgID)
	return err
}
