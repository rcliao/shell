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
	ID               string
	ChatID           int64
	ClaudeSessionID  string
	Status           SessionStatus
	HasHistory       bool // true after first successful exchange
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func NewSession(chatID int64) *Session {
	return &Session{
		ID:               generateID(),
		ChatID:           chatID,
		ClaudeSessionID:  generateID(),
		Status:           StatusActive,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
}

func generateID() string {
	return uuid.New().String()
}
