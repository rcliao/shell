package bridge

import "github.com/rcliao/shell/internal/process"

// Transport abstracts the delivery of messages and media to users.
// The bridge calls Transport to send output; it never imports a transport package.
//
// threadID is the Telegram forum topic ID (0 = main chat / no topic).
type Transport interface {
	// Notify sends a one-way text message to a chat (plan progress, async notifications).
	Notify(chatID, threadID int64, msg string)

	// SendPhoto sends an image to a chat.
	SendPhoto(chatID, threadID int64, data []byte, caption string)
}

// AgentPool resolves which Agent handles a given chat.
// When set on the bridge, multi-agent routing is enabled.
type AgentPool interface {
	// Resolve returns the Agent for a chatID.
	Resolve(chatID int64) process.Agent

	// Route binds a chatID to a named agent. Returns false if agent not found.
	Route(chatID int64, agentName string) bool

	// AgentNames returns all registered agent names.
	AgentNames() []string

	// CurrentAgent returns the agent name for a chatID.
	CurrentAgent(chatID int64) string
}
