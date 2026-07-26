package process

import (
	"errors"
	"io"
	"sync"
	"time"
)

// V2-H46 mid-turn injection: absorb a queued same-sender follow-up into the
// in-flight turn by writing an additional {"type":"user"} line to the live
// subprocess stdin instead of waiting for the full prior turn.
//
// WHY the tool-in-flight gate (measured against claude CLI 2.1.212,
// stream-json bidirectional mode, 2026-07-26):
//   - Injected while a tool call was executing → the CLI queued the message
//     and ABSORBED it at the post-tool inference step: ONE result event whose
//     text answered both messages (num_turns=2). The read loop
//     (parseBidirectionalEventsObserved returns at the first result) handles
//     this transparently.
//   - Injected during a no-tool inference (model streaming its answer) → the
//     CLI ran the message as a SECOND turn and emitted TWO result events. The
//     read loop returns at the first, so the second would sit unread in the
//     pipe and be mis-parsed as the NEXT turn's output — a mangled reply.
// A pending tool_result guarantees at least one more inference step in the
// current turn, so injection while toolsInflight > 0 always lands in the
// absorb path. With no tool in flight we refuse (ErrInjectNoToolInflight)
// and the caller falls back to normal queueing.

var (
	// ErrInjectNoProc — no live persistent subprocess for the session key.
	ErrInjectNoProc = errors.New("inject: no persistent process")
	// ErrInjectNoTurn — the subprocess exists but no turn is in flight.
	ErrInjectNoTurn = errors.New("inject: no active turn")
	// ErrInjectNoToolInflight — mid-inference, not mid-tool: injection would
	// start a second turn and orphan its result (see package comment above).
	ErrInjectNoToolInflight = errors.New("inject: no tool call in flight")
	// ErrInjectAlready — one injection per turn; further follow-ups queue.
	ErrInjectAlready = errors.New("inject: turn already absorbed a follow-up")
)

// Injector is implemented by agents that can absorb a user message into an
// in-flight turn (V2-H46). Kept out of the Agent interface so test doubles
// and alternative agents don't have to implement it; callers type-assert.
type Injector interface {
	// InjectUserText writes text as an additional user message to the live
	// turn. Returns the age of the active turn (0 when unknown) and an error
	// naming the refusal reason when injection is not protocol-safe.
	InjectUserText(key SessionKey, text string) (time.Duration, error)
}

// syncWriter serializes writes to the subprocess stdin. During a turn the
// parse loop answers control_requests on stdin while the injector may write a
// user message from another goroutine; pipe writes above PIPE_BUF are not
// atomic, so unsynchronized JSON lines could interleave mid-line.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// turnState tracks the live turn on a persistent process. It has its own
// mutex because sendMessage holds proc.mu for the entire turn — the injector
// must be able to inspect and act without that lock.
type turnState struct {
	mu            sync.Mutex
	active        bool
	startedAt     time.Time
	toolsInflight int
	injected      int
}

func (t *turnState) begin() {
	t.mu.Lock()
	t.active = true
	t.startedAt = time.Now()
	t.toolsInflight = 0
	t.injected = 0
	t.mu.Unlock()
}

func (t *turnState) end() {
	t.mu.Lock()
	t.active = false
	t.toolsInflight = 0
	t.mu.Unlock()
}

// toolStarted / toolEnded implement turnObserver (called from the parse loop).
func (t *turnState) toolStarted() {
	t.mu.Lock()
	t.toolsInflight++
	t.mu.Unlock()
}

func (t *turnState) toolEnded() {
	t.mu.Lock()
	if t.toolsInflight > 0 {
		t.toolsInflight--
	}
	t.mu.Unlock()
}

// injectUserText writes text into the in-flight turn if protocol-safe.
// The eligibility check and the stdin write happen under turn.mu so the
// tool-in-flight state cannot flip between check and write; the parse loop's
// toolEnded blocks for at most one small pipe write.
func (p *persistentProc) injectUserText(text string) (time.Duration, error) {
	p.turn.mu.Lock()
	defer p.turn.mu.Unlock()
	if !p.turn.active {
		return 0, ErrInjectNoTurn
	}
	age := time.Since(p.turn.startedAt)
	if p.turn.toolsInflight <= 0 {
		return age, ErrInjectNoToolInflight
	}
	if p.turn.injected >= 1 {
		return age, ErrInjectAlready
	}
	if err := writeJSON(p.stdinW, newTextUserMessage(text, p.sessionID)); err != nil {
		return age, err
	}
	p.turn.injected++
	return age, nil
}

// InjectUserText implements Injector for the CLI-subprocess manager.
func (m *Manager) InjectUserText(key SessionKey, text string) (time.Duration, error) {
	m.mu.RLock()
	proc, ok := m.persistent[key]
	m.mu.RUnlock()
	if !ok {
		return 0, ErrInjectNoProc
	}
	return proc.injectUserText(text)
}

// Compile-time check.
var _ Injector = (*Manager)(nil)
