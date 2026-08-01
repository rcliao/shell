package bridge

import (
	"log/slog"
	"time"
)

// ActiveTurns reports how many HandleMessageStreaming calls are in flight.
func (b *Bridge) ActiveTurns() int {
	return int(b.turnCount.Load())
}

// WaitIdle blocks until all in-flight work finishes or the timeout elapses.
// Returns true if fully drained. Used by the SIGHUP restart path so a deploy
// never kills a turn mid-generation (the lost-reply incidents).
//
// "Idle" has to mean the HUMAN HAS THEIR ANSWER, not that the bridge returned.
// turnWG is released inside HandleMessageStreaming, but the Telegram handler
// delivers the reply and calls CompletePendingTurn AFTER that — so a barrier on
// turnWG alone can report idle while a computed answer is still undelivered.
// That is exactly what happened on 2026-08-01 08:42:00.241, where drain declared
// idle in the same millisecond the session exited, the daemon exec'd, and an
// already-computed turn was replayed 39 seconds later in front of the family.
//
// So the barrier is two-phase: wait for turns to finish computing, then wait for
// the pending-turn ledger to have nothing recent left undone. The second phase
// is what covers delivery and completion.
func (b *Bridge) WaitIdle(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	done := make(chan struct{})
	go func() {
		b.turnWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		slog.Warn("drain timeout — proceeding with turns still in flight",
			"active_turns", b.ActiveTurns(), "timeout", timeout)
		return false
	}

	return b.waitDeliveries(deadline)
}

// deliveryPollInterval paces the second drain phase. Delivery is a Telegram
// round trip, so sub-second polling would just spin.
const deliveryPollInterval = 100 * time.Millisecond

// deliveryGraceWindow bounds which undone rows block a restart. A row older
// than this is a genuine failure from an earlier session — 6 undone rows from
// two weeks ago exist on the live agents today — and waiting on those would
// turn every deploy into a timeout.
const deliveryGraceWindow = 5 * time.Minute

// waitDeliveries blocks until no recently-received turn is still undelivered.
func (b *Bridge) waitDeliveries(deadline time.Time) bool {
	if b.store == nil {
		return true
	}
	for {
		pending, err := b.store.UndeliveredSince(deliveryGraceWindow)
		if err != nil {
			// Never let a ledger read failure block a restart; the pending-turn
			// replay path still covers anything genuinely in flight.
			slog.Warn("drain: undelivered check failed, proceeding", "error", err)
			return true
		}
		if pending == 0 {
			return true
		}
		if time.Now().After(deadline) {
			slog.Warn("drain timeout — proceeding with undelivered replies",
				"undelivered", pending)
			return false
		}
		time.Sleep(deliveryPollInterval)
	}
}
