package store

import "time"

// ReplyPair is one person's message in a thread and the agent's reply to it.
type ReplyPair struct {
	At    time.Time
	User  string
	Reply string
}

// ReplyPairsInThread returns people's messages in a thread's session since
// a time, each with the agent reply that followed it (for the project
// context eval). Synthetic prompts ("[" …) are skipped, and so are replies
// that were only [noop].
func (s *Store) ReplyPairsInThread(chatID, threadID int64, since time.Time) ([]ReplyPair, error) {
	rows, err := s.db.Query(`
		SELECT m.role, m.content, m.created_at FROM messages m JOIN sessions se ON se.id = m.session_id
		WHERE se.chat_id = ? AND se.message_thread_id = ? AND datetime(m.created_at) >= datetime(?)
		ORDER BY m.id`, chatID, threadID, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReplyPair
	var pending *ReplyPair
	for rows.Next() {
		var role, content string
		var at time.Time
		if err := rows.Scan(&role, &content, &at); err != nil {
			return nil, err
		}
		switch {
		case role == "user" && len(content) > 0 && content[0] != '[':
			pending = &ReplyPair{At: at, User: content}
		case role == "user":
			pending = nil
		case role == "assistant" && pending != nil && content != "" && content != "[noop]":
			pending.Reply = content
			out = append(out, *pending)
			pending = nil
		}
	}
	return out, rows.Err()
}
