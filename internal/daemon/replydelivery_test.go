package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

var errSendTimeout = errors.New("context deadline exceeded (Client.Timeout exceeded while awaiting headers)")

// A replayed reply whose send fails is re-sent, not given up on: the replay is
// the turn's last attempt, so the send is the only thing left to retry.
func TestReplaySinkRetriesUntilDelivered(t *testing.T) {
	calls := 0
	sink := &telegramSink{
		chatID: -100200300, thread: 0,
		retryDelays: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
		send: func(_, _ int64, _ string) error {
			calls++
			if calls < 3 {
				return errSendTimeout
			}
			return nil
		},
	}
	if err := sink.Finish(context.Background(), "the answer"); err != nil {
		t.Fatalf("Finish = %v after the third send landed", err)
	}
	if calls != 3 {
		t.Fatalf("sent %d times, want 3", calls)
	}
}

// When every send fails, Finish says so. It used to return nil, which marked
// the task done with the answer never delivered.
func TestReplaySinkReportsUndeliveredReply(t *testing.T) {
	calls := 0
	sink := &telegramSink{
		retryDelays: []time.Duration{time.Millisecond, time.Millisecond},
		send:        func(_, _ int64, _ string) error { calls++; return errSendTimeout },
	}
	err := sink.Finish(context.Background(), "the answer")
	if !errors.Is(err, errSendTimeout) {
		t.Fatalf("Finish = %v, want the send error", err)
	}
	if calls != 3 {
		t.Fatalf("sent %d times, want 1 + 2 retries", calls)
	}
}

// A cancelled worker stops waiting between retries.
func TestReplaySinkStopsRetryingOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sink := &telegramSink{
		retryDelays: []time.Duration{time.Hour},
		send:        func(_, _ int64, _ string) error { cancel(); return errSendTimeout },
	}
	done := make(chan error, 1)
	go func() { done <- sink.Finish(ctx, "the answer") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled Finish reported the reply delivered")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Finish kept waiting after its context was cancelled")
	}
}

// End to end, the 2026-11-09 incident: an interrupted turn is replayed, the
// answer is written, and every send of it times out. The task must end failed,
// not done — "done" is what hid the lost reply.
func TestReplayWithUndeliveredReplyIsNotMarkedDone(t *testing.T) {
	saved := replyRetryDelays
	replyRetryDelays = []time.Duration{time.Millisecond}
	t.Cleanup(func() { replyRetryDelays = saved })

	st := queueTestStore(t)
	const chatID, threadID, msgID = -100200300, 0, 4343
	dead := &queueTurnLedger{store: st, owner: "boot-1-111"}
	if _, err := dead.Begin(chatID, threadID, msgID, "someone", "are we still on for dinner?"); err != nil {
		t.Fatal(err)
	}
	const owner = "boot-1-222"
	sched := scheduler.New(scheduler.NewStoreAdapter(st), nil, nil, "UTC")
	sched.SetQueue(scheduler.NewStoreAdapter(st), owner)

	var mu sync.Mutex
	sends := 0
	wireTelegramQueue(sched, func(_, _ int64, _ string) error {
		mu.Lock()
		sends++
		mu.Unlock()
		return errSendTimeout
	})
	sched.EnableMessageTurns(func(ctx context.Context, m scheduler.MessageTurn, sink scheduler.Sink) (string, error) {
		if err := sink.Finish(ctx, "yes, 7pm"); err != nil {
			return "", err
		}
		return "yes, 7pm", nil
	})
	if _, err := st.ReclaimTasks(owner); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sched.RunWorkers(ctx)

	key := telegramTurnKey(chatID, msgID)
	var task *store.Task
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		if task, err = st.GetTaskByIdempotencyKey(key); err != nil {
			t.Fatal(err)
		}
		if task.State == store.TaskDone || task.State == store.TaskFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	if task.State != store.TaskFailed {
		t.Fatalf("task state %q, want %q: an undelivered reply was recorded as answered", task.State, store.TaskFailed)
	}
	mu.Lock()
	defer mu.Unlock()
	if sends != 2 {
		t.Fatalf("send tried %d times, want 2 (first send + one retry)", sends)
	}
}
