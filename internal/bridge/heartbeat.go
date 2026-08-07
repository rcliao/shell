package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/beat"
	"github.com/rcliao/shell/internal/process"
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
	allChats := b.activeHeartbeatChats()
	for _, cid := range allChats {
		if ex := b.memory.RecentExchanges(ctx, cid, 5); len(ex) > 0 {
			// Tag each exchange with its SOURCE chat. A heartbeat has no
			// current chat (chat_id 0), so any follow-up it sends must name
			// a target explicitly — and without attribution the agent is
			// guessing. 7/19: work following up a DM question was relayed
			// to the family group because this list was flat and unlabeled.
			for _, e := range ex {
				exchanges = append(exchanges, fmt.Sprintf("(chat %d) %s", cid, e))
			}
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
	// Computed before the gate below, not after: a failed task on a quiet day
	// is exactly the case where nothing else would carry the beat, and
	// enriching only when there is already something to say would hide the
	// work precisely when it is the only thing worth reporting.
	queueWork := b.queueDigest()
	hasContent := len(exchanges) > 0 || insights != "" || hasConsolidation || isDeep || hasTaskStore || queueWork != ""

	slog.Info("heartbeat: enrichment",
		"chat_id", chatID,
		"chats_scanned", len(allChats),
		"exchanges", len(exchanges),
		"has_insights", insights != "",
		"has_consolidation", hasConsolidation,
		"has_content", hasContent,
		"is_deep", isDeep,
	)

	if !hasContent {
		return msg
	}

	var sb strings.Builder

	// CONTEXT: labeled facts only. No priority numbering — the goal framing
	// at the end asks the agent to judge what the beat needs; ordering here
	// is presentation, not instruction.

	// Consolidation candidates (memory hygiene)
	if consolidation != "" {
		sb.WriteString(consolidation)
		sb.WriteString("\n")
	}

	// Durable queue: work this agent registered, and work that failed.
	//
	// Injected rather than left to a tool call, because the tool call does not
	// happen. Over seven days the two live agents called shell_task once
	// between them, and that once was a smoke test — while calling ghost_put a
	// hundred times. The read half of a tool gets skipped even when the
	// instructions describe it, which is the same failure shell_schedule had
	// (three calls in fourteen days, all writes, never a list).
	//
	// So the queue's state arrives as part of the situation instead of as an
	// answer to a question nobody asks. A task that outlives the turn that
	// created it is only useful if something later notices it, and the
	// heartbeat is the only moment that reliably looks around.
	sb.WriteString(queueWork)

	// Shared task store activity (self-tasks + delegation)
	if b.taskStore != nil {
		if pending, err := b.taskStore.PendingTasksFor(b.agentBotUsername); err == nil && len(pending) > 0 {
			sb.WriteString("[Pending delegated/self tasks]\n")
			for _, t := range pending {
				src := "self"
				if t.FromAgent != t.ToAgent {
					src = t.FromAgent
				}
				id := t.ID
				if len(id) > 12 {
					id = id[:12]
				}
				sb.WriteString(fmt.Sprintf("- Task %s (%s): %s\n", id, src, t.Description))
			}
			sb.WriteString("[End delegated tasks]\n\n")
		}
		if recent, err := b.taskStore.RecentTasks(5); err == nil && len(recent) > 0 {
			hasActivity := false
			for _, t := range recent {
				// The shared store holds every agent's tasks; only surface
				// ones this agent sent or received, so one agent's activity
				// never leaks into another's heartbeat (and from there to chat).
				if t.FromAgent != b.agentBotUsername && t.ToAgent != b.agentBotUsername {
					continue
				}
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
					rid := t.ID
					if len(rid) > 12 {
						rid = rid[:12]
					}
					sb.WriteString(fmt.Sprintf("- %s %s (%s): %s → %s\n", icon, rid, arrow, t.Description, result))
				}
			}
			if hasActivity {
				sb.WriteString("[End recent tasks]\n\n")
			}
		}
	}

	// Recent conversations, tagged with source chat id (load-bearing: see below)
	if len(exchanges) > 0 {
		sb.WriteString("[Recent conversation history — each line is tagged with the chat it came from]\n")
		sb.WriteString("When you follow up on any of these, send the reply BACK TO THAT SAME chat id (shell_relay chat_id=<that id>, cross_chat=true). Never default to the group.\n")
		for _, ex := range exchanges {
			sb.WriteString("- ")
			sb.WriteString(ex)
			sb.WriteString("\n")
		}
		sb.WriteString("[End of recent history]\n\n")
	}

	// Previous heartbeat insights
	if insights != "" {
		sb.WriteString("[Previous heartbeat insights]\n")
		sb.WriteString(insights)
		sb.WriteString("\n[End of previous insights]\n\n")
	}

	// Deep only: existing behavioral learnings for review
	if isDeep && behavioralContext != "" {
		sb.WriteString("[Current behavioral learnings]\n")
		sb.WriteString(behavioralContext)
		sb.WriteString("\n[End of behavioral learnings]\n\n")
	}

	sb.WriteString(msg)

	// GOAL: judgment framing instead of a numbered checklist. The agent
	// decides what (if anything) most deserves this beat; the context above
	// is information, not a to-do list.
	if isDeep {
		// Skill inventory retro: ground-truth usage stats + action menu.
		if retro := b.buildSkillRetroBlock(); retro != "" {
			sb.WriteString(retro)
		}
		// Pin hygiene: importance decides what a budget-constrained retrieval
		// keeps, but nothing maintains it, so it drifts out of line with
		// consequence. Surface the cut and let the agent re-rank its own pins.
		if audit := b.buildPinAuditBlock(ctx, chatID); audit != "" {
			sb.WriteString(audit)
		}
		// NOTE: the pinned skill-inventory digest was retired 2026-07-28 — the
		// skills catalog is composed into every generation's system prompt, so
		// skills never needed a pin to survive rotation; the pin duplicated
		// the catalog and its refresh churned a dead row every deep beat.

		sb.WriteString("\n\n---\n**[Deep Reflection]**\n")
		sb.WriteString("This is your deep-reflection heartbeat — the one turn where you think as hard as you can about getting better. ultrathink: don't settle for the first observation, trace root causes, weigh alternatives, and be honest about your own failures. Everything above is context, not a checklist — judge what most deserves this beat's attention and do that one thing well. Typical moves, any one of which can be the whole beat:\n")
		sb.WriteString("- Complete a due task (scripts/shell-task complete --id <id>), or open a task row for in-flight multi-step work that has none (scripts/shell-task add --description \"<work>\") so it survives session rotation.\n")
		sb.WriteString("- Consolidate any flagged memory clusters above: ghost_get the full content, write a concise summary, ghost_consolidate.\n")
		sb.WriteString("- Distill what recent conversations actually taught you — a correction, a missed expectation, a recurring pattern — into ONE stored adjustment (scripts/shell-remember --action behavioral --content \"<specific behavior change>\" --kind procedural), or sharpen/retire a vague or superseded learning shown above. Specific and testable beats \"be more helpful\".\n")
		sb.WriteString("- Formalize a routine you've now done manually 2+ times as a schedule you author yourself (scripts/shell-schedule cron --expr \"<cron>\" --message \"<msg>\" --mode <prompt|notify>, or once --at \"<HH:MM or ISO>\"). Under-scheduling is the common failure mode; each agent's own schedule library is where differentiation comes from.\n")
		if !b.lessonToActionDisabled {
			sb.WriteString("- **Lesson to action** (AT MOST ONE action per deep beat): pick one stored lesson (insights, behavioral learnings, memory context) that is applicable RIGHT NOW and act on it — a skill draft in your own workspace, a pinned-memory adjustment encoding the corrected behavior, a lingering task the lesson points at, or ONE proactive message only if the lesson directly concerns something the family explicitly asked for. Guardrails: never send media; never message the family group otherwise; changes only to your OWN workspace and pins — never to shell repo code or another agent's state. Ledger the action: scripts/shell-remember --action heartbeat-learning --content \"[lesson-action] <lesson> → <action taken>\". If no stored lesson is actionable right now, say so in one line — that's a valid outcome, not a failure.\n")
		}
		sb.WriteString("- Send one genuinely useful or delightful message: a due reminder, a finding, a follow-up the user expects.\n")
		sb.WriteString("Boundaries: never send media unless a user explicitly asked in the current conversation; don't re-send reminders that a notify-mode schedule already owns. Reflection alone is not a reason to send anything — [noop] is the normal outcome for most heartbeats.\n")
	} else {
		sb.WriteString("\n\n---\nDecide what, if anything, this beat needs — the context above is information, not a to-do list. Typical moves: complete a due task (scripts/shell-task complete --id <id>), consolidate any flagged memory clusters above (ghost_get → concise summary → ghost_consolidate), or record a genuinely new insight from recent conversations (scripts/shell-remember --action heartbeat-learning --content \"<specific, actionable insight>\"). Never send media unprompted, and don't re-send reminders that a notify-mode schedule already owns — [noop] is the normal outcome for most heartbeats.\n")
	}

	sb.WriteString("\nIf there is nothing that needs a user-facing message, respond with just: [noop]\n")

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

// deepHeartbeatPrefix marks a deep-reflection beat. The scheduler prepends it
// in execute(); the bridge keys journal capture off it.
const deepHeartbeatPrefix = "[Heartbeat:deep] "

// captureReflection journals one deep-heartbeat turn.
//
// The scheduler attaches the fire's job_runs id and heartbeat counter to the
// context (internal/beat), so a journal row joins to the ledger exactly rather
// than by timestamp proximity. Metadata is absent for a manually invoked beat,
// and the zero Meta is fine — the row is still captured, just unlinked.
func (b *Bridge) captureReflection(ctx context.Context, chatID int64, response string, result process.SendResult, turnModel string) {
	meta := beat.From(ctx)
	id, err := b.store.RecordReflection(store.Reflection{
		JobRunID:   meta.RunID,
		ChatID:     chatID,
		BeatCount:  meta.Count,
		Model:      turnModel,
		Text:       response,
		ToolCalls:  len(result.ToolCalls),
		Noop:       response == "",
		DurationMS: result.Timings.TotalMs,
	})
	if err != nil {
		slog.Warn("reflection capture failed", "chat_id", chatID, "error", err)
		return
	}
	slog.Info("reflection captured",
		"id", id, "chat_id", chatID, "job_run_id", meta.RunID, "beat", meta.Count,
		"chars", len(response), "tool_calls", len(result.ToolCalls), "noop", response == "")
}
