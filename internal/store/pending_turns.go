package store

import (
	"database/sql"
	"fmt"
	"time"
)

// PendingTurn tracks an inbound user message from receipt to answered.
// Purpose (V2-H30 replay): a deploy restart can kill a turn after Telegram
// has acked the message — the user sees a placeholder stuck on "Thinking..."
// forever (observed twice on 7/14). Rows with done=0 at startup are replayed.
// The (chat_id, telegram_msg_id) key also dedupes Telegram redeliveries of
// already-answered messages.
type PendingTurn struct {
	ChatID        int64
	ThreadID      int64
	TelegramMsgID int
	SenderName    string
	Text          string
	CreatedAt     time.Time
}

func (s *Store) migratePendingTurns() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS pending_turns (
			chat_id         INTEGER NOT NULL,
			thread_id       INTEGER NOT NULL DEFAULT 0,
			telegram_msg_id INTEGER NOT NULL,
			sender_name     TEXT NOT NULL DEFAULT '',
			text            TEXT NOT NULL DEFAULT '',
			done            INTEGER NOT NULL DEFAULT 0,
			created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (chat_id, telegram_msg_id)
		)`)
	return err
}

// BeginPendingTurn records an inbound message before processing. Returns
// alreadyDone=true when this exact message was answered before (a Telegram
// redelivery) so the caller can skip it.
func (s *Store) BeginPendingTurn(chatID, threadID int64, telegramMsgID int, senderName, text string) (alreadyDone bool, err error) {
	var done int
	err = s.db.QueryRow(`SELECT done FROM pending_turns WHERE chat_id = ? AND telegram_msg_id = ?`,
		chatID, telegramMsgID).Scan(&done)
	if err == nil {
		return done == 1, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	_, err = s.db.Exec(`INSERT OR IGNORE INTO pending_turns (chat_id, thread_id, telegram_msg_id, sender_name, text) VALUES (?, ?, ?, ?, ?)`,
		chatID, threadID, telegramMsgID, senderName, text)
	return false, err
}

// CompletePendingTurn marks a message answered and prunes old completed rows.
func (s *Store) CompletePendingTurn(chatID int64, telegramMsgID int) error {
	_, err := s.db.Exec(`UPDATE pending_turns SET done = 1 WHERE chat_id = ? AND telegram_msg_id = ?`, chatID, telegramMsgID)
	s.db.Exec(`DELETE FROM pending_turns WHERE done = 1 AND created_at < datetime('now', '-2 days')`)
	return err
}

// ListUnfinishedTurns returns unanswered messages newer than maxAge,
// oldest first — the startup replay queue.
func (s *Store) ListUnfinishedTurns(maxAge time.Duration) ([]PendingTurn, error) {
	rows, err := s.db.Query(`
		SELECT chat_id, thread_id, telegram_msg_id, sender_name, text, created_at
		FROM pending_turns
		WHERE done = 0 AND created_at >= datetime('now', ?)
		ORDER BY created_at ASC`,
		fmt.Sprintf("-%d seconds", int(maxAge.Seconds())))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingTurn
	for rows.Next() {
		var t PendingTurn
		if err := rows.Scan(&t.ChatID, &t.ThreadID, &t.TelegramMsgID, &t.SenderName, &t.Text, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
