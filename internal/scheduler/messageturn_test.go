package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rcliao/shell/internal/store"
)

type recordingSink struct {
	updates  []string
	finished string
	failed   error
}

func (r *recordingSink) Update(_ context.Context, s string) error {
	r.updates = append(r.updates, s)
	return nil
}
func (r *recordingSink) Finish(_ context.Context, s string) error { r.finished = s; return nil }
func (r *recordingSink) Fail(_ context.Context, e error)          { r.failed = e }

func enqueueMessage(t *testing.T, st *store.Store, m MessageTurn) int64 {
	t.Helper()
	payload, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := st.EnqueueTask(store.Task{
		Kind:           TaskKindMessageTurn,
		Source:         store.TaskSourceTelegram,
		IdempotencyKey: MessageTurnKey(m.Transport, m.ExternalID),
		PartitionKey:   PartitionKey(m.ChatID, m.ThreadID),
		Payload:        string(payload),
		MaxAttempts:    2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func msg(transport, id string, chat, thread int64, text string) MessageTurn {
	return MessageTurn{
		Transport: transport, ExternalID: id, ChatID: chat, ThreadID: thread,
		SenderID: 7, SenderName: "someone", Text: text,
	}
}

// The point of the whole design: a transport the queue has never heard of
// works by registering a sink, with no change to the kind, the payload or the
// worker. If a TUI needs anything else here, intake is not actually generic.
func TestAnyTransportWorksByRegisteringASink(t *testing.T) {
	q, st := queueAdapter(t)
	s := New(newMockStore(nil), nil, nil, "UTC")
	s.SetQueue(q, "boot-t")

	sink := &recordingSink{}
	s.RegisterTransport("tui", func(MessageTurn) (Sink, error) { return sink, nil })

	var ran MessageTurn
	s.EnableMessageTurns(func(_ context.Context, m MessageTurn, k Sink) error {
		ran = m
		if err := k.Update(context.Background(), "thinking"); err != nil {
			return err
		}
		return k.Finish(context.Background(), "done: "+m.Text)
	})

	id := enqueueMessage(t, st, msg("tui", "tui-1", 42, 0, "hello from a terminal"))
	if !s.leaseAndRunOne(context.Background()) {
		t.Fatal("worker found no work")
	}

	if ran.Text != "hello from a terminal" || ran.Transport != "tui" {
		t.Fatalf("runner got %+v", ran)
	}
	if len(sink.updates) != 1 || sink.finished != "done: hello from a terminal" {
		t.Fatalf("sink saw updates=%v finished=%q — streaming must reach the transport", sink.updates, sink.finished)
	}
	if got, _ := st.GetTask(id); got.State != store.TaskDone {
		t.Errorf("state = %q, want done", got.State)
	}
}

// A redelivered message must not produce a second reply to a human. The key is
// (transport, external_id), so the SAME message id is always the same work
// while two identical texts are not.
func TestRedeliveredMessageDoesNotRunTwice(t *testing.T) {
	_, st := queueAdapter(t)

	first := enqueueMessage(t, st, msg("telegram", "12366", -100200300, 2479, "ok"))
	again := enqueueMessage(t, st, msg("telegram", "12366", -100200300, 2479, "ok"))
	if first != again {
		t.Fatalf("a redelivered update created a second task (%d then %d) — the family would be answered twice", first, again)
	}

	// Same text, different message: genuinely new work.
	other := enqueueMessage(t, st, msg("telegram", "12367", -100200300, 2479, "ok"))
	if other == first {
		t.Fatal("two different messages with identical text collapsed into one task")
	}
}

// A transport with no registered sink has nowhere to put the reply. Running the
// turn anyway would burn a real inference and discard the answer in front of
// someone still waiting, so it fails without running.
func TestUnknownTransportFailsWithoutRunningTheTurn(t *testing.T) {
	q, st := queueAdapter(t)
	s := New(newMockStore(nil), nil, nil, "UTC")
	s.SetQueue(q, "boot-t")

	ran := false
	s.EnableMessageTurns(func(context.Context, MessageTurn, Sink) error { ran = true; return nil })

	id := enqueueMessage(t, st, msg("carrier-pigeon", "p-1", 42, 0, "hi"))
	if !s.leaseAndRunOne(context.Background()) {
		t.Fatal("worker found no work")
	}
	if ran {
		t.Error("the turn ran despite having nowhere to deliver the reply")
	}
	got, _ := st.GetTask(id)
	if got.State == store.TaskDone {
		t.Error("a message with no sink was marked done")
	}
}

// A failing turn tells the sink, so the person waiting learns something went
// wrong instead of watching silence.
func TestFailedTurnNotifiesTheSink(t *testing.T) {
	q, st := queueAdapter(t)
	s := New(newMockStore(nil), nil, nil, "UTC")
	s.SetQueue(q, "boot-t")

	sink := &recordingSink{}
	s.RegisterTransport("tui", func(MessageTurn) (Sink, error) { return sink, nil })
	boom := errors.New("model unreachable")
	s.EnableMessageTurns(func(context.Context, MessageTurn, Sink) error { return boom })

	enqueueMessage(t, st, msg("tui", "tui-9", 42, 0, "hi"))
	if !s.leaseAndRunOne(context.Background()) {
		t.Fatal("worker found no work")
	}
	if !errors.Is(sink.failed, boom) {
		t.Errorf("sink.failed = %v, want the turn error — silence is the worst outcome", sink.failed)
	}
}

// Messages in different forum topics own different subprocesses and must run
// concurrently; two in the same topic must not.
func TestMessagesPartitionByConversationNotChat(t *testing.T) {
	q, st := queueAdapter(t)
	const group int64 = -100200300

	enqueueMessage(t, st, msg("telegram", "1", group, 111, "topic A"))
	enqueueMessage(t, st, msg("telegram", "2", group, 222, "topic B"))
	enqueueMessage(t, st, msg("telegram", "3", group, 111, "topic A again"))

	a, err := q.LeaseNext("boot-A", 0)
	if err != nil || a == nil {
		t.Fatalf("lease 1 = %v, %v", a, err)
	}
	b, err := q.LeaseNext("boot-A", 0)
	if err != nil || b == nil {
		t.Fatal("two different topics must be leasable at once — they own separate subprocesses")
	}
	if c, _ := q.LeaseNext("boot-A", 0); c != nil {
		t.Error("a third message leased while both topics were busy")
	}
}
