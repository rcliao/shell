package scheduler

import (
	"context"
	"testing"
	"time"
)

// eventEntry builds a due event-mode schedule whose envelope binds a chat.
// Synthetic IDs only.
func eventEntry(id int64, message string) ScheduleEntry {
	return ScheduleEntry{
		ID: id, ChatID: 0, Label: "research", Message: message,
		Schedule: "0 9 * * 1", Timezone: "UTC", Type: "cron", Mode: ModeEvent,
		NextRunAt: time.Now().UTC().Add(-time.Minute),
	}
}

// An event-mode fire enqueues exactly one task-queue row carrying the
// envelope's kind and payload — and touches neither NotifyFunc nor PromptFunc,
// which is the whole point of the mode.
func TestEventFireEnqueuesTaskNotChatDelivery(t *testing.T) {
	q, st := queueAdapter(t)

	notified, prompted := false, false
	s := New(newMockStore(nil),
		func(chatID int64, msg string) { notified = true },
		func(ctx context.Context, chatID int64, msg string) error { prompted = true; return nil },
		"UTC")
	s.SetQueue(q, "boot-A")

	entry := eventEntry(11, `{"kind":"project.event","payload":{"event":"research.due","project":"synthetic-slug","chat_id":42,"message_thread_id":7}}`)
	outcome, errMsg, err := s.execute(context.Background(), entry, 0)
	if err != nil || outcome != OutcomeFiredOK {
		t.Fatalf("execute = (%s, %q, %v), want fired_ok", outcome, errMsg, err)
	}
	if notified || prompted {
		t.Fatalf("event fire reached chat delivery (notified=%v prompted=%v) — it must only enqueue", notified, prompted)
	}

	tasks, err := st.ListTasks("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want exactly 1", len(tasks))
	}
	task := tasks[0]
	if task.Kind != "project.event" {
		t.Errorf("kind = %q, want project.event", task.Kind)
	}
	if task.Payload != `{"event":"research.due","project":"synthetic-slug","chat_id":42,"message_thread_id":7}` {
		t.Errorf("payload did not pass through byte-for-byte: %s", task.Payload)
	}
	// A payload with a chat binding serializes against that chat's turns.
	if task.PartitionKey != PartitionKey(42, 7) {
		t.Errorf("partition = %q, want %q", task.PartitionKey, PartitionKey(42, 7))
	}
	if task.IdempotencyKey == "" {
		t.Error("task has no idempotency key")
	}
	if task.ExpiresAt == nil {
		t.Error("event task must carry a generous expiry")
	}
}

// Retries and replays of the SAME occurrence must not double-enqueue; a later
// occurrence must enqueue its own row. The occurrence time is exactly what
// distinguishes the two — same house rule as schedule.fire idempotency.
func TestEventFireIdempotentPerOccurrence(t *testing.T) {
	q, st := queueAdapter(t)
	s := New(newMockStore(nil), nil, nil, "UTC")
	s.SetQueue(q, "boot-A")

	entry := eventEntry(12, `{"kind":"project.event","payload":{"event":"research.due","project":"synthetic-slug"}}`)

	for i := 0; i < 2; i++ {
		outcome, errMsg, err := s.execute(context.Background(), entry, 0)
		if err != nil || outcome != OutcomeFiredOK {
			t.Fatalf("fire %d = (%s, %q, %v)", i+1, outcome, errMsg, err)
		}
	}
	tasks, err := st.ListTasks("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("double-fire of one occurrence produced %d tasks, want 1", len(tasks))
	}

	// The NEXT occurrence is new work and must enqueue.
	entry.NextRunAt = entry.NextRunAt.Add(7 * 24 * time.Hour)
	if outcome, _, err := s.execute(context.Background(), entry, 0); err != nil || outcome != OutcomeFiredOK {
		t.Fatalf("next occurrence fire failed: %s %v", outcome, err)
	}
	tasks, _ = st.ListTasks("", 10)
	if len(tasks) != 2 {
		t.Fatalf("successive occurrences produced %d tasks, want 2", len(tasks))
	}
}

// An envelope without a chat binding falls back to a stable non-chat
// partition, so unrelated events of one kind run in order without ever
// contending with a conversation.
func TestEventPartitionFallsBackToKind(t *testing.T) {
	q, st := queueAdapter(t)
	s := New(newMockStore(nil), nil, nil, "UTC")
	s.SetQueue(q, "boot-A")

	entry := eventEntry(13, `{"kind":"notion.poll.tick","payload":{"cursor":"abc"}}`)
	if outcome, _, _ := s.execute(context.Background(), entry, 0); outcome != OutcomeFiredOK {
		t.Fatalf("outcome = %s", outcome)
	}
	tasks, _ := st.ListTasks("", 10)
	if len(tasks) != 1 || tasks[0].PartitionKey != "event:notion.poll.tick" {
		t.Fatalf("tasks = %+v, want one row partitioned event:notion.poll.tick", tasks)
	}
}

// A malformed envelope is a config error, not a transient failure: the same
// bytes will never parse. The schedule auto-pauses instead of retrying every
// occurrence forever, mirroring invalid_cron.
func TestEventFireBadEnvelopeAutoPauses(t *testing.T) {
	q, _ := queueAdapter(t)
	ms := newMockStore(nil)
	s := New(ms, nil, nil, "UTC")
	s.SetQueue(q, "boot-A")

	entry := eventEntry(14, `remind the user about the thing`) // not an envelope
	outcome, _, err := s.execute(context.Background(), entry, 0)
	if err != nil {
		t.Fatalf("parse failure must not be retryable: %v", err)
	}
	if outcome != OutcomeSpawnFailed {
		t.Errorf("outcome = %s, want spawn_failed", outcome)
	}
	if ms.pauseReasons[14] != PauseInvalidEvent {
		t.Errorf("pause reason = %q, want %q", ms.pauseReasons[14], PauseInvalidEvent)
	}
}

// Without the durable queue there is nowhere for an event to live — the fire
// reports a wiring failure rather than pretending to succeed.
func TestEventFireWithoutQueueFails(t *testing.T) {
	s := New(newMockStore(nil), nil, nil, "UTC")
	entry := eventEntry(15, `{"kind":"project.event"}`)
	outcome, errMsg, err := s.execute(context.Background(), entry, 0)
	if err != nil || outcome != OutcomeSpawnFailed || errMsg == "" {
		t.Fatalf("execute = (%s, %q, %v), want spawn_failed with a reason", outcome, errMsg, err)
	}
}

// ParseEventMessage defaults a missing payload to {} so consumers can rely on
// valid JSON, and rejects a missing kind — the field that selects the handler.
func TestParseEventMessage(t *testing.T) {
	kind, payload, err := ParseEventMessage(`{"kind":"project.event"}`)
	if err != nil || kind != "project.event" || payload != "{}" {
		t.Errorf("ParseEventMessage = (%q, %q, %v)", kind, payload, err)
	}
	if _, _, err := ParseEventMessage(`{"payload":{}}`); err == nil {
		t.Error("missing kind must be rejected")
	}
	if _, _, err := ParseEventMessage(`not json`); err == nil {
		t.Error("non-JSON must be rejected")
	}
}
