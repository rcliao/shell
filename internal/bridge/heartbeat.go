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
// history, previous heartbeat insights, memory context, consolidation candidates,
// and pending background tasks for self-improvement reflection and proactive work.
// Returns the original message unchanged if there's nothing to reflect on.
func (b *Bridge) enrichHeartbeatPrompt(ctx context.Context, chatID int64, msg string) string {
	// Aggregate context from all active chats (shared heartbeat optimization).
	// This pulls recent exchanges and pending tasks from every chat, not just
	// the heartbeat's own chat, so a single heartbeat covers all conversations.
	var exchanges []string
	var pendingTasks []store.Task
	allChats := b.activeHeartbeatChats()
	for _, cid := range allChats {
		if ex := b.memory.RecentExchanges(ctx, cid, 5); len(ex) > 0 {
			exchanges = append(exchanges, ex...)
		}
		if tasks, err := b.store.PendingTasks(cid); err == nil {
			pendingTasks = append(pendingTasks, tasks...)
		}
	}
	insights := b.memory.HeartbeatContext(ctx, chatID, 500)
	consolidation := b.popConsolidationCandidates(chatID)

	hasConsolidation := consolidation != ""
	hasContent := len(exchanges) > 0 || insights != "" || len(pendingTasks) > 0 || hasConsolidation

	slog.Info("heartbeat: enrichment",
		"chat_id", chatID,
		"chats_scanned", len(allChats),
		"exchanges", len(exchanges),
		"pending_tasks", len(pendingTasks),
		"has_insights", insights != "",
		"has_consolidation", hasConsolidation,
		"has_content", hasContent,
	)

	if !hasContent {
		return msg
	}

	var sb strings.Builder

	// Priority 1: Consolidation tasks (memory hygiene)
	if consolidation != "" {
		sb.WriteString(consolidation)
		sb.WriteString("\n")
	}

	// Priority 2: Pending background tasks
	if len(pendingTasks) > 0 {
		sb.WriteString("[Pending background tasks]\n")
		for _, t := range pendingTasks {
			sb.WriteString(fmt.Sprintf("- Task #%d: %s (queued %s)\n", t.ID, t.Description, t.CreatedAt.Format("Jan 2 15:04")))
		}
		sb.WriteString("[End of pending tasks]\n\n")
	}

	// Priority 3: Recent conversations for reflection
	if len(exchanges) > 0 {
		sb.WriteString("[Recent conversation history]\n")
		for _, ex := range exchanges {
			sb.WriteString("- ")
			sb.WriteString(ex)
			sb.WriteString("\n")
		}
		sb.WriteString("[End of recent history]\n\n")
	}

	// Priority 4: Previous insights
	if insights != "" {
		sb.WriteString("[Previous heartbeat insights]\n")
		sb.WriteString(insights)
		sb.WriteString("\n[End of previous insights]\n\n")
	}

	sb.WriteString(msg)

	sb.WriteString("\n\n---\nHeartbeat priorities (in order):\n")
	if consolidation != "" {
		sb.WriteString("1. **CONSOLIDATE** the memory clusters listed above — use ghost_get to read full content, write a concise summary, call ghost_consolidate. This is the highest priority.\n")
	}
	if len(pendingTasks) > 0 {
		sb.WriteString("2. Complete pending background tasks (scripts/shell-task complete --id <id>).\n")
	}
	sb.WriteString("3. If recent conversations reveal patterns, corrections, or user preferences worth remembering:\n")
	sb.WriteString("   scripts/shell-remember --action heartbeat-learning --content \"<specific, actionable insight>\"\n")
	sb.WriteString("4. If there is genuinely nothing to do, respond with just: [noop]\n")

	return sb.String()
}

// EnsureDefaultHeartbeats creates a single shared heartbeat for one primary DM chat
// rather than separate heartbeats per chat. This reduces token usage by ~2/3 since
// all memory maintenance and reflection happens in one session.
// Called at daemon startup.
func (b *Bridge) EnsureDefaultHeartbeats() {
	if !b.schedulerEnabled || b.memory == nil {
		return
	}
	sessions, err := b.store.ListActiveSessions()
	if err != nil {
		slog.Warn("failed to list sessions for default heartbeats", "error", err)
		return
	}

	// Pick the first DM chat (positive chat_id) as the primary heartbeat chat.
	// Group chats (negative chat_id) don't need their own heartbeat since the
	// shared heartbeat aggregates context from all chats.
	var primaryChat int64
	for _, sess := range sessions {
		if sess.ChatID > 0 {
			primaryChat = sess.ChatID
			break
		}
	}
	if primaryChat == 0 && len(sessions) > 0 {
		primaryChat = sessions[0].ChatID // fallback to any chat
	}
	if primaryChat == 0 {
		return
	}

	b.ensureDefaultHeartbeat(primaryChat)
}

// activeHeartbeatChats returns all active chat IDs for heartbeat context aggregation.
func (b *Bridge) activeHeartbeatChats() []int64 {
	sessions, err := b.store.ListActiveSessions()
	if err != nil {
		return nil
	}
	chats := make([]int64, 0, len(sessions))
	for _, sess := range sessions {
		chats = append(chats, sess.ChatID)
	}
	return chats
}

// ensureDefaultHeartbeat creates a default heartbeat for a chat if none exists.
func (b *Bridge) ensureDefaultHeartbeat(chatID int64) {
	hb, err := b.store.GetHeartbeat(chatID)
	if err != nil {
		slog.Warn("failed to check for existing heartbeat", "chat_id", chatID, "error", err)
		return
	}
	if hb != nil {
		return // already has a heartbeat
	}

	hbInterval := b.heartbeatInterval
	if hbInterval == "" {
		hbInterval = defaultHeartbeatInterval
	}

	interval, _ := time.ParseDuration(hbInterval)
	nextRun := time.Now().Add(interval).UTC()

	sched := &store.Schedule{
		ChatID:    chatID,
		Label:     "Heartbeat: " + defaultHeartbeatMessage,
		Message:   defaultHeartbeatMessage,
		Schedule:  hbInterval,
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
	slog.Info("default heartbeat created", "chat_id", chatID, "id", id, "interval", hbInterval)
}
