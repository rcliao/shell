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

	// SendVideo sends a video to a chat.
	SendVideo(chatID, threadID int64, data []byte, caption string)

	// SendDocument sends a file (by path) to a chat as a document attachment.
	SendDocument(chatID, threadID int64, path, caption string) error

	// SendMessageID sends a text message and returns its message ID, so the
	// caller can pin or edit it later. Notify deliberately returns nothing;
	// the pinned 📋 Projects message needs the id back.
	SendMessageID(chatID, threadID int64, text string) (int, error)

	// EditMessage replaces the text of a previously sent message in place.
	EditMessage(chatID int64, messageID int, text string) error

	// PinMessage pins a message in a chat; silent suppresses the pin
	// notification.
	PinMessage(chatID int64, messageID int, silent bool) error

	// UnpinMessage unpins a previously pinned message.
	UnpinMessage(chatID int64, messageID int) error

	// NotifyButtons is Notify plus inline URL buttons under the message. It
	// returns an error (unlike Notify) because its callers decide whether a
	// delivery with a doc link actually landed. An empty buttons slice
	// behaves exactly like Notify.
	NotifyButtons(chatID, threadID int64, text string, buttons []LinkButton) error

	// SendMessageIDButtons is SendMessageID plus inline URL buttons.
	SendMessageIDButtons(chatID, threadID int64, text string, buttons []LinkButton) (int, error)

	// EditMessageButtons is EditMessage plus inline URL buttons; the buttons
	// replace whatever keyboard the message previously carried.
	EditMessageButtons(chatID int64, messageID int, text string, buttons []LinkButton) error
}

// LinkButton is one inline URL button under a message: a label the user taps
// and the URL it opens. URL buttons only — no callback buttons, so no update
// plumbing is needed on the receive side.
type LinkButton struct {
	Label string
	URL   string
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
