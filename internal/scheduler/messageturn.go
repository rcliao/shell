package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
)

// Transport-agnostic message intake.
//
// A message is a unit of work described by (who said it, in which conversation,
// what it says). None of that is Telegram-specific. What IS transport-specific
// is how the message arrived and how the reply gets rendered — so the queue
// carries the transport NAME and the worker resolves a delivery sink from it.
//
// That split is the whole point. Telegram becomes one producer of message.turn
// rather than the message path, and a TUI or Discord client is a second
// producer with no changes here at all.
//
// The trap this avoids: making intake generic and quietly assuming delivery is
// too. Telegram streams a reply by editing a message in place; a terminal
// writes to stdout; a webhook posts once at the end. Those are not the same
// operation wearing different names, so the sink is an interface rather than a
// callback the queue pretends is universal.

// TaskKindMessageTurn is the kind for one inbound message from any transport.
const TaskKindMessageTurn = "message.turn"

// MessageTurn is the payload. Deliberately free of transport types: an ID is a
// string because Telegram numbers its messages and a TUI may not.
type MessageTurn struct {
	// Transport names the delivery sink: "telegram", "tui", ...
	Transport string `json:"transport"`
	// ExternalID is the transport's own id for this message. Together with
	// Transport it forms the idempotency key — a NATURAL key, because a
	// redelivered update carries the same id while two identical "ok" messages
	// minutes apart carry different ones. A content hash gets both backwards.
	ExternalID string `json:"external_id"`
	ChatID     int64  `json:"chat_id"`
	ThreadID   int64  `json:"thread_id"`
	SenderID   int64  `json:"sender_id"`
	SenderName string `json:"sender_name"`
	Text       string `json:"text"`
}

// Sink renders a reply back to wherever the message came from.
//
// Three calls rather than one because streaming is the reason the abstraction
// exists: the owner sees text appear as it is generated, and a sink that could
// only deliver a finished string would throw that away. A transport with no
// streaming implements Update as a no-op and does its work in Finish.
type Sink interface {
	// Update is called with the reply so far, repeatedly, as it is generated.
	Update(ctx context.Context, textSoFar string) error
	// Finish delivers the completed reply and any artifacts.
	Finish(ctx context.Context, final string) error
	// Fail reports that the turn could not be completed.
	Fail(ctx context.Context, cause error)
}

// SinkFactory builds a Sink for one message. Registered per transport.
type SinkFactory func(MessageTurn) (Sink, error)

type sinkRegistry struct {
	mu        sync.RWMutex
	factories map[string]SinkFactory
}

// RegisterTransport binds a transport name to its delivery sink.
//
// A transport that is configured but not registered is a wiring bug, and the
// worker fails such a task permanently rather than retrying — the same
// reasoning as an unregistered kind. Retrying cannot conjure a sink.
func (s *Scheduler) RegisterTransport(name string, f SinkFactory) {
	s.sinks.mu.Lock()
	defer s.sinks.mu.Unlock()
	if s.sinks.factories == nil {
		s.sinks.factories = map[string]SinkFactory{}
	}
	s.sinks.factories[name] = f
	slog.Info("worker: transport registered", "transport", name)
}

func (s *Scheduler) sinkFor(m MessageTurn) (Sink, error) {
	s.sinks.mu.RLock()
	f, ok := s.sinks.factories[m.Transport]
	s.sinks.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no delivery sink registered for transport %q", m.Transport)
	}
	return f(m)
}

// MessageTurnKey builds the idempotency key for an inbound message.
func MessageTurnKey(transport, externalID string) string {
	return transport + ":" + externalID
}

// EnableMessageTurns registers the message.turn handler. Producers enqueue;
// this runs them.
func (s *Scheduler) EnableMessageTurns(run MessageRunner) {
	s.runMessage = run
	s.RegisterHandler(TaskKindMessageTurn, s.handleMessageTurn)
}

// MessageRunner executes one message turn against the agent, streaming through
// the sink. It is the bridge's HandleMessageStreaming in transport-neutral
// clothing — the queue must not import the bridge.
type MessageRunner func(ctx context.Context, m MessageTurn, sink Sink) error

func (s *Scheduler) handleMessageTurn(ctx context.Context, t LeasedTask) (string, error) {
	var m MessageTurn
	if err := json.Unmarshal([]byte(t.Payload), &m); err != nil {
		return "", fmt.Errorf("undecodable message payload: %w", err)
	}
	if s.runMessage == nil {
		return "", fmt.Errorf("message turns enabled with no runner wired")
	}

	sink, err := s.sinkFor(m)
	if err != nil {
		// No sink means the reply has nowhere to go. Running the turn anyway
		// would burn a real inference and then discard the answer in front of
		// someone still waiting for it.
		return "", err
	}

	if t.Attempt > 1 {
		slog.Info("worker: replaying message turn after interruption",
			"task_id", t.ID, "transport", m.Transport, "external_id", m.ExternalID,
			"attempt", t.Attempt)
	}

	if err := s.runMessage(ctx, m, sink); err != nil {
		sink.Fail(ctx, err)
		return "", err
	}
	return "", nil
}
