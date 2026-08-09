package bridge

import (
	"errors"
	"testing"
	"time"
)

// fakeLedger records what drain asked it, so a test can prove the question was
// asked at all rather than only that the answer was handled.
type fakeLedger struct {
	undelivered int
	err         error
	askedWith   time.Duration
	asked       int
}

func (f *fakeLedger) Begin(int64, int64, int, string, string) (bool, error) { return false, nil }
func (f *fakeLedger) Complete(int64, int) error                             { return nil }
func (f *fakeLedger) Abandon(int64, int) error                              { return nil }
func (f *fakeLedger) Undelivered(maxAge time.Duration) (int, error) {
	f.asked++
	f.askedWith = maxAge
	return f.undelivered, f.err
}

// The regression this guards: drain's delivery barrier read pending_turns while
// the queue cutover had moved writes to the task ledger, so it counted 0 forever
// and passed instantly. The barrier must ask whichever ledger is recording.
func TestDrainAsksTheActiveLedgerNotTheRawStore(t *testing.T) {
	led := &fakeLedger{undelivered: 3}
	b := &Bridge{}
	b.SetTurnLedger(led)

	n, err := b.UndeliveredTurns(deliveryGraceWindow)
	if err != nil {
		t.Fatalf("undelivered: %v", err)
	}
	if led.asked == 0 {
		t.Fatal("drain never asked the active ledger — it is reading past it, which is the original bug")
	}
	if n != 3 {
		t.Fatalf("undelivered = %d, want 3 from the active ledger", n)
	}
	if led.askedWith != deliveryGraceWindow {
		t.Fatalf("asked with maxAge %v, want the grace window %v", led.askedWith, deliveryGraceWindow)
	}
}

// With work outstanding the barrier must actually block, and give up only at the
// deadline. A barrier that returns true immediately is the bug wearing a fix.
func TestWaitDeliveriesBlocksWhileTurnsAreUndelivered(t *testing.T) {
	b := &Bridge{}
	b.SetTurnLedger(&fakeLedger{undelivered: 1})

	start := time.Now()
	drained := b.waitDeliveries(start.Add(300 * time.Millisecond))
	waited := time.Since(start)

	if drained {
		t.Fatal("drain reported success while a reply was still undelivered")
	}
	if waited < 250*time.Millisecond {
		t.Fatalf("returned after %v — it did not wait for the deadline", waited)
	}
}

// The barrier must clear as soon as nothing is outstanding, or every deploy
// pays the full timeout.
func TestWaitDeliveriesReturnsWhenNothingIsOutstanding(t *testing.T) {
	b := &Bridge{}
	b.SetTurnLedger(&fakeLedger{undelivered: 0})

	start := time.Now()
	if !b.waitDeliveries(start.Add(5 * time.Second)) {
		t.Fatal("barrier blocked with nothing undelivered")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("took %v to notice an empty ledger", waited)
	}
}

// A ledger read failure must not wedge deploys. Losing the barrier is bad;
// making the daemon un-restartable is worse, and replay still covers the turn.
func TestWaitDeliveriesProceedsWhenTheLedgerErrors(t *testing.T) {
	b := &Bridge{}
	b.SetTurnLedger(&fakeLedger{undelivered: 9, err: errors.New("db is gone")})

	if !b.waitDeliveries(time.Now().Add(5 * time.Second)) {
		t.Fatal("a ledger read error blocked the restart instead of proceeding")
	}
}
