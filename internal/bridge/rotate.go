package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/store"
)

// rotationMaxAge is the soft trigger for rotating based on generation age.
// Longer-lived generations accumulate more drift; by a week the per-turn
// pinned-delta injection has usually paid back its cost in cache saves.
const rotationMaxAge = 7 * 24 * time.Hour

// rotationMinAge is the floor that gates the day-boundary trigger so we
// don't churn sessions every few minutes around midnight. Sessions younger
// than this skip the day-boundary check (other triggers still apply).
const rotationMinAge = 30 * time.Minute

// rotationSummaryExchanges is how many recent exchanges we fold into the
// mechanical summary handed to the new generation's first user message.
// Kept small so the carry-forward pack stays under a few hundred tokens.
const rotationSummaryExchanges = 8

// rotationMemoryPackBudget bounds the semantic-memory slice pulled from
// ghost at rotation time. This is Channel A adjacent — the pack rides the
// next generation's first user message, so it's big enough to preserve
// context but small enough not to bloat every subsequent turn.
const rotationMemoryPackBudget = 800

// maybeRotate inspects a session and rotates if any trigger fires:
//   - rotate_pending flag (set by Channel B delta overflow, manual CLI, or
//     skill/identity hash change)
//   - generation age exceeds rotationMaxAge
//   - local calendar day changed since generation started, and the session
//     is older than rotationMinAge (prevents churn near midnight)
//
// The day-boundary trigger keeps the cached prefix and conversation history
// in sync with "today" — without it, a Friday session running into Saturday
// keeps Friday's [Current time:] markers in history and the agent often
// pattern-matches on the older bulk instead of the freshest marker.
//
// Returns true when a rotation happened so callers can refresh any cached
// session state they hold.
func (b *Bridge) maybeRotate(ctx context.Context, chatID, threadID int64) bool {
	if b.store == nil {
		return false
	}
	sess, err := b.store.GetSession(chatID, threadID)
	if err != nil || sess == nil {
		return false
	}
	// Never rotate a session that hasn't had its first turn yet — there's
	// nothing to summarize and the UUID is already empty.
	if sess.ProviderSessionID == "" {
		return false
	}

	age := time.Since(sess.GenerationStartedAt)
	dayChanged := age >= rotationMinAge && b.calendarDayChanged(sess.GenerationStartedAt)

	if !sess.RotatePending && age < rotationMaxAge && !dayChanged {
		return false
	}

	reason := "soft_trigger"
	switch {
	case sess.RotatePending:
		reason = "rotate_pending"
	case age >= rotationMaxAge:
		reason = "age"
	case dayChanged:
		reason = "day_boundary"
	}
	if err := b.rotateSession(ctx, chatID, threadID, sess, reason); err != nil {
		slog.Warn("session rotation failed", "chat_id", chatID, "thread_id", threadID, "error", err)
		return false
	}
	return true
}

// calendarDayChanged returns true when the local-timezone calendar day of
// `now` differs from the day of `start`. Uses b.schedulerTZ so the boundary
// matches user-perceived "today" rather than UTC.
func (b *Bridge) calendarDayChanged(start time.Time) bool {
	loc, _ := time.LoadLocation(b.schedulerTZ)
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	startLocal := start.In(loc)
	return now.Year() != startLocal.Year() ||
		now.Month() != startLocal.Month() ||
		now.Day() != startLocal.Day()
}

// rotateSession closes the current generation and prepares a fresh one. It
// writes a session_summaries row capturing the mechanical summary + relevant
// memory pack, then bumps the generation counter (which also clears the
// Claude UUID, prefix hash, rotate_pending, and compact_state — see
// store.BumpGeneration). The in-memory process session is reset so the next
// send runs a fresh CLI invocation with --append-system-prompt.
//
// Summary is mechanical (last N exchanges from message_map) rather than
// LLM-generated: rotation runs in the hot path and we want it fast. The
// carry-forward pack handles semantic coverage.
func (b *Bridge) rotateSession(ctx context.Context, chatID, threadID int64, sess *store.Session, reason string) error {
	slog.Info("rotating session",
		"chat_id", chatID,
		"thread_id", threadID,
		"generation", sess.Generation,
		"reason", reason,
		"age", time.Since(sess.GenerationStartedAt),
	)

	summary := b.buildRotationSummary(sess)
	memoryPackJSON := b.buildRotationMemoryPack(ctx, chatID, summary)

	if err := b.store.SaveSessionSummary(chatID, threadID, sess.Generation, summary, memoryPackJSON); err != nil {
		return fmt.Errorf("save summary: %w", err)
	}

	// Rebake Channel A: capture the current pinned snapshot so the new
	// generation starts with a hash matching what the fresh send will
	// include in its system prompt.
	var newHash string
	if b.memory != nil {
		_, newHash = b.memory.PinnedSnapshot(ctx, chatID)
	}
	newGen, err := b.store.BumpGeneration(chatID, threadID, newHash)
	if err != nil {
		return fmt.Errorf("bump generation: %w", err)
	}

	// Wipe the in-memory process session so the next send is treated as
	// fresh (no --resume, system prompt re-appended).
	key := process.SessionKey{ChatID: chatID, ThreadID: threadID}
	if agent := b.resolveAgent(chatID); agent != nil {
		if ps, _ := agent.Get(key); ps != nil {
			ps.HasHistory = false
			ps.ProviderSessionID = ""
		}
	}

	// Run a reflect cycle on the closed generation — the exchanges that
	// just ended are rich material for consolidation.
	if b.memory != nil {
		go b.memory.RunReflect(context.Background())
	}

	slog.Info("session rotated",
		"chat_id", chatID, "thread_id", threadID,
		"old_generation", sess.Generation, "new_generation", newGen,
		"summary_len", utf8.RuneCountInString(summary),
		"pack_bytes", len(memoryPackJSON),
	)
	return nil
}

// buildRotationSummary composes a compact "Previously in this chat" note
// from the tail of the conversation. Each exchange is trimmed so the
// summary fits in a few hundred tokens.
func (b *Bridge) buildRotationSummary(sess *store.Session) string {
	exchanges, err := b.store.RecentExchanges(sess.ID, rotationSummaryExchanges)
	if err != nil || len(exchanges) == 0 {
		return fmt.Sprintf("Generation %d closed at %s (no conversation history captured).",
			sess.Generation, time.Now().UTC().Format(time.RFC3339))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Generation %d ran from %s to %s (%d exchanges shown).\n\n",
		sess.Generation,
		sess.GenerationStartedAt.Format("2006-01-02 15:04"),
		time.Now().Format("2006-01-02 15:04"),
		len(exchanges)))
	for _, ex := range exchanges {
		u := truncateLine(ex.UserMessage, 160)
		r := truncateLine(ex.BotResponse, 200)
		if u == "" && r == "" {
			continue
		}
		if u != "" {
			sb.WriteString("- User: ")
			sb.WriteString(u)
			sb.WriteString("\n")
		}
		if r != "" {
			sb.WriteString("  Reply: ")
			sb.WriteString(r)
			sb.WriteString("\n")
		}
	}
	return strings.TrimSpace(sb.String())
}

// buildRotationMemoryPack queries ghost for the top-N non-pinned memories
// semantically relevant to the recent conversation, returning a JSON-
// serialized slice ready to store in session_summaries.memory_pack.
func (b *Bridge) buildRotationMemoryPack(ctx context.Context, chatID int64, query string) string {
	if b.memory == nil {
		return ""
	}
	pack := b.memory.RelevantMemoryPack(ctx, chatID, query, rotationMemoryPackBudget)
	if len(pack) == 0 {
		return ""
	}
	raw, err := json.Marshal(pack)
	if err != nil {
		slog.Warn("marshal memory pack failed", "error", err)
		return ""
	}
	return string(raw)
}

// truncateLine collapses a message to a single line and caps its length so
// rotation summaries don't balloon.
func truncateLine(s string, maxRunes int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "…"
}
