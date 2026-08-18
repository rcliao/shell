package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rcliao/shell/internal/bridge"
	"github.com/rcliao/shell/internal/config"
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
	SendDocument(chatID, threadID int64, path, caption string) error
	// SendMessageID sends a text message and returns its id for later
	// pin/edit; EditMessage/PinMessage/UnpinMessage operate on that id.
	// These return errors (unlike the fire-and-forget sends) because their
	// callers hold state — a pinned message id — that must not be updated
	// from a send that never happened.
	SendMessageID(chatID, threadID int64, text string) (int, error)
	EditMessage(chatID int64, messageID int, text string) error
	// Button variants: same sends with inline URL buttons attached (project
	// doc links). SendTextButtons returns an error unlike SendText because
	// its callers report whether a linked delivery landed.
	SendTextButtons(chatID, threadID int64, text string, buttons []bridge.LinkButton) error
	SendMessageIDButtons(chatID, threadID int64, text string, buttons []bridge.LinkButton) (int, error)
	EditMessageButtons(chatID int64, messageID int, text string, buttons []bridge.LinkButton) error
	PinMessage(chatID int64, messageID int, silent bool) error
	UnpinMessage(chatID int64, messageID int) error
	SetOutboundDedup(check func(chatID, threadID int64, text string) bool)
	// Start runs the inbound poller. A transport with no inbound side blocks
	// until the context is cancelled, matching the bot's lifecycle contract.
	Start(ctx context.Context)
}

// errNoTransport is what the request/response-shaped outbound methods return
// when no transport is attached.
var errNoTransport = errors.New("no transport attached")

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

// The id-returning and id-consuming methods error instead of logging: their
// callers keep state (a pinned message id) that a fabricated success would
// poison.
func (headlessOutbound) SendDocument(chatID, threadID int64, path, caption string) error {
	slog.Info("headless: outbound document dropped (no transport attached)",
		"chat_id", chatID, "thread_id", threadID, "path", path)
	return errNoTransport
}

func (headlessOutbound) SendMessageID(chatID, threadID int64, text string) (int, error) {
	slog.Info("headless: outbound message dropped (no transport attached)",
		"chat_id", chatID, "thread_id", threadID, "chars", len(text))
	return 0, errNoTransport
}

func (headlessOutbound) EditMessage(chatID int64, messageID int, text string) error {
	return errNoTransport
}

func (headlessOutbound) SendTextButtons(chatID, threadID int64, text string, buttons []bridge.LinkButton) error {
	slog.Info("headless: outbound text dropped (no transport attached)",
		"chat_id", chatID, "thread_id", threadID, "chars", len(text), "buttons", len(buttons))
	return errNoTransport
}

func (headlessOutbound) SendMessageIDButtons(chatID, threadID int64, text string, buttons []bridge.LinkButton) (int, error) {
	slog.Info("headless: outbound message dropped (no transport attached)",
		"chat_id", chatID, "thread_id", threadID, "chars", len(text), "buttons", len(buttons))
	return 0, errNoTransport
}

func (headlessOutbound) EditMessageButtons(chatID int64, messageID int, text string, buttons []bridge.LinkButton) error {
	return errNoTransport
}

func (headlessOutbound) PinMessage(chatID int64, messageID int, silent bool) error {
	return errNoTransport
}

func (headlessOutbound) UnpinMessage(chatID int64, messageID int) error {
	return errNoTransport
}

func (headlessOutbound) SetOutboundDedup(func(chatID, threadID int64, text string) bool) {}

// Start blocks until cancelled: there is no inbound poller, but the daemon's
// run loop expects this call to own the process's lifetime.
func (headlessOutbound) Start(ctx context.Context) { <-ctx.Done() }

// tokenOwnedByAnotherAgent reports whether another agent's config on this
// machine already claims the same Telegram token.
//
// The check is by TOKEN VALUE, not by env-var name: the failure that motivated
// it was an agent inheriting the DEFAULT env name and therefore a different
// agent's token, so comparing names would have missed it entirely.
func tokenOwnedByAnotherAgent(cfg config.Config, token string) (string, bool) {
	agentsDir := filepath.Join(config.DefaultConfigDir(), "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return "", false // no agents dir: nothing to clash with
	}
	self := filepath.Dir(cfg.Daemon.PIDFile)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(agentsDir, e.Name())
		if dir == self {
			continue
		}
		other, err := config.Load(filepath.Join(dir, "config.json"))
		if err != nil {
			continue // a broken sibling config is not this daemon's problem
		}
		if other.TelegramToken() == token {
			return e.Name(), true
		}
	}
	return "", false
}
