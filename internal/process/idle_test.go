package process

import (
	"context"
	"testing"
	"time"
)

// The 2026-11-06 incident: a turn the bridge sent was still working when the
// idle timer fired at spawn+10m, and the process was killed mid-turn. A turn
// in flight must keep the process alive, before and after it starts streaming.
func TestIdleFireKeepsProcAliveDuringTurn(t *testing.T) {
	f := newFakeCLI(t)
	p := f.proc
	p.idleTimer = time.NewTimer(time.Hour) // stand-in; idleFire only re-arms it
	t.Cleanup(func() { p.idleTimer.Stop() })
	past := time.Now().Add(2 * idleTimeout)

	done := make(chan SendResult, 1)
	go func() { r, _ := f.send(context.Background()); done <- r }()
	time.Sleep(50 * time.Millisecond) // send has claimed its turn (waiter set), nothing streamed yet

	if p.idleFire(past) {
		t.Fatal("idle timer killed a process whose bridge turn was waiting for its first event")
	}

	// The turn starts streaming (tool use, text) and is still open.
	f.emit(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working"}]}}`)
	time.Sleep(50 * time.Millisecond)
	if p.idleFire(past) {
		t.Fatal("idle timer killed a process mid-turn")
	}

	f.emit(`{"type":"result","subtype":"success","result":"done","num_turns":1,"usage":{"input_tokens":1,"output_tokens":1}}`)
	if r := waitResult(t, done); r.StopReason != StopEndTurn {
		t.Fatalf("turn result = %+v", r)
	}

	// Turn over and nothing used it for idleTimeout: now it is idle.
	if !p.idleFire(past) {
		t.Fatal("idle timer must kill a process with no turn and no recent use")
	}
	if p.markUsed() {
		t.Fatal("a lookup after the idle kill decision must spawn fresh, not reuse")
	}
}

// A turn that never closes (caller gave up, tool wedged) must not keep the
// process alive forever: past maxTurnBusy the idle timer kills it.
func TestIdleFireKillsWedgedTurnAfterCeiling(t *testing.T) {
	f := newFakeCLI(t)
	p := f.proc
	p.idleTimer = time.NewTimer(time.Hour)
	t.Cleanup(func() { p.idleTimer.Stop() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := f.send(ctx); done <- err }()
	time.Sleep(50 * time.Millisecond)
	f.emit(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working"}]}}`)
	time.Sleep(50 * time.Millisecond)
	cancel() // caller abandons; turnOpen stays set, no result ever comes
	<-done

	start := time.Now()
	if p.idleFire(start) {
		t.Fatal("killed on the first busy fire")
	}
	if p.idleFire(start.Add(maxTurnBusy - time.Minute)) {
		t.Fatal("killed before the ceiling")
	}
	if !p.idleFire(start.Add(maxTurnBusy)) {
		t.Fatal("a turn wedged past maxTurnBusy must be killed")
	}
}

// A lookup that just handed the proc to a sender (who has not claimed a turn
// yet) must not lose it to a timer firing in that gap.
func TestIdleFireRespectsRecentLookup(t *testing.T) {
	f := newFakeCLI(t)
	p := f.proc
	p.idleTimer = time.NewTimer(time.Hour)
	t.Cleanup(func() { p.idleTimer.Stop() })

	if !p.markUsed() {
		t.Fatal("markUsed on a live proc must succeed")
	}
	if p.idleFire(time.Now()) {
		t.Fatal("idle timer killed a process looked up moments ago")
	}
	if !p.markUsed() {
		t.Fatal("proc kept alive by the re-arm must still be reusable")
	}
}
