package process

import (
	"time"

	"github.com/google/uuid"
)

type SessionStatus string

const (
	StatusActive SessionStatus = "active"
	StatusBusy   SessionStatus = "busy"
	StatusClosed SessionStatus = "closed"
)

// SessionKey identifies a session by (chat_id, message_thread_id).
// Forum topics within a group each get their own session so context does
// not bleed across topics. message_thread_id == 0 means the main chat.
type SessionKey struct {
	ChatID   int64
	ThreadID int64
}

// Key returns the SessionKey for this session.
func (s *Session) Key() SessionKey {
	return SessionKey{ChatID: s.ChatID, ThreadID: s.MessageThreadID}
}

type Session struct {
	ID                string
	ChatID            int64
	MessageThreadID   int64 // Telegram forum topic ID (0 = main chat)
	ProviderSessionID string
	Status            SessionStatus
	Compacting        bool // true while session is being compacted (messages should wait)
	HasHistory        bool // true after first successful exchange
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func NewSession(chatID, threadID int64) *Session {
	return &Session{
		ID:                generateID(),
		ChatID:            chatID,
		MessageThreadID:   threadID,
		ProviderSessionID: generateID(),
		Status:            StatusActive,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
}

func generateID() string {
	return uuid.New().String()
}
