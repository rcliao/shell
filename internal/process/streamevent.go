package process

// What a turn emits while it runs.
//
// The stream used to be `func(delta string)` — text and nothing else. Tool
// calls existed but were invisible until the turn ended and they appeared in
// SendResult.ToolCalls, so "what is it doing right now?" had no answer, and a
// caller that wanted to show tool activity live could not.
//
// Modelled on ACP's session/update variants (agent_message_chunk, tool_call,
// tool_call_update, usage_update). Our own types, because the bridge calls a Go
// interface rather than a protocol.
//
// A closed set: an interface with an unexported marker method, so a new event
// type cannot be introduced outside this package and every switch over events
// stays exhaustive by construction.
type StreamEvent interface{ isStreamEvent() }

// TextDelta is a chunk of assistant text as it is produced.
type TextDelta struct{ Text string }

// ToolStarted reports a tool call beginning. Input carries the tool's
// arguments as the parse layer already has them — passed through, not
// interpreted, because the stream layer has no business reading tool inputs.
type ToolStarted struct {
	ID    string
	Name  string
	Input map[string]any
}

// ToolFinished reports a tool call completing. Err is empty on success —
// mirroring the CLI's is_error flag rather than inventing an error type for a
// string we did not produce.
type ToolFinished struct {
	ID  string
	Err string
}

// UsageUpdate reports token accounting as it becomes known.
type UsageUpdate struct{ Usage Usage }

func (TextDelta) isStreamEvent()    {}
func (ToolStarted) isStreamEvent()  {}
func (ToolFinished) isStreamEvent() {}
func (UsageUpdate) isStreamEvent()  {}

// EventFunc receives stream events. Nil means the caller wants nothing.
type EventFunc func(StreamEvent)

// textOnly adapts an EventFunc down to the old text-delta callback.
//
// This is what keeps phase 2 additive: every existing caller passes a
// StreamFunc and keeps working, because the protocol layer now emits events
// and this collapses them back to text at the boundary. Callers migrate when
// they have a reason to, not because the signature changed under them.
func textOnly(fn StreamFunc) EventFunc {
	if fn == nil {
		return nil
	}
	return func(ev StreamEvent) {
		if d, ok := ev.(TextDelta); ok && d.Text != "" {
			fn(d.Text)
		}
	}
}
