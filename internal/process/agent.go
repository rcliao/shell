package process

import "context"

// Agent abstracts the Claude session manager so the implementation can be
// swapped (e.g. Claude CLI, HTTP API, mock for testing).
type Agent interface {
	// Send sends a prompt and returns the full response.
	Send(ctx context.Context, chatID int64, claudeSessionID, message, systemPrompt string) (SendResult, error)

	// SendStreaming sends a prompt and streams text deltas via onUpdate.
	SendStreaming(ctx context.Context, chatID int64, claudeSessionID, message, systemPrompt string, onUpdate StreamFunc) (SendResult, error)

	// Get returns the session for a chat ID.
	Get(chatID int64) (*Session, bool)

	// Register adds or updates a session.
	Register(sess *Session)

	// Kill terminates a session and removes it.
	Kill(chatID int64)

	// KillAll terminates all sessions.
	KillAll()

	// ActiveCount returns the number of tracked sessions.
	ActiveCount() int

	// ListSessions returns all tracked sessions.
	ListSessions() []Session
}

// Ensure Manager satisfies Agent at compile time.
var _ Agent = (*Manager)(nil)
