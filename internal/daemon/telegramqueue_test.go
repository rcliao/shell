package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

func queueTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// The whole point of the cutover: a turn the daemon died in the middle of gets
// answered by a worker on the next boot, delivered through the Telegram sink.
//
// Exercised end to end — ledger, reclaim, worker, sink — because the parts are
// individually testable and the SEAMS between them are where this would break.
func TestInterruptedTelegramTurnIsReplayedThroughTheSink(t *testing.T) {
	st := queueTestStore(t)
	const chatID, threadID, msgID = -100200300, 0, 4242

	// Boot 1 receives the message and starts answering it, then dies.
	dead := &queueTurnLedger{store: st, owner: "boot-1-111"}
	if done, err := dead.Begin(chatID, threadID, msgID, "someone", "did the parcel arrive?"); err != nil || done {
		t.Fatalf("begin: done=%v err=%v", done, err)
	}

	// Boot 2 comes up with a fresh owner and a worker.
	const owner = "boot-1-222"
	sched := scheduler.New(scheduler.NewStoreAdapter(st), nil, nil, "UTC")
	sched.SetQueue(scheduler.NewStoreAdapter(st), owner)

	var sent []string
	var sentChat, sentThread int64
	wireTelegramQueue(sched, func(c, th int64, text string) {
		sentChat, sentThread = c, th
		sent = append(sent, text)
	})

	var sawText string
	var sawAttempt int
	sched.EnableMessageTurns(func(ctx context.Context, m scheduler.MessageTurn, sink scheduler.Sink) (string, error) {
		sawText, sawAttempt = m.Text, m.Attempt
		if m.Attempt > 1 {
			// Mirrors the real runner, which appends the note itself.
			sawText += replayNote
		}
		reply := "yes, it arrived"
		return reply, sink.Finish(ctx, reply)
	})

	if n, err := st.ReclaimTasks(owner); err != nil || n != 1 {
		t.Fatalf("reclaim: n=%d err=%v", n, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sched.RunWorkers(ctx)

	deadline := time.Now().Add(10 * time.Second)
	for len(sent) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want exactly 1 — the interrupted turn was %s",
			len(sent), map[bool]string{true: "dropped", false: "duplicated"}[len(sent) == 0])
	}
	if sent[0] != "yes, it arrived" {
		t.Fatalf("delivered %q", sent[0])
	}
	if sentChat != chatID || sentThread != threadID {
		t.Fatalf("delivered to chat=%d thread=%d, want %d/%d", sentChat, sentThread, chatID, threadID)
	}
	if sawAttempt != 2 {
		t.Fatalf("replay ran as attempt %d, want 2", sawAttempt)
	}
	if !strings.Contains(sawText, "answered late") {
		t.Fatal("replayed turn was not told it is answering late; the agent will invent a reason for the gap")
	}
	if !strings.Contains(sawText, "did the parcel arrive?") {
		t.Fatalf("replay lost the original message: %q", sawText)
	}
}

// A turn that was ANSWERED before the crash must not be answered again. This is
// the failure the family would actually notice.
func TestAnsweredTelegramTurnIsNotReplayed(t *testing.T) {
	st := queueTestStore(t)
	const chatID, msgID = -100200300, 77

	first := &queueTurnLedger{store: st, owner: "boot-1-111"}
	if _, err := first.Begin(chatID, 0, msgID, "someone", "hello"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := first.Complete(chatID, msgID); err != nil {
		t.Fatalf("complete: %v", err)
	}

	const owner = "boot-1-222"
	if n, err := st.ReclaimTasks(owner); err != nil {
		t.Fatalf("reclaim: %v", err)
	} else if n != 0 {
		t.Fatalf("reclaimed %d completed turns; they would be answered twice", n)
	}
	if got, err := st.LeaseTask(owner, time.Minute); err != nil {
		t.Fatalf("lease: %v", err)
	} else if got != nil {
		t.Fatal("an already-answered turn was leased for replay")
	}

	// And Telegram redelivering the same update is recognised, not re-run.
	second := &queueTurnLedger{store: st, owner: owner}
	if done, err := second.Begin(chatID, 0, msgID, "someone", "hello"); err != nil || !done {
		t.Fatalf("redelivery: done=%v err=%v, want done=true", done, err)
	}
}

// Telegram numbers messages per chat, so the key must include the chat. Two
// people sending their nth message must not collide — the second would be
// silently dropped as a redelivery of the first.
func TestTurnKeysDoNotCollideAcrossChats(t *testing.T) {
	st := queueTestStore(t)
	l := &queueTurnLedger{store: st, owner: "boot-1-111"}

	if _, err := l.Begin(-100200300, 0, 5012, "a", "first"); err != nil {
		t.Fatalf("begin a: %v", err)
	}
	done, err := l.Begin(42, 0, 5012, "b", "second")
	if err != nil {
		t.Fatalf("begin b: %v", err)
	}
	if done {
		t.Fatal("a different chat's message 5012 was mistaken for a redelivery")
	}
	counts, err := st.CountTasksByState()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if counts[store.TaskLeased] != 2 {
		t.Fatalf("%d tasks recorded for two distinct messages, want 2", counts[store.TaskLeased])
	}
}
