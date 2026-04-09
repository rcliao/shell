package process

import "context"

// ImageAttachment represents an image file to include in the message.
type ImageAttachment struct {
	Path   string // local file path
	Width  int    // image width in pixels (0 if unknown)
	Height int    // image height in pixels (0 if unknown)
	Size   int64  // file size in bytes (0 if unknown)
}

// PDFAttachment represents a PDF file to include in the message.
type PDFAttachment struct {
	Path string // local file path
	Size int64  // file size in bytes (0 if unknown)
}

// AgentRequest is the typed message from bridge → process.
type AgentRequest struct {
	ChatID       int64
	SessionID    string            // claude session ID for --resume
	Text         string            // user message text
	Images       []ImageAttachment // attached images
	PDFs         []PDFAttachment   // attached PDFs
	SystemPrompt string            // appended system prompt
	Model        string            // per-request model override (empty = use manager default)
}

// Agent abstracts the Claude session manager so the implementation can be
// swapped (e.g. Claude CLI, HTTP API, mock for testing).
type Agent interface {
	// Send sends a prompt and streams text deltas via onUpdate (nil for no streaming).
	Send(ctx context.Context, req AgentRequest, onUpdate StreamFunc) (SendResult, error)

	// Get returns the session for a chat ID.
	Get(chatID int64) (*Session, bool)

	// Register adds or updates a session.
	Register(sess *Session)

	// SetCompacting marks whether a session is being compacted.
	// When compacting, incoming messages wait instead of getting "busy".
	SetCompacting(chatID int64, compacting bool)

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
