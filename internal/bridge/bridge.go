package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rcliao/teeny-relay/internal/process"
	"github.com/rcliao/teeny-relay/internal/store"
)

type Bridge struct {
	proc  *process.Manager
	store *store.Store
}

func New(proc *process.Manager, store *store.Store) *Bridge {
	return &Bridge{proc: proc, store: store}
}

// HandleMessage processes an incoming user message and returns Claude's response.
func (b *Bridge) HandleMessage(ctx context.Context, chatID int64, userMsg string) (string, error) {
	sess, err := b.ensureSession(ctx, chatID)
	if err != nil {
		return "", fmt.Errorf("ensure session: %w", err)
	}

	// Log user message
	if err := b.store.LogMessage(sess.ID, "user", userMsg); err != nil {
		slog.Warn("failed to log user message", "error", err)
	}

	// Send to Claude
	response, err := b.proc.Send(ctx, chatID, sess.ClaudeSessionID, userMsg)
	if err != nil {
		return "", fmt.Errorf("claude: %w", err)
	}

	response = strings.TrimSpace(response)

	// Log assistant response
	if err := b.store.LogMessage(sess.ID, "assistant", response); err != nil {
		slog.Warn("failed to log assistant message", "error", err)
	}

	// Update session timestamp
	if err := b.store.UpdateSessionStatus(chatID, "active"); err != nil {
		slog.Warn("failed to update session", "error", err)
	}

	return response, nil
}

// HandleCommand processes a bot command.
func (b *Bridge) HandleCommand(ctx context.Context, chatID int64, cmd, args string) (string, error) {
	switch cmd {
	case "new":
		return b.Reset(ctx, chatID)
	case "status":
		return b.Status(chatID)
	case "help":
		return b.Help(), nil
	case "start":
		return b.Start(ctx, chatID)
	default:
		return fmt.Sprintf("Unknown command: /%s", cmd), nil
	}
}

// Start handles the /start command.
func (b *Bridge) Start(ctx context.Context, chatID int64) (string, error) {
	_, err := b.ensureSession(ctx, chatID)
	if err != nil {
		return "", err
	}
	return "Welcome to teeny-relay! Send me a message and I'll forward it to Claude Code.\n\nCommands:\n/new — Start a fresh session\n/status — Show session info\n/help — Show help", nil
}

// Reset kills the current session and creates a fresh one.
func (b *Bridge) Reset(ctx context.Context, chatID int64) (string, error) {
	b.proc.Kill(chatID)
	if err := b.store.DeleteSession(chatID); err != nil {
		slog.Warn("failed to delete session from store", "error", err)
	}

	_, err := b.ensureSession(ctx, chatID)
	if err != nil {
		return "", err
	}
	return "Session reset. Starting fresh conversation.", nil
}

// Status returns info about the current session.
func (b *Bridge) Status(chatID int64) (string, error) {
	sess, err := b.store.GetSession(chatID)
	if err != nil {
		return "", err
	}
	if sess == nil {
		return "No active session. Send a message to start one.", nil
	}

	msgs, err := b.store.GetMessages(sess.ID, 0)
	if err != nil {
		return "", err
	}

	procSess, _ := b.proc.Get(chatID)
	status := "active"
	if procSess != nil {
		status = string(procSess.Status)
	}

	return fmt.Sprintf(
		"Session: %s\nStatus: %s\nMessages: %d\nCreated: %s\nLast active: %s",
		sess.ClaudeSessionID[:12]+"...",
		status,
		len(msgs),
		sess.CreatedAt.Format("2006-01-02 15:04:05"),
		sess.UpdatedAt.Format("2006-01-02 15:04:05"),
	), nil
}

func (b *Bridge) Help() string {
	return "teeny-relay — Telegram ↔ Claude Code bridge\n\n" +
		"Send any message to chat with Claude Code.\n\n" +
		"Commands:\n" +
		"/new — Start a fresh conversation\n" +
		"/status — Show current session info\n" +
		"/help — Show this help message"
}

// ensureSession returns the existing session for a chat or creates a new one.
func (b *Bridge) ensureSession(ctx context.Context, chatID int64) (*store.Session, error) {
	sess, err := b.store.GetSession(chatID)
	if err != nil {
		return nil, err
	}
	if sess != nil {
		// Ensure process manager knows about it
		if _, ok := b.proc.Get(chatID); !ok {
			procSess := &process.Session{
				ID:              fmt.Sprintf("%d", sess.ID),
				ChatID:          chatID,
				ClaudeSessionID: sess.ClaudeSessionID,
				Status:          process.StatusActive,
				CreatedAt:       sess.CreatedAt,
				UpdatedAt:       sess.UpdatedAt,
			}
			b.proc.Register(procSess)
		}
		return sess, nil
	}

	// Create new session
	procSess := process.NewSession(chatID)
	b.proc.Register(procSess)

	if err := b.store.SaveSession(chatID, procSess.ClaudeSessionID); err != nil {
		return nil, fmt.Errorf("save session: %w", err)
	}

	// Re-read to get the DB-assigned ID
	return b.store.GetSession(chatID)
}

// CleanupStaleSessions kills sessions that have been idle too long.
func (b *Bridge) CleanupStaleSessions(idleDuration time.Duration) error {
	chatIDs, err := b.store.StaleSessionChatIDs(idleDuration)
	if err != nil {
		return err
	}
	for _, chatID := range chatIDs {
		b.proc.Kill(chatID)
		b.store.UpdateSessionStatus(chatID, "stale")
		slog.Info("cleaned up stale session", "chat_id", chatID)
	}
	return nil
}
