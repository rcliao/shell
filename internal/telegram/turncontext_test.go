package telegram

import (
	"context"
	"testing"
	"time"
)

// The 2026-08-08 incident: drain cancelled the poller context to stop fetching
// updates, and the library hands that same context to handlers — so the reply
// to a message already being answered failed its final edit with "context
// canceled", leaving "Thinking..." on the owner's screen forever.
//
// A turn must survive the poller being stopped. Intake and delivery have
// different lifetimes, and conflating them makes drain destroy the very turns
// it is waiting for.
func TestTurnSurvivesThePollerBeingStopped(t *testing.T) {
	pollCtx, pollCancel := context.WithCancel(context.Background())
	turn := turnContext(pollCtx)

	pollCancel() // drain: stop fetching new updates

	if err := turn.Err(); err != nil {
		t.Fatalf("turn context died with the poller (%v) — the reply cannot be delivered", err)
	}
	select {
	case <-turn.Done():
		t.Fatal("turn context reported Done after the poller stopped")
	default:
	}
}

// Detaching cancellation must not throw away values: the library and handlers
// pass request-scoped data through this context, and silently dropping it would
// trade one bug for a subtler one.
func TestTurnContextKeepsValues(t *testing.T) {
	type ctxKey string
	const k ctxKey = "bot"

	base := context.WithValue(context.Background(), k, "pikamini")
	turn := turnContext(base)

	if got, _ := turn.Value(k).(string); got != "pikamini" {
		t.Fatalf("value lost through turnContext: got %q", got)
	}
}

// A deadline set by the poller must not silently outlive it either — detaching
// is about cancellation, and a turn that inherited a poll timeout would be cut
// off just as arbitrarily.
func TestTurnContextDropsThePollerDeadline(t *testing.T) {
	base, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	turn := turnContext(base)

	time.Sleep(30 * time.Millisecond)

	if err := turn.Err(); err != nil {
		t.Fatalf("turn expired on the poller's deadline (%v)", err)
	}
	if _, ok := turn.Deadline(); ok {
		t.Fatal("turn inherited the poller's deadline")
	}
}
