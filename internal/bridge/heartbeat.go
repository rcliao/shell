package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// defaultHeartbeatInterval is the interval for auto-created heartbeats.
const defaultHeartbeatInterval = "1h"

// defaultHeartbeatMessage is the prompt for auto-created heartbeats.
const defaultHeartbeatMessage = "Review recent activity and check for anything that needs attention."

// enrichHeartbeatPrompt augments a heartbeat message with recent conversation
// history, previous heartbeat insights, memory context, and pending background
// tasks for self-improvement reflection and proactive work.
// Returns the original message unchanged if there's nothing to reflect on.
func (b *Bridge) enrichHeartbeatPrompt(ctx context.Context, chatID int64, msg string) string {
	// Fetch context up front to decide whether enrichment is worthwhile
	exchanges := b.memory.RecentExchanges(ctx, chatID, 10)
	insights := b.memory.HeartbeatContext(ctx, chatID, 500)

	// Fetch pending background tasks
	pendingTasks, _ := b.store.PendingTasks(chatID)

	// Fetch general memory context for reflection (reduced budget for heartbeats)
	memoryCtx := b.memory.SystemPromptWithBudget(ctx, chatID, 300)

	hasContent := len(exchanges) > 0 || insights != "" || len(pendingTasks) > 0

	// Skip enrichment if there's no history, learnings, or tasks
	if !hasContent {
		slog.Debug("heartbeat: skipping enrichment, no history, insights, or tasks", "chat_id", chatID)
		return msg
	}

	var sb strings.Builder

	if len(exchanges) > 0 {
		sb.WriteString("[Recent conversation history for reflection]\n")
		for _, ex := range exchanges {
			sb.WriteString("- ")
			sb.WriteString(ex)
			sb.WriteString("\n")
		}
		sb.WriteString("[End of recent history]\n\n")
	}

	if insights != "" {
		sb.WriteString("[Previous heartbeat insights]\n")
		sb.WriteString(insights)
		sb.WriteString("\n[End of previous insights]\n\n")
	}

	// Include memory context so heartbeat can reflect on stored knowledge
	if memoryCtx != "" {
		sb.WriteString("[Memory context for reflection]\n")
		sb.WriteString(memoryCtx)
		sb.WriteString("\n[End of memory context]\n\n")
	}

	// Include pending background tasks
	if len(pendingTasks) > 0 {
		sb.WriteString("[Pending background tasks]\n")
		for _, t := range pendingTasks {
			sb.WriteString(fmt.Sprintf("- Task #%d: %s (queued %s)\n", t.ID, t.Description, t.CreatedAt.Format("Jan 2 15:04")))
		}
		sb.WriteString("[End of pending tasks]\n\n")
		sb.WriteString("If you can complete any pending tasks above, do so and run:\n")
		sb.WriteString("scripts/shell-task complete --id <task_id>\n\n")
	}

	sb.WriteString(msg)

	sb.WriteString("\n\n---\n")
	sb.WriteString("Instructions for this heartbeat:\n")
	sb.WriteString("1. Proactively check for anything that needs attention (files, PRs, notifications, scheduled items).\n")
	sb.WriteString("2. If pending background tasks are listed, try to complete them.\n")
	sb.WriteString("3. Reflect on recent conversations and memory for patterns or corrections.\n")
	sb.WriteString("4. If you notice reusable patterns, user preferences, or useful corrections, store them:\n")
	sb.WriteString("   scripts/shell-remember --action heartbeat-learning --content \"<specific, actionable insight>\"\n")
	sb.WriteString("5. If there is genuinely nothing to report, respond with just: [noop]\n")
	sb.WriteString("6. Keep responses concise and actionable.\n")

	return sb.String()
}

// EnsureDefaultHeartbeats creates default heartbeats for all active sessions
// that don't already have one. Called at daemon startup.
func (b *Bridge) EnsureDefaultHeartbeats() {
	if !b.schedulerEnabled || b.memory == nil {
		return
	}
	sessions, err := b.store.ListActiveSessions()
	if err != nil {
		slog.Warn("failed to list sessions for default heartbeats", "error", err)
		return
	}
	for _, sess := range sessions {
		b.ensureDefaultHeartbeat(sess.ChatID)
	}
}

// ensureDefaultHeartbeat creates a default 1-hour heartbeat for a chat if none exists.
func (b *Bridge) ensureDefaultHeartbeat(chatID int64) {
	hb, err := b.store.GetHeartbeat(chatID)
	if err != nil {
		slog.Warn("failed to check for existing heartbeat", "chat_id", chatID, "error", err)
		return
	}
	if hb != nil {
		return // already has a heartbeat
	}

	interval, _ := time.ParseDuration(defaultHeartbeatInterval)
	nextRun := time.Now().Add(interval).UTC()

	sched := &store.Schedule{
		ChatID:    chatID,
		Label:     "Heartbeat: " + defaultHeartbeatMessage,
		Message:   defaultHeartbeatMessage,
		Schedule:  defaultHeartbeatInterval,
		Timezone:  b.schedulerTZ,
		Type:      "heartbeat",
		Mode:      "prompt",
		NextRunAt: nextRun,
		Enabled:   true,
	}

	id, err := b.store.SaveSchedule(sched)
	if err != nil {
		slog.Warn("failed to create default heartbeat", "chat_id", chatID, "error", err)
		return
	}
	slog.Info("default heartbeat created", "chat_id", chatID, "id", id, "interval", defaultHeartbeatInterval)
}
