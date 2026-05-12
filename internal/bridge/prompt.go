package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// skillsSystemPrompt returns the skills listing and bridge operation rules.
func (b *Bridge) skillsSystemPrompt() string {
	if b.skills == nil {
		return ""
	}
	prompt := b.skills.CatalogPrompt()
	prompt += "\n\n## Bridge Rules\n\n" +
		"Do NOT emit text directives like `[pm]`, `[tunnel]`, `[schedule]`, `[relay]`, " +
		"`[remember]`, `[heartbeat-learning]`, `[task-complete]`, or `[browser]` in your response. " +
		"These are silently stripped and do nothing.\n\n" +
		"For process management and tunnels, use the `shell_pm` and `shell_tunnel` MCP tools directly.\n" +
		"For scheduling, memory, relay, and tasks, use the corresponding skill scripts via Bash.\n\n" +
		"**CRITICAL:** NEVER run long-running processes (servers, watchers) directly via Bash. " +
		"Always use `shell_pm` so they are tracked, have logs, and can be stopped.\n\n" +
		"**IMPORTANT — Scheduling:** CronCreate is SESSION-ONLY and dies on every session restart. " +
		"NEVER use CronCreate for reminders. ALWAYS use `scripts/shell-schedule` instead — it persists to SQLite.\n" +
		"Example: `scripts/shell-schedule once --at \"21:00\" --message \"Flonase time!\" --mode notify`\n" +
		"Example: `scripts/shell-schedule cron --expr \"0 21 * * *\" --message \"Daily Flonase\" --mode notify`\n"

	// Playground directory info (if configured).
	if b.claudeCfg.PlaygroundDir != "" {
		prompt += fmt.Sprintf("\n## Playground\n\nWritable sandbox: `%s` — create project subdirectories here for web apps, experiments, and prototypes.\n", b.claudeCfg.PlaygroundDir)
	}

	return prompt
}

// skillOverrides returns true if a skill with the given name is loaded,
// meaning the built-in directive should be suppressed in favor of the skill.
func (b *Bridge) skillOverrides(name string) bool {
	return b.skills != nil && b.skills.Has(name)
}

// timestampSystemPrompt returns guidance about where to find the current time.
// It deliberately does NOT include a specific date — on resumed sessions the
// system prompt is cached, so any hardcoded date would become stale. The
// authoritative time is injected per-turn via injectCurrentTime().
func (b *Bridge) timestampSystemPrompt() string {
	tz := b.schedulerTZ
	if tz == "" {
		tz = "UTC"
	}
	return "\n\n## Current Time\n\n" +
		"Each user message is prefixed with `[Current time: ...]` containing the authoritative " +
		"date, day of week, and time. **ALWAYS read that marker to determine what day it is.** " +
		"Do not trust dates from conversation history, compacted summaries, or your own prior " +
		"responses — only trust the `[Current time: ...]` marker on the current turn.\n" +
		"Timezone: " + tz + "\n"
}

// injectCurrentTime prepends a precise timestamp to the user message when the
// scheduler is enabled, so Claude always knows the exact current time for
// computing relative schedule expressions like "in 30 minutes".
//
// Deprecated: prefer injectPerTurnContext, which composes the full Channel B
// prefix (time + pinned-memory delta + tasks). This helper is kept for unit
// tests and paths where chat context is unavailable.
func (b *Bridge) injectCurrentTime(msg string) string {
	if !b.schedulerEnabled {
		return msg
	}
	loc, _ := time.LoadLocation(b.schedulerTZ)
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	// Include day name explicitly — the agent has been getting day-of-week wrong
	// because the system prompt's Monday/Tuesday/etc. was cached on resumed sessions.
	return fmt.Sprintf("[Current time: %s | %s]\n%s",
		now.Format("Monday 2006-01-02 15:04 MST"),
		b.schedulerTZ,
		msg)
}

// pinnedDeltaTokenBudget is the soft cap on how many tokens the pinned-delta
// block may occupy in Channel B. Beyond this, rotation is flagged instead —
// rebaking the prefix is cheaper than continuing to inject the diff every turn.
const pinnedDeltaTokenBudget = 1000

// injectPerTurnContext composes the full Channel B prefix for a user turn:
// current time, carry-forward pack (on post-rotation fresh turns), pinned-
// memory delta since session generation start, and any active background
// tasks. Each block is omitted when empty.
//
// When the pinned-delta exceeds pinnedDeltaTokenBudget, the block is dropped
// and the session is flagged rotate_pending — the next turn will rotate and
// rebuild Channel A with a fresh pinned snapshot.
//
// ChatID may be 0 (phantom system chat); behavior is identical.
func (b *Bridge) injectPerTurnContext(ctx context.Context, chatID, threadID int64, msg string) string {
	var blocks []string

	// Block 1: current time (always on when scheduler is enabled).
	if b.schedulerEnabled {
		loc, _ := time.LoadLocation(b.schedulerTZ)
		if loc == nil {
			loc = time.UTC
		}
		now := time.Now().In(loc)
		blocks = append(blocks, fmt.Sprintf("[Current time: %s | %s]",
			now.Format("Monday 2006-01-02 15:04 MST"),
			b.schedulerTZ))
	}

	var sess *store.Session
	if b.store != nil {
		sess, _ = b.store.GetSession(chatID, threadID)
	}

	// Block 2: carry-forward pack, only on the first turn after rotation.
	// Detection: session has no Claude UUID yet (fresh send about to run) AND
	// a prior-generation summary exists. The pack rides the user message so
	// the agent can acknowledge continuity without waiting on Channel A.
	if sess != nil && sess.ProviderSessionID == "" {
		if block := b.buildCarryForwardBlock(chatID, threadID, sess.Generation); block != "" {
			blocks = append(blocks, block)
		}
	}

	// Block 3: pinned-memory delta since generation start.
	if b.memory != nil && sess != nil {
		// Legacy-session bootstrap: rows that existed before the lifecycle
		// upgrade have prefix_hash="" but an active Claude UUID. Stamp the
		// current hash once on first turn post-upgrade so the delta logic
		// has a baseline — otherwise every pinned memory would show as
		// "new since session start" and instantly blow the budget.
		if sess.PrefixHash == "" && sess.ProviderSessionID != "" {
			if _, hash := b.memory.PinnedSnapshot(ctx, chatID); hash != "" {
				if err := b.store.SetPrefixHash(chatID, threadID, hash); err != nil {
					slog.Warn("legacy prefix hash stamp failed", "chat_id", chatID, "error", err)
				}
			}
		} else {
			delta, _, tokens := b.memory.PinnedDelta(ctx, chatID, sess.GenerationStartedAt, sess.PrefixHash)
			switch {
			case tokens > pinnedDeltaTokenBudget:
				// Budget blown — don't inject; flag for rotation instead.
				if err := b.store.SetRotatePending(chatID, threadID, true); err != nil {
					slog.Warn("set rotate_pending failed", "chat_id", chatID, "error", err)
				}
				slog.Info("pinned delta over budget, flagging rotation",
					"chat_id", chatID, "thread_id", threadID, "tokens", tokens)
			case delta != "":
				blocks = append(blocks, "[Memory updates since session start:\n"+strings.TrimRight(delta, "\n")+"]")
				// Note: we intentionally do NOT update prefix_hash here — the
				// hash only advances at generation rotation. Otherwise a turn
				// would "accept" the delta silently and future turns would
				// stop showing it.
			}
		}
	}

	// Block 4: active background tasks (for non-system chats).
	if b.store != nil && chatID != 0 {
		tasks, err := b.store.PendingTasks(chatID)
		if err == nil && len(tasks) > 0 {
			var sb strings.Builder
			sb.WriteString("[Active tasks:\n")
			for _, t := range tasks {
				sb.WriteString("- ")
				sb.WriteString(t.Description)
				sb.WriteString("\n")
			}
			sb.WriteString("]")
			blocks = append(blocks, sb.String())
		}
	}

	if len(blocks) == 0 {
		return msg
	}
	return strings.Join(blocks, "\n") + "\n" + msg
}

// buildCarryForwardBlock returns the "Previously in this chat" + relevant-
// memory-pack block for the first turn of a new generation. Returns empty
// when there is no prior generation (brand new session) or the summary is
// for a non-adjacent generation (already consumed).
func (b *Bridge) buildCarryForwardBlock(chatID, threadID, currentGen int64) string {
	summary, err := b.store.GetLatestSessionSummary(chatID, threadID)
	if err != nil || summary == nil {
		return ""
	}
	// Only attach when the summary is for the immediately-prior generation.
	// Older summaries mean we've already rotated past them.
	if summary.Generation != currentGen-1 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("[Previously in this chat (generation ")
	fmt.Fprintf(&sb, "%d", summary.Generation)
	sb.WriteString(" summary):\n")
	sb.WriteString(strings.TrimSpace(summary.Summary))
	sb.WriteString("\n]")

	if summary.MemoryPack != "" {
		var pack []struct {
			Key     string `json:"key"`
			Kind    string `json:"kind"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(summary.MemoryPack), &pack); err == nil && len(pack) > 0 {
			sb.WriteString("\n[Relevant memory context:\n")
			for _, p := range pack {
				sb.WriteString("- [")
				sb.WriteString(p.Kind)
				sb.WriteString("] ")
				sb.WriteString(p.Content)
				sb.WriteString("\n")
			}
			sb.WriteString("]")
		}
	}
	return sb.String()
}
