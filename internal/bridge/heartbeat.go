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
// When isDeep is true, adds behavioral reflection prompts for self-evaluation.
// Returns the original message unchanged if there's nothing to reflect on.
func (b *Bridge) enrichHeartbeatPrompt(ctx context.Context, chatID int64, msg string, isDeep bool) string {
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

	// For deep heartbeats, also load existing behavioral learnings for review.
	var behavioralContext string
	if isDeep {
		behavioralContext = b.memory.BehavioralContext(ctx, chatID, 500)
	}

	hasConsolidation := consolidation != ""
	hasTaskStore := b.taskStore != nil
	hasContent := len(exchanges) > 0 || insights != "" || len(pendingTasks) > 0 || hasConsolidation || isDeep || hasTaskStore

	slog.Info("heartbeat: enrichment",
		"chat_id", chatID,
		"chats_scanned", len(allChats),
		"exchanges", len(exchanges),
		"pending_tasks", len(pendingTasks),
		"has_insights", insights != "",
		"has_consolidation", hasConsolidation,
		"has_content", hasContent,
		"is_deep", isDeep,
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

	// Priority 2b: Shared task store activity (self-tasks + delegation)
	if b.taskStore != nil {
		if pending, err := b.taskStore.PendingTasksFor(b.agentBotUsername); err == nil && len(pending) > 0 {
			sb.WriteString("[Pending delegated/self tasks]\n")
			for _, t := range pending {
				src := "self"
				if t.FromAgent != t.ToAgent {
					src = t.FromAgent
				}
				sb.WriteString(fmt.Sprintf("- Task %s (%s): %s\n", t.ID[:12], src, t.Description))
			}
			sb.WriteString("[End delegated tasks]\n\n")
		}
		if recent, err := b.taskStore.RecentTasks(5); err == nil && len(recent) > 0 {
			hasActivity := false
			for _, t := range recent {
				if t.Status == "completed" || t.Status == "failed" {
					if !hasActivity {
						sb.WriteString("[Recent task completions]\n")
						hasActivity = true
					}
					icon := "✅"
					if t.Status == "failed" {
						icon = "❌"
					}
					arrow := t.FromAgent + " → " + t.ToAgent
					if t.FromAgent == t.ToAgent {
						arrow = t.FromAgent + " (self)"
					}
					result := t.Result
					if len(result) > 100 {
						result = result[:100] + "..."
					}
					sb.WriteString(fmt.Sprintf("- %s %s (%s): %s → %s\n", icon, t.ID[:12], arrow, t.Description, result))
				}
			}
			if hasActivity {
				sb.WriteString("[End recent tasks]\n\n")
			}
		}
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

	// Priority 5 (deep only): Existing behavioral learnings for review
	if isDeep && behavioralContext != "" {
		sb.WriteString("[Current behavioral learnings]\n")
		sb.WriteString(behavioralContext)
		sb.WriteString("\n[End of behavioral learnings]\n\n")
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

	// Deep heartbeats add behavioral self-evaluation + skill inventory retro.
	if isDeep {
		if retro := b.buildSkillRetroBlock(); retro != "" {
			sb.WriteString(retro)
		}
		// Refresh the pinned skill-inventory memory so the agent's tools
		// survive session rotation. Non-blocking; failures are logged.
		b.refreshSkillInventoryMemory(ctx, chatID)

		sb.WriteString("\n---\n**[Deep Reflection — Behavioral Self-Evaluation]**\n")
		sb.WriteString("This is a deep reflection heartbeat. In addition to the regular priorities above, perform a thorough self-evaluation:\n\n")
		sb.WriteString("1. **Response quality audit**: Review the recent conversations above. Were any of your responses:\n")
		sb.WriteString("   - Corrected by the user? (e.g., wrong info, wrong format, missed context)\n")
		sb.WriteString("   - Followed up with clarification? (suggests your first answer was unclear)\n")
		sb.WriteString("   - Ignored or met with a short \"ok\"? (may indicate low value)\n\n")
		sb.WriteString("2. **Missed expectations**: Did you miss anything the user likely expected?\n")
		sb.WriteString("   - Reminders that should have fired but didn't\n")
		sb.WriteString("   - Context from previous conversations you should have recalled\n")
		sb.WriteString("   - Follow-ups you should have proactively offered\n\n")
		sb.WriteString("3. **Usage pattern recognition**: What task types does each user ask you for most?\n")
		sb.WriteString("   - Are you getting better at those tasks? What specific improvements can you make?\n")
		sb.WriteString("   - Are there recurring tasks you could handle more efficiently?\n\n")
		sb.WriteString("4. **Store behavioral adjustments** as procedural memories:\n")
		sb.WriteString("   scripts/shell-remember --action behavioral --content \"<specific behavior change>\" --kind procedural\n")
		sb.WriteString("   Good examples:\n")
		sb.WriteString("   - \"When mami asks about food storage, always include safety timeframe not just yes/no\"\n")
		sb.WriteString("   - \"When papi reports a bug, check the logs first before asking clarifying questions\"\n")
		sb.WriteString("   - \"Meal memo format: always echo back the date, items as bullet list, and a brief reaction\"\n")
		sb.WriteString("   Bad examples (too vague):\n")
		sb.WriteString("   - \"Be more helpful\" / \"Try harder\" / \"Remember things better\"\n\n")
		sb.WriteString("5. **Review existing behavioral learnings** (shown above if any exist).\n")
		sb.WriteString("   - Are any outdated or superseded? Update or remove them.\n")
		sb.WriteString("   - Are any too vague to be actionable? Make them more specific.\n\n")
		sb.WriteString("6. **Schedule self-authorship**: scan recent conversations + previous insights for actions you took 2+ times manually that are predictable in time (a daily check, a weekly nudge, a recurring report). Author the schedule yourself instead of waiting for it to be set up for you:\n")
		sb.WriteString("     scripts/shell-schedule cron --expr \"<cron>\" --message \"<msg>\" --mode <prompt|notify>\n")
		sb.WriteString("     scripts/shell-schedule once --at \"<HH:MM or ISO>\" --message \"<msg>\" --mode notify\n")
		sb.WriteString("   Bias toward authoring — under-scheduling is the failure mode. If a routine has emerged organically, formalize it. Differentiation comes from each agent's own schedule library, not from shared prompts.\n\n")
		sb.WriteString("7. **Task hygiene**: scan recent conversations for in-flight multi-step work that has no task row backing it. Open one so it survives session rotation and shows up in heartbeat context next time:\n")
		sb.WriteString("     scripts/shell-task add --description \"<work>\"\n")
		sb.WriteString("   Mark complete with `scripts/shell-task complete --id <id>` when done. The task table is currently underused — most multi-step work evaporates because it never got tracked.\n\n")
		sb.WriteString("After reflection, send a brief check-in message to the most recently active chat.\n")
	}

	sb.WriteString("\nIf there is genuinely nothing to do, respond with just: [noop]\n")

	return sb.String()
}

// EnsureDefaultHeartbeats creates a single agent-level heartbeat in the phantom
// SystemChat. This is the agent's "inner monologue" — heartbeat reflection runs
// here, aggregates context from all real chats, writes to agent-wide memory, and
// uses shell-relay to send any actual outputs to real chats. No Telegram delivery
// happens for the system chat itself.
// Called at daemon startup.
func (b *Bridge) EnsureDefaultHeartbeats() {
	if !b.schedulerEnabled || b.memory == nil {
		return
	}
	b.ensureDefaultHeartbeat(SystemChatID)
}

// activeHeartbeatChats returns all real chat IDs (excluding the system chat)
// for heartbeat context aggregation. The system chat is excluded so the agent
// reflects on real user conversations, not its own past heartbeat thoughts.
func (b *Bridge) activeHeartbeatChats() []int64 {
	sessions, err := b.store.ListActiveSessions()
	if err != nil {
		return nil
	}
	chats := make([]int64, 0, len(sessions))
	for _, sess := range sessions {
		if IsSystemChat(sess.ChatID) {
			continue
		}
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
