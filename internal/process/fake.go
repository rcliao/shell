package process

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// A scripted Agent, for tests that want a turn without a subprocess.
//
// Seventeen bridge test files exist and none of them can drive a turn, because
// a turn meant spawning Claude. So the 429 lines around Send — rotation, ghost
// injection, prompt assembly, write-hygiene, media notes — are exercised only
// in production. This is the seam the Agent interface always documented ("so
// the implementation can be swapped … mock for testing") and nobody could use,
// because the interface was shaped around one implementation.
//
// Deliberately not a mock framework. It replays a script and records what it
// was asked, which is what a bridge test needs: assert on the request the
// bridge built, and control what comes back.

// ScriptedTurn is one canned exchange.
type ScriptedTurn struct {
	// Expect, when non-empty, must appear in the prompt text. A mismatch fails
	// the turn loudly rather than returning the wrong canned answer — a fake
	// that silently answers the wrong question is worse than no fake.
	Expect string
	// Events are emitted in order before the result is returned.
	Events []StreamEvent
	// Result is returned to the caller. Text is filled from the TextDelta
	// events when left empty, so simple scripts need only list events.
	Result SendResult
	// Err, when set, is returned instead of Result.
	Err error
}

// Fake is a scripted Agent implementation.
type Fake struct {
	// Script is consumed one turn per Send call. Running past the end is an
	// error, not a silent repeat of the last turn.
	Script []ScriptedTurn
	// Caps is what this fake claims to support. The zero value supports
	// nothing, which is useful: a test can assert the bridge degrades.
	Caps Capabilities

	mu       sync.Mutex
	requests []AgentRequest
	sessions map[SessionKey]*Session
	turn     int
}

// NewFake returns a Fake that supports everything, for tests that care about
// the bridge rather than about degradation.
func NewFake(script ...ScriptedTurn) *Fake {
	return &Fake{
		Script: script,
		Caps:   Capabilities{Images: true, PDFs: true, Streaming: true, Injection: true},
	}
}

// Requests returns every AgentRequest the fake was handed, in order. This is
// the point of the fake for most tests: assert on what the bridge built.
func (f *Fake) Requests() []AgentRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]AgentRequest(nil), f.requests...)
}

// LastRequest returns the most recent request, or false if none.
func (f *Fake) LastRequest() (AgentRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return AgentRequest{}, false
	}
	return f.requests[len(f.requests)-1], true
}

func (f *Fake) Send(ctx context.Context, req AgentRequest, onUpdate StreamFunc) (SendResult, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	i := f.turn
	f.turn++
	f.mu.Unlock()

	if i >= len(f.Script) {
		return SendResult{}, fmt.Errorf("fake: turn %d has no script (%d scripted)", i+1, len(f.Script))
	}
	st := f.Script[i]

	if st.Expect != "" && !strings.Contains(req.Text, st.Expect) {
		return SendResult{}, fmt.Errorf("fake: turn %d expected prompt containing %q, got %q",
			i+1, st.Expect, req.Text)
	}
	// Honour cancellation the way a real runtime does, so a test can exercise
	// the drain path without a subprocess.
	if err := ctx.Err(); err != nil {
		return SendResult{StopReason: StopCancelled}, err
	}
	if st.Err != nil {
		return SendResult{StopReason: StopError}, st.Err
	}

	emit := textOnly(onUpdate)
	var text strings.Builder
	for _, ev := range st.Events {
		if d, ok := ev.(TextDelta); ok {
			text.WriteString(d.Text)
		}
		if emit != nil {
			emit(ev)
		}
	}

	res := st.Result
	if res.Text == "" {
		res.Text = text.String()
	}
	if res.SessionID == "" {
		res.SessionID = req.SessionID
	}
	if res.StopReason == "" {
		res.StopReason = StopEndTurn
	}
	return res, nil
}

func (f *Fake) Capabilities() Capabilities { return f.Caps }

func (f *Fake) Get(key SessionKey) (*Session, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[key]
	return s, ok
}

func (f *Fake) Register(sess *Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sessions == nil {
		f.sessions = map[SessionKey]*Session{}
	}
	f.sessions[SessionKey{ChatID: sess.ChatID, ThreadID: sess.MessageThreadID}] = sess
}

func (f *Fake) SetCompacting(SessionKey, bool) {}

func (f *Fake) Kill(key SessionKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, key)
}

func (f *Fake) KillProcess(SessionKey) {}

func (f *Fake) KillAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = nil
}

func (f *Fake) ActiveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

func (f *Fake) ListSessions() []Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Session, 0, len(f.sessions))
	for _, s := range f.sessions {
		out = append(out, *s)
	}
	return out
}

// InjectUserText makes Fake satisfy Injector when Caps.Injection is set, so a
// test can exercise the absorb path.
func (f *Fake) InjectUserText(SessionKey, string) (time.Duration, error) {
	if !f.Caps.Injection {
		return 0, fmt.Errorf("fake: injection not supported")
	}
	return 0, nil
}

var (
	_ Agent    = (*Fake)(nil)
	_ Injector = (*Fake)(nil)
)
