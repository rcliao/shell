package store

import (
	"time"
)

// Inbound media ledger (V2-H19 vision memory).
//
// Every photo a user sends is archived to a persistent path and recorded
// here, so 「剛剛那張照片」 keeps working across turns, rotations, and days —
// the old temp-file flow expired mid-conversation (7/11: 「暫存檔案也已經
// 過期了，讀不到了」). Description is filled in after the answering turn via
// the [media-note] marker, making photos text-searchable.

// MediaRow is one archived inbound photo.
type MediaRow struct {
	ID          int64
	ChatID      int64
	ThreadID    int64
	MsgID       int
	Path        string
	Caption     string
	Description string
	CreatedAt   time.Time
}

// RecordMedia ledgers an archived inbound photo and returns its row id.
func (s *Store) RecordMedia(chatID, threadID int64, msgID int, path, caption string) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO media (chat_id, thread_id, msg_id, path, caption)
		VALUES (?, ?, ?, ?, ?)`,
		chatID, threadID, msgID, path, caption)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetMediaDescription stores the agent's own one-line understanding of the
// photo (from the [media-note] marker).
func (s *Store) SetMediaDescription(id int64, desc string) error {
	_, err := s.db.Exec(`UPDATE media SET description = ? WHERE id = ?`, desc, id)
	return err
}

// RecentMedia returns the newest archived photos for a chat, newest first.
func (s *Store) RecentMedia(chatID, threadID int64, limit int) ([]MediaRow, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, thread_id, msg_id, path, caption, description, created_at
		FROM media WHERE chat_id = ? AND thread_id = ?
		ORDER BY id DESC LIMIT ?`,
		chatID, threadID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaRow
	for rows.Next() {
		var r MediaRow
		if err := rows.Scan(&r.ID, &r.ChatID, &r.ThreadID, &r.MsgID, &r.Path, &r.Caption, &r.Description, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
