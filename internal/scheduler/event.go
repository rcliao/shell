package scheduler

import (
	"encoding/json"
	"fmt"
	"time"
)

// Event-mode schedules (owner decision 2026-08-14, docs/PLAN-PROJECT-
// WORKSPACE.md "Event schedules").
//
// The third mode beside notify/prompt: a schedule not bound to chat delivery.
// On fire it enqueues its payload as a task-queue row and is done — no
// NotifyFunc/PromptFunc involved. Consumers own everything downstream,
// including any chat delivery. This makes the scheduler a cron front-end to
// the queue: a project's research fire, the Notion poll tick, and future cron
// monitors are all event schedules, and chat-bound schedules keep working
// unchanged.

// ModeEvent is the schedule mode that fires into the task queue.
const ModeEvent = "event"

// PauseInvalidEvent is the auto-pause reason for an event schedule whose
// message field does not parse as an event envelope. The same bytes will not
// parse on the next occurrence either, so retrying every fire forever would
// just hide the config error.
const PauseInvalidEvent = "invalid_event"

// eventFireTTL bounds how long an enqueued event stays worth starting.
// Generous on purpose: event consumers do background work (research passes,
// doc revisions) that is still worth doing hours late, unlike a reminder.
const eventFireTTL = 6 * time.Hour

// eventEnvelope is the contract for an event-mode schedule's message field:
// JSON {"kind": "<queue kind>", "payload": {...}}. The payload is opaque to
// the scheduler — it becomes the task's payload byte-for-byte.
type eventEnvelope struct {
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// ParseEventMessage validates and decodes an event schedule's message field.
// Exported so /schedule registration can reject a malformed envelope at
// create time instead of auto-pausing on the first fire.
func ParseEventMessage(message string) (kind, payload string, err error) {
	var env eventEnvelope
	if err := json.Unmarshal([]byte(message), &env); err != nil {
		return "", "", fmt.Errorf("event message must be JSON {\"kind\": ..., \"payload\": ...}: %w", err)
	}
	if env.Kind == "" {
		return "", "", fmt.Errorf("event message has no kind")
	}
	if len(env.Payload) == 0 {
		env.Payload = json.RawMessage("{}")
	}
	return env.Kind, string(env.Payload), nil
}

// eventPartitionKey names the serialization domain for an event's consumer
// turn. A payload carrying a chat binding serializes against that chat's live
// turns — a project revision must not race the conversation it belongs to.
// A payload without one gets a stable non-chat partition per kind, so e.g.
// poll ticks of one kind run in order without blocking anything else.
func eventPartitionKey(kind, payload string) string {
	var p struct {
		ChatID   int64 `json:"chat_id"`
		ThreadID int64 `json:"message_thread_id"`
	}
	if json.Unmarshal([]byte(payload), &p) == nil && p.ChatID != 0 {
		return PartitionKey(p.ChatID, p.ThreadID)
	}
	return "event:" + kind
}

// fireEvent executes one event-mode fire: enqueue the envelope's payload as a
// task of the envelope's kind. Idempotent on (kind, schedule id, occurrence),
// so a replayed or double-offered fire never double-enqueues while successive
// occurrences each enqueue their own row.
func (s *Scheduler) fireEvent(sc ScheduleEntry) (outcome, errMsg string, err error) {
	if s.queue == nil {
		// Without the durable queue there is nowhere for the event to live.
		// This is a wiring gap, not a transient failure — do not retry.
		return OutcomeSpawnFailed, "event mode requires the durable task queue", nil
	}
	kind, payload, perr := ParseEventMessage(sc.Message)
	if perr != nil {
		s.pause(sc.ID, PauseInvalidEvent)
		return OutcomeSpawnFailed, perr.Error(), nil
	}

	occurrence := sc.NextRunAt
	created, qerr := s.queue.EnqueueEvent(kind, payload, eventPartitionKey(kind, payload),
		sc.ID, occurrence, occurrence.Add(eventFireTTL))
	if qerr != nil {
		return OutcomeTurnFailed, qerr.Error(), qerr
	}
	if !created {
		// Same occurrence already enqueued — a replayed fire. Not an error,
		// and not a second event.
		return OutcomeFiredOK, "", nil
	}
	s.wakeWorkers()
	return OutcomeFiredOK, "", nil
}
