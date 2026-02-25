package process

import (
	"crypto/rand"
	"fmt"
	"io"
	"time"
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
	b := make([]byte, 16)
	io.ReadFull(rand.Reader, b)
	return fmt.Sprintf("%x", b)
}
