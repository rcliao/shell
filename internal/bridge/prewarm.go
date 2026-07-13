package bridge

import (
	"context"
	"log/slog"
	"time"
)

// Eager rotation + cache pre-warm (V2-H33 speed program).
//
// Rotation used to be lazy: the flag (or day boundary) sat until the owner's
// next message, which then paid the whole bill — kill subprocess, write the
// summary, spawn fresh, and re-create the full prompt cache (~40-90s
// observed). PrewarmDueSessions moves that bill to a background tick: when a
// session is due to rotate AND has been idle long enough that no conversation
// is in flight, it rotates now and immediately runs a tiny warm-up turn so
// cache creation happens before any human notices. The owner's next message
// lands on a warm generation (~6-9s to first token instead of 60-120s).

// prewarmPrompt is the warm-up turn body. The [noop] contract keeps output
// minimal; the point of the turn is the system-prompt cache write, not the
// reply. The response is discarded by the caller and never delivered.
const prewarmPrompt = "[System cache warm-up after session rotation. Reply with exactly [noop] and do nothing else — no tools, no memory writes.]"

// PrewarmDueSessions rotates and warms every idle session that is due.
// Called from the daemon's maintenance loop. idle guards against rotating
// mid-conversation; sessions touched more recently than that are left for
// the lazy path.
func (b *Bridge) PrewarmDueSessions(ctx context.Context, idle time.Duration) {
	if b.store == nil {
		return
	}
	sessions, err := b.store.ListPrewarmableSessions()
	if err != nil {
		slog.Warn("prewarm: list sessions failed", "error", err)
		return
	}
	for i := range sessions {
		sess := &sessions[i]
		if IsSystemChat(sess.ChatID) {
			continue // heartbeat chat is ephemeral/rebuilt on its own cadence
		}
		if sess.ProviderSessionID == "" {
			continue // never had a first turn — nothing to rotate or warm
		}
		if time.Since(sess.UpdatedAt) < idle {
			continue // conversation may be live; lazy path handles it
		}
		age := time.Since(sess.GenerationStartedAt)
		dayChanged := age >= rotationMinAge && b.calendarDayChanged(sess.GenerationStartedAt)
		if !sess.RotatePending && age < rotationMaxAge && !dayChanged {
			continue // not due
		}

		reason := "eager_" // prefix distinguishes background rotations in logs
		switch {
		case sess.RotatePending:
			if sess.RotateReason != "" {
				reason += sess.RotateReason
			} else {
				reason += "pending"
			}
		case age >= rotationMaxAge:
			reason += "age"
		default:
			reason += "day_boundary"
		}

		if err := b.rotateSession(ctx, sess.ChatID, sess.MessageThreadID, sess, reason); err != nil {
			slog.Warn("prewarm: rotation failed", "chat_id", sess.ChatID, "error", err)
			continue
		}

		start := time.Now()
		if _, err := b.HandleMessageStreaming(ctx, sess.ChatID, sess.MessageThreadID, prewarmPrompt, "prewarm", nil, nil, nil); err != nil {
			// Busy or transient — the generation is rotated either way; the
			// next tick (or the owner's turn) creates the cache instead.
			slog.Warn("prewarm: warm-up turn failed", "chat_id", sess.ChatID, "error", err)
			continue
		}
		slog.Info("prewarm: session rotated and cache warmed",
			"chat_id", sess.ChatID, "thread_id", sess.MessageThreadID,
			"reason", reason, "warmup_secs", int(time.Since(start).Seconds()))
	}
}
