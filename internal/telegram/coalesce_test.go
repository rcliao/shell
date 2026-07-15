package telegram

import (
	"strings"
	"testing"
	"time"
)

func newCoalesceHandler() *Handler {
	return &Handler{coalesceQueues: make(map[chatLockKey][]*queuedMsg)}
}

func TestAbsorbQueuedMergesSameSender(t *testing.T) {
	h := newCoalesceHandler()
	key := chatLockKey{chatID: 1, threadID: 0}
	winner := &queuedMsg{senderID: 7, msgID: 10, text: "first"}
	b := &queuedMsg{senderID: 7, msgID: 11, text: "second"}
	c := &queuedMsg{senderID: 7, msgID: 12, text: "third"}
	other := &queuedMsg{senderID: 99, msgID: 13, text: "different sender"}
	h.coalesceQueues[key] = []*queuedMsg{winner, b, c, other}

	consumed, absorbed := h.absorbQueued(key, winner, 7)
	if consumed {
		t.Fatal("winner must not be consumed")
	}
	if len(absorbed) != 2 || absorbed[0].msgID != 11 || absorbed[1].msgID != 12 {
		t.Fatalf("expected b,c absorbed in order, got %+v", absorbed)
	}
	if !b.consumed || !c.consumed {
		t.Fatal("absorbed entries must be marked consumed")
	}
	if q := h.coalesceQueues[key]; len(q) != 1 || q[0] != other {
		t.Fatalf("other-sender entry must remain queued, got %+v", q)
	}
}

func TestAbsorbQueuedConsumedStandsDown(t *testing.T) {
	h := newCoalesceHandler()
	key := chatLockKey{chatID: 1}
	e := &queuedMsg{senderID: 7, msgID: 11, consumed: true}
	consumed, absorbed := h.absorbQueued(key, e, 7)
	if !consumed || absorbed != nil {
		t.Fatalf("consumed waiter must stand down, got consumed=%v absorbed=%v", consumed, absorbed)
	}
}

func TestAbsorbQueuedCap(t *testing.T) {
	h := newCoalesceHandler()
	key := chatLockKey{chatID: 1}
	winner := &queuedMsg{senderID: 7, msgID: 1}
	q := []*queuedMsg{winner}
	for i := 2; i <= 8; i++ {
		q = append(q, &queuedMsg{senderID: 7, msgID: i, enqueued: time.Now()})
	}
	h.coalesceQueues[key] = q
	_, absorbed := h.absorbQueued(key, winner, 7)
	if len(absorbed) != 4 {
		t.Fatalf("cap is 4 absorbed (5 total incl winner), got %d", len(absorbed))
	}
	if remaining := h.coalesceQueues[key]; len(remaining) != 3 {
		t.Fatalf("overflow must stay queued, got %d", len(remaining))
	}
}

func TestAbsorbQueuedNilSelf(t *testing.T) {
	h := newCoalesceHandler()
	consumed, absorbed := h.absorbQueued(chatLockKey{}, nil, 7)
	if consumed || absorbed != nil {
		t.Fatal("nil self must be a no-op")
	}
}

func TestCoalesceText(t *testing.T) {
	out := coalesceText("main question", []*queuedMsg{{text: "follow-up A"}, {text: "follow-up B"}})
	for _, want := range []string{"main question", "1. follow-up A", "2. follow-up B", "一併回答"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}
