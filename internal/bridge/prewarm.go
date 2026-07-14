package bridge

import (
	"context"
	"log/slog"
	"time"

	"github.com/rcliao/shell/internal/store"
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
const prewarmPrompt = "[System cache warm-up after session rotation. For THIS message only, reply with exactly [noop] and do nothing else — no tools, no memory writes. This is not an example for future turns: real user messages always get real answers.]"

// Cache keep-alive (owner-approved 7/13, "#3"): the API prompt cache expires
// after ~1h idle, so a conversation gap past that pays a full context rebuild
// (measured: 71k-token re-creation, ~80s turn, 21s to first words). During
// waking hours, sessions with real recent activity get a tiny ping just
// before expiry so the cache never lapses. Costs a cache-read per ping
// (~$0.02-0.04); scoped to chats used in the last keepAliveActivityWindow.
const (
	keepAliveMinIdle        = 45 * time.Minute
	keepAliveMaxIdle        = 65 * time.Minute
	keepAliveActivityWindow = 8 * time.Hour
	keepAliveDayStart       = 7  // local hour, matches default quiet-hours end
	keepAliveDayEnd         = 22 // local hour, matches default quiet-hours start
)

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
			// Not due for rotation — consider a cache keep-alive instead.
			b.maybeKeepAlive(ctx, sess)
			continue
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

// WarmActiveSessionsAfterBoot re-warms sessions with recent real activity
// immediately after a daemon restart. A restart kills every persistent
// subprocess, so the next message per chat paid spawn + full API cache
// re-creation (~15-20s to first words — the most-active owner ate this after
// most of 7/13's seven deploys). Runs once, shortly after boot; busy
// sessions are skipped (a real turn is already doing the warming).
func (b *Bridge) WarmActiveSessionsAfterBoot(ctx context.Context) {
	if b.store == nil {
		return
	}
	sessions, err := b.store.ListPrewarmableSessions()
	if err != nil {
		return
	}
	for i := range sessions {
		sess := &sessions[i]
		if IsSystemChat(sess.ChatID) || sess.ProviderSessionID == "" {
			continue
		}
		lastUser, err := b.store.LastUserMessageAt(sess.ID)
		if err != nil || lastUser.IsZero() || time.Since(lastUser) > 2*time.Hour {
			continue // only chats in active use
		}
		start := time.Now()
		if _, err := b.HandleMessageStreaming(ctx, sess.ChatID, sess.MessageThreadID, prewarmPrompt, "prewarm", nil, nil, nil); err != nil {
			continue // busy = a real turn is warming it; other errors non-fatal
		}
		slog.Info("prewarm: post-boot session warm", "chat_id", sess.ChatID, "thread_id", sess.MessageThreadID,
			"secs", int(time.Since(start).Seconds()))
	}
}

// maybeKeepAlive pings a session whose prompt cache is about to expire, so
// the owner's next message lands warm instead of paying a full rebuild.
func (b *Bridge) maybeKeepAlive(ctx context.Context, sess *store.Session) {
	idle := time.Since(sess.UpdatedAt)
	if idle < keepAliveMinIdle || idle > keepAliveMaxIdle {
		return // cache still fresh, or already lapsed (rebuild is sunk cost)
	}
	loc, _ := time.LoadLocation(b.schedulerTZ)
	if loc == nil {
		loc = time.UTC
	}
	hour := time.Now().In(loc).Hour()
	if hour < keepAliveDayStart || hour >= keepAliveDayEnd {
		return // sleeping hours: let caches lapse, nobody is waiting
	}
	lastUser, err := b.store.LastUserMessageAt(sess.ID)
	if err != nil || lastUser.IsZero() || time.Since(lastUser) > keepAliveActivityWindow {
		return // no recent REAL activity — don't keep dead chats warm on pings
	}
	start := time.Now()
	if _, err := b.HandleMessageStreaming(ctx, sess.ChatID, sess.MessageThreadID, prewarmPrompt, "prewarm", nil, nil, nil); err != nil {
		slog.Warn("prewarm: keep-alive failed", "chat_id", sess.ChatID, "error", err)
		return
	}
	slog.Info("prewarm: cache keep-alive", "chat_id", sess.ChatID, "thread_id", sess.MessageThreadID,
		"idle_min", int(idle.Minutes()), "secs", int(time.Since(start).Seconds()))
}
