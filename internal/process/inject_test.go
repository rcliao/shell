package process

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// newTestProc builds a persistentProc with an in-memory stdin, enough for
// exercising the injection state machine without a real subprocess.
func newTestProc(stdin *bytes.Buffer) *persistentProc {
	return &persistentProc{stdinW: &syncWriter{w: stdin}}
}

func TestInjectUserText_NoActiveTurn(t *testing.T) {
	var stdin bytes.Buffer
	p := newTestProc(&stdin)
	if _, err := p.injectUserText("hi"); !errors.Is(err, ErrInjectNoTurn) {
		t.Fatalf("expected ErrInjectNoTurn, got %v", err)
	}
	if stdin.Len() != 0 {
		t.Errorf("nothing should be written on refusal, got %q", stdin.String())
	}
}

func TestInjectUserText_NoToolInflight(t *testing.T) {
	// Mid-inference (no pending tool_result) is the case measured to spawn a
	// SECOND turn whose result the read loop would orphan — must refuse.
	var stdin bytes.Buffer
	p := newTestProc(&stdin)
	p.turn.begin()
	if _, err := p.injectUserText("hi"); !errors.Is(err, ErrInjectNoToolInflight) {
		t.Fatalf("expected ErrInjectNoToolInflight, got %v", err)
	}
	// A tool that started and finished also leaves no absorption window.
	p.turn.toolStarted()
	p.turn.toolEnded()
	if _, err := p.injectUserText("hi"); !errors.Is(err, ErrInjectNoToolInflight) {
		t.Fatalf("expected ErrInjectNoToolInflight after tool ended, got %v", err)
	}
	if stdin.Len() != 0 {
		t.Errorf("nothing should be written on refusal, got %q", stdin.String())
	}
}

func TestInjectUserText_ToolInflight_WritesUserMessage(t *testing.T) {
	var stdin bytes.Buffer
	p := newTestProc(&stdin)
	p.sessionID = "sess-42"
	p.turn.begin()
	p.turn.toolStarted()

	age, err := p.injectUserText("follow-up")
	if err != nil {
		t.Fatalf("expected injection to succeed, got %v", err)
	}
	if age < 0 {
		t.Errorf("expected non-negative turn age, got %v", age)
	}

	line := strings.TrimSpace(stdin.String())
	var msg stdinUserMessage
	if uerr := json.Unmarshal([]byte(line), &msg); uerr != nil {
		t.Fatalf("injected line is not valid JSON: %v (%q)", uerr, line)
	}
	if msg.Type != "user" || msg.Message.Role != "user" {
		t.Errorf("expected a user message, got type=%q role=%q", msg.Type, msg.Message.Role)
	}
	if msg.SessionID != "sess-42" {
		t.Errorf("expected session id carried through, got %q", msg.SessionID)
	}
	var content string
	if uerr := json.Unmarshal(msg.Message.Content, &content); uerr != nil || content != "follow-up" {
		t.Errorf("expected text content %q, got %q (err %v)", "follow-up", content, uerr)
	}

	// Second injection into the same turn is refused (one addendum per turn).
	if _, err := p.injectUserText("again"); !errors.Is(err, ErrInjectAlready) {
		t.Fatalf("expected ErrInjectAlready, got %v", err)
	}
}

func TestInjectUserText_TurnEndResetsState(t *testing.T) {
	var stdin bytes.Buffer
	p := newTestProc(&stdin)
	p.turn.begin()
	p.turn.toolStarted()
	if _, err := p.injectUserText("one"); err != nil {
		t.Fatalf("first injection failed: %v", err)
	}
	p.turn.end()
	if _, err := p.injectUserText("two"); !errors.Is(err, ErrInjectNoTurn) {
		t.Fatalf("expected ErrInjectNoTurn after end, got %v", err)
	}
	// A fresh turn gets a fresh injection budget.
	p.turn.begin()
	p.turn.toolStarted()
	if _, err := p.injectUserText("three"); err != nil {
		t.Fatalf("expected fresh turn to accept injection, got %v", err)
	}
}

func TestManagerInjectUserText_NoProc(t *testing.T) {
	m := NewManager(ManagerConfig{})
	if _, err := m.InjectUserText(SessionKey{ChatID: 1}, "hi"); !errors.Is(err, ErrInjectNoProc) {
		t.Fatalf("expected ErrInjectNoProc, got %v", err)
	}
}

func TestParseObserved_ToolLifecycleDrivesInjectionWindow(t *testing.T) {
	// Feed the parse loop a synthetic turn and verify the observer opens the
	// injection window exactly between tool_use and tool_result.
	events := []string{
		`{"type":"system","subtype":"init","session_id":"s1"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Bash","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"result","result":"done","session_id":"s1"}`,
	}

	var stdin bytes.Buffer
	p := newTestProc(&stdin)
	p.turn.begin()

	inflightAfter := map[string]int{}
	sc := bufio.NewScanner(strings.NewReader(strings.Join(events, "\n")))
	// The parse loop is a single blocking call, so snapshot the inflight
	// count from the observer callbacks via a wrapper.
	obs := &recordingObserver{inner: &p.turn, snapshots: inflightAfter, ts: &p.turn}
	result := parseBidirectionalEventsObserved(sc, &stdin, nil, obs)

	if result.Text != "done" {
		t.Errorf("expected result text 'done', got %q", result.Text)
	}
	if got := inflightAfter["started"]; got != 1 {
		t.Errorf("expected 1 tool inflight after tool_use, got %d", got)
	}
	if got := inflightAfter["ended"]; got != 0 {
		t.Errorf("expected 0 tools inflight after tool_result, got %d", got)
	}
	// After the turn, injection must refuse again.
	p.turn.end()
	if _, err := p.injectUserText("late"); !errors.Is(err, ErrInjectNoTurn) {
		t.Errorf("expected ErrInjectNoTurn after result, got %v", err)
	}
}

// recordingObserver forwards to the real turnState and snapshots the
// inflight count after each transition.
type recordingObserver struct {
	inner     turnObserver
	ts        *turnState
	snapshots map[string]int
}

func (r *recordingObserver) toolStarted() {
	r.inner.toolStarted()
	r.ts.mu.Lock()
	r.snapshots["started"] = r.ts.toolsInflight
	r.ts.mu.Unlock()
}

func (r *recordingObserver) toolEnded() {
	r.inner.toolEnded()
	r.ts.mu.Lock()
	r.snapshots["ended"] = r.ts.toolsInflight
	r.ts.mu.Unlock()
}
