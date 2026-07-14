package bridge

import (
	"log/slog"
	"time"
)

// ActiveTurns reports how many HandleMessageStreaming calls are in flight.
func (b *Bridge) ActiveTurns() int {
	return int(b.turnCount.Load())
}

// WaitIdle blocks until all in-flight turns finish or the timeout elapses.
// Returns true if fully drained. Used by the SIGHUP restart path so a deploy
// never kills a turn mid-generation (the lost-reply incidents).
func (b *Bridge) WaitIdle(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		b.turnWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		slog.Warn("drain timeout — proceeding with turns still in flight",
			"active_turns", b.ActiveTurns(), "timeout", timeout)
		return false
	}
}
