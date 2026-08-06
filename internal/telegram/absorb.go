package telegram

import (
	"log/slog"
	"time"
)

// V2-H46: when a same-sender text message queues behind a turn that has
// already been running longer than absorbMinTurnAge, inject it into the
// in-flight turn as an addendum instead of waiting for the whole turn plus a
// fresh one (measured lock waits of 25-47s for single waiters; H44 coalescing
// never fires for them because it needs 2+ waiters in the queue).

// absorbMinTurnAge is the minimum age of the active turn before a waiter is
// absorbed into it. Young turns finish soon anyway, and very early injection
// races the turn's first inference.
const absorbMinTurnAge = 20 * time.Second

// activeTurnInfo describes the lock-holding turn for a (chat, thread) key.
type activeTurnInfo struct {
	senderID  int64
	startedAt time.Time
	// answering is set once the turn has put visible content in front of the
	// owner. After that the reply is already being composed and an injected
	// question CANNOT change it — see markTurnAnswering.
	answering bool
}

// setActiveTurn publishes this goroutine's turn as the active lock holder.
func (h *Handler) setActiveTurn(key chatLockKey, senderID int64) {
	h.activeTurnsMu.Lock()
	h.activeTurns[key] = activeTurnInfo{senderID: senderID, startedAt: time.Now()}
	h.activeTurnsMu.Unlock()
}

// markTurnAnswering records that the active turn has begun emitting its reply.
//
// This closes the LATE race, the mirror of the early one absorbMinTurnAge
// guards. Injecting a follow-up while the model is still thinking can change
// the answer; injecting after it has started writing cannot. The message lands
// in the session anyway, unanswered, and the model picks it up on the NEXT turn
// — which then answers the previous question while a new one arrives, leaving
// every reply one behind until a lull resets it.
//
// That happened in production on 2026-08-06: a follow-up was absorbed at turn
// age 35s into a turn whose first visible output was at 25.5s. Three
// consecutive replies answered the previous message — a question about hotel
// checkout was answered with a meal count.
func (h *Handler) markTurnAnswering(key chatLockKey) {
	h.activeTurnsMu.Lock()
	if at, ok := h.activeTurns[key]; ok && !at.answering {
		at.answering = true
		h.activeTurns[key] = at
	}
	h.activeTurnsMu.Unlock()
}

// clearActiveTurn removes the active-turn record (turn finished or failed).
func (h *Handler) clearActiveTurn(key chatLockKey) {
	h.activeTurnsMu.Lock()
	delete(h.activeTurns, key)
	h.activeTurnsMu.Unlock()
}

// shouldAbsorb is the pure eligibility decision for absorbing a queued waiter
// into the active turn. Returns (ok, skip-reason).
func shouldAbsorb(enabled, sameSender, hasMedia, answering bool, turnAge time.Duration, queueLen int) (bool, string) {
	switch {
	case !enabled:
		return false, "disabled"
	case answering:
		// The reply is already being written; injecting now cannot change it,
		// and the message would be answered a turn late — putting every
		// subsequent reply one behind. Fall through to the normal queue path,
		// which answers it in order.
		return false, "already_answering"
	case hasMedia:
		return false, "media"
	case !sameSender:
		return false, "different_sender"
	case queueLen != 1:
		// 2+ waiters is the H44 coalesce pattern — let the lock winner merge
		// them; absorbing one mid-turn would split the burst across replies.
		return false, "queue_not_single"
	case turnAge < absorbMinTurnAge:
		return false, "turn_too_young"
	}
	return true, ""
}

// tryAbsorbIntoActiveTurn attempts to inject a registered waiter into the
// lock-holding turn. Returns true when the injection landed — the caller
// must then stand down after the lock releases instead of running its own
// turn. On any refusal it logs "absorb: skipped" and the waiter proceeds on
// the normal queue/coalesce path.
func (h *Handler) tryAbsorbIntoActiveTurn(key chatLockKey, entry *queuedMsg, queueLen int) bool {
	if !h.absorbEnabled {
		slog.Info("absorb: skipped", "chat_id", key.chatID, "thread_id", key.threadID,
			"msg_id", entry.msgID, "reason", "disabled")
		return false
	}

	h.activeTurnsMu.Lock()
	at, running := h.activeTurns[key]
	h.activeTurnsMu.Unlock()
	if !running {
		// Lock is held by a turn we don't track (album/media turn, command,
		// reaction regenerate) — no sender/age info, so don't absorb into it.
		slog.Info("absorb: skipped", "chat_id", key.chatID, "thread_id", key.threadID,
			"msg_id", entry.msgID, "reason", "no_active_turn_info")
		return false
	}

	turnAge := time.Since(at.startedAt)
	ok, reason := shouldAbsorb(true, entry.senderID == at.senderID, false, at.answering, turnAge, queueLen)
	if !ok {
		slog.Info("absorb: skipped", "chat_id", key.chatID, "thread_id", key.threadID,
			"msg_id", entry.msgID, "reason", reason, "turn_age_s", int(turnAge.Seconds()))
		return false
	}

	age, err := h.bridge.InjectFollowUp(key.chatID, key.threadID, entry.text, entry.senderName)
	if err != nil {
		// Process-layer refusal (no tool in flight, turn just ended, one-shot
		// session, already injected) — all map to "not protocol-safe now".
		slog.Info("absorb: skipped", "chat_id", key.chatID, "thread_id", key.threadID,
			"msg_id", entry.msgID, "reason", "protocol_unsupported", "detail", err.Error(),
			"turn_age_s", int(turnAge.Seconds()))
		return false
	}

	// Remove the entry from the coalesce queue so neither a sibling waiter
	// nor the next lock winner double-answers it.
	h.coalesceMu.Lock()
	q := h.coalesceQueues[key]
	remaining := q[:0]
	for _, e := range q {
		if e != entry {
			remaining = append(remaining, e)
		}
	}
	h.coalesceQueues[key] = remaining
	h.coalesceMu.Unlock()

	slog.Info("absorb: injected into active turn", "chat_id", key.chatID,
		"thread_id", key.threadID, "msg_id", entry.msgID, "turn_age_s", int(age.Seconds()))
	return true
}
