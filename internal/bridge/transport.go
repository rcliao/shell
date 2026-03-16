package bridge

// Transport abstracts the delivery of messages and media to users.
// The bridge calls Transport to send output; it never imports a transport package.
type Transport interface {
	// Notify sends a one-way text message to a chat (plan progress, async notifications).
	Notify(chatID int64, msg string)

	// SendPhoto sends an image to a chat.
	SendPhoto(chatID int64, data []byte, caption string)
}
