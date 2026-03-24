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

type Session struct {
	ID                string
	ChatID            int64
	ProviderSessionID string
	Status            SessionStatus
	Compacting        bool // true while session is being compacted (messages should wait)
	HasHistory        bool // true after first successful exchange
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func NewSession(chatID int64) *Session {
	return &Session{
		ID:               generateID(),
		ChatID:           chatID,
		ProviderSessionID:  generateID(),
		Status:           StatusActive,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
}

func generateID() string {
	return uuid.New().String()
}
