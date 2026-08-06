package telegram

import (
	"testing"
	"time"
)

func TestShouldAbsorb(t *testing.T) {
	old := 25 * time.Second // past the 20s threshold
	cases := []struct {
		name       string
		enabled    bool
		sameSender bool
		hasMedia   bool
		answering  bool
		turnAge    time.Duration
		queueLen   int
		wantOK     bool
		wantReason string
	}{
		{"happy path", true, true, false, false, old, 1, true, ""},
		// The LATE race. Once the turn has started writing its reply, injecting
		// a question cannot change that reply — the message would be answered a
		// turn late, leaving every subsequent reply one behind. Production
		// 2026-08-06: three consecutive replies answered the previous message.
		{"turn already answering", true, true, false, true, old, 1, false, "already_answering"},
		{"disabled", false, true, false, false, old, 1, false, "disabled"},
		{"media", true, true, true, false, old, 1, false, "media"},
		{"different sender", true, false, false, false, old, 1, false, "different_sender"},
		{"turn too young", true, true, false, false, 5 * time.Second, 1, false, "turn_too_young"},
		{"exactly at threshold", true, true, false, false, absorbMinTurnAge, 1, true, ""},
		{"just under threshold", true, true, false, false, absorbMinTurnAge - time.Millisecond, 1, false, "turn_too_young"},
		{"multiple waiters go to H44", true, true, false, false, old, 2, false, "queue_not_single"},
		{"zero queue len", true, true, false, false, old, 0, false, "queue_not_single"},
		// Precedence: disabled wins over everything else.
		{"disabled wins", false, false, true, true, 0, 3, false, "disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := shouldAbsorb(tc.enabled, tc.sameSender, tc.hasMedia, tc.answering, tc.turnAge, tc.queueLen)
			if ok != tc.wantOK || reason != tc.wantReason {
				t.Errorf("shouldAbsorb() = (%v, %q), want (%v, %q)", ok, reason, tc.wantOK, tc.wantReason)
			}
		})
	}
}

func TestActiveTurnTracking(t *testing.T) {
	h := NewHandler(nil, nil, AgentConfig{AbsorbEnabled: true})
	key := chatLockKey{chatID: 100, threadID: 0}

	h.activeTurnsMu.Lock()
	_, ok := h.activeTurns[key]
	h.activeTurnsMu.Unlock()
	if ok {
		t.Fatal("no active turn expected before set")
	}

	h.setActiveTurn(key, 42)
	h.activeTurnsMu.Lock()
	at, ok := h.activeTurns[key]
	h.activeTurnsMu.Unlock()
	if !ok || at.senderID != 42 {
		t.Fatalf("expected active turn for sender 42, got ok=%v senderID=%d", ok, at.senderID)
	}
	if time.Since(at.startedAt) > time.Second {
		t.Errorf("startedAt should be ~now, got %v", at.startedAt)
	}

	h.clearActiveTurn(key)
	h.activeTurnsMu.Lock()
	_, ok = h.activeTurns[key]
	h.activeTurnsMu.Unlock()
	if ok {
		t.Fatal("active turn should be cleared")
	}
}

func TestTryAbsorb_NoActiveTurnInfo(t *testing.T) {
	// Lock held by an untracked turn (album/media/command) → refuse, waiter
	// stays on the normal queue path.
	h := NewHandler(nil, nil, AgentConfig{AbsorbEnabled: true})
	key := chatLockKey{chatID: 100}
	e := &queuedMsg{senderID: 42, text: "hi", msgID: 7}
	if h.tryAbsorbIntoActiveTurn(key, e, 1) {
		t.Fatal("must not absorb without active-turn info")
	}
}

func TestTryAbsorb_DisabledShortCircuits(t *testing.T) {
	h := NewHandler(nil, nil, AgentConfig{AbsorbEnabled: false})
	key := chatLockKey{chatID: 100}
	h.setActiveTurn(key, 42)
	e := &queuedMsg{senderID: 42, text: "hi", msgID: 7}
	if h.tryAbsorbIntoActiveTurn(key, e, 1) {
		t.Fatal("must not absorb when disabled")
	}
}
