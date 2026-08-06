package daemon

import (
	"context"
	"log/slog"
)

// Outbound delivery, abstracted away from Telegram.
//
// The daemon required a Telegram bot token to start at all: NewBot rejects an
// empty token, so an agent with no Telegram could not exist. That made the
// "transport-agnostic intake" claim only half true — messages could arrive from
// anywhere, but the process still could not run without the one transport it
// was supposed to be independent of.
//
// Fifteen call sites sent through the bot directly. Nil-guarding each would
// have worked and taught nothing; naming the capability they actually depend on
// is the same amount of code and leaves a seam a TUI or a test harness can fill.
type outbound interface {
	SendText(chatID, threadID int64, text string)
	SendPhoto(chatID, threadID int64, data []byte, caption string)
	SendVideo(chatID, threadID int64, data []byte, caption string)
	SetOutboundDedup(check func(chatID, threadID int64, text string) bool)
	// Start runs the inbound poller. A transport with no inbound side blocks
	// until the context is cancelled, matching the bot's lifecycle contract.
	Start(ctx context.Context)
}

// headlessOutbound is the no-Telegram implementation: an agent reachable only
// through the CLI transport (and later a TUI).
//
// Sends are LOGGED rather than silently dropped. A proactive message with
// nowhere to go is a real event — a scheduled reminder firing into the void —
// and a test agent that hides them would make the queue look healthier than it
// is.
type headlessOutbound struct{}

func (headlessOutbound) SendText(chatID, threadID int64, text string) {
	slog.Info("headless: outbound text dropped (no transport attached)",
		"chat_id", chatID, "thread_id", threadID, "chars", len(text))
}

func (headlessOutbound) SendPhoto(chatID, threadID int64, data []byte, caption string) {
	slog.Info("headless: outbound photo dropped (no transport attached)",
		"chat_id", chatID, "thread_id", threadID, "bytes", len(data))
}

func (headlessOutbound) SendVideo(chatID, threadID int64, data []byte, caption string) {
	slog.Info("headless: outbound video dropped (no transport attached)",
		"chat_id", chatID, "thread_id", threadID, "bytes", len(data))
}

func (headlessOutbound) SetOutboundDedup(func(chatID, threadID int64, text string) bool) {}

// Start blocks until cancelled: there is no inbound poller, but the daemon's
// run loop expects this call to own the process's lifetime.
func (headlessOutbound) Start(ctx context.Context) { <-ctx.Done() }
