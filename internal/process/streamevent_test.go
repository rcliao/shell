package process

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func eventsFrom(t *testing.T, lines []string) ([]StreamEvent, SendResult) {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(strings.Join(lines, "\n")))
	var stdin bytes.Buffer
	var got []StreamEvent
	res := parseEvents(sc, &stdin, func(ev StreamEvent) { got = append(got, ev) }, nil)
	return got, res
}

const (
	lineInit = `{"type":"control_response","response":{"subtype":"success","request_id":"init_1"}}`
	lineSys  = `{"type":"system","subtype":"init","session_id":"s"}`
)

// Tool activity must be observable DURING the turn. Before this, tool calls
// only appeared in SendResult once everything was over, so nothing could show
// what the agent was doing while it did it.
func TestToolLifecycleIsVisibleWhileTheTurnRuns(t *testing.T) {
	events, res := eventsFrom(t, []string{
		lineInit, lineSys,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"result","subtype":"success","session_id":"s","result":"done"}`,
	})

	var started, finished int
	for _, ev := range events {
		switch e := ev.(type) {
		case ToolStarted:
			started++
			if e.Name != "Bash" || e.ID != "tu_1" {
				t.Errorf("ToolStarted = %+v", e)
			}
			if e.Input["command"] != "ls" {
				t.Errorf("tool input not passed through: %+v", e.Input)
			}
		case ToolFinished:
			finished++
			if e.ID != "tu_1" {
				t.Errorf("ToolFinished id = %q", e.ID)
			}
			if e.Err != "" {
				t.Errorf("successful tool reported an error: %q", e.Err)
			}
		}
	}
	if started != 1 || finished != 1 {
		t.Fatalf("started=%d finished=%d, want 1 and 1", started, finished)
	}
	if res.StopReason != StopEndTurn {
		t.Errorf("StopReason = %q", res.StopReason)
	}
}

// A failed tool must be distinguishable from a successful one at stream time,
// not only by cross-referencing SendResult afterwards.
func TestFailedToolIsMarkedOnTheStream(t *testing.T) {
	events, _ := eventsFrom(t, []string{
		lineInit, lineSys,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_9","name":"Write","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_9","is_error":true,"content":"denied"}]}}`,
		`{"type":"result","subtype":"success","session_id":"s"}`,
	})
	for _, ev := range events {
		if e, ok := ev.(ToolFinished); ok {
			if e.Err == "" {
				t.Fatal("a failed tool arrived on the stream indistinguishable from success")
			}
			return
		}
	}
	t.Fatal("no ToolFinished event emitted")
}

// The compatibility promise for phase 2: existing callers pass a StreamFunc and
// see exactly the text they saw before — no tool noise leaking into a callback
// that only ever handled prose.
func TestTextOnlyAdapterForwardsTextAndNothingElse(t *testing.T) {
	var got []string
	emit := textOnly(func(s string) { got = append(got, s) })

	emit(TextDelta{Text: "hello"})
	emit(ToolStarted{ID: "t", Name: "Bash"})
	emit(ToolFinished{ID: "t"})
	emit(ToolFinished{ID: "t2"})
	emit(TextDelta{Text: " world"})

	if strings.Join(got, "") != "hello world" {
		t.Fatalf("adapter forwarded %q", got)
	}
}

// A nil StreamFunc must produce a nil EventFunc, not a callback that panics on
// first use — several call sites pass nil when they want no streaming.
func TestTextOnlyOfNilIsNil(t *testing.T) {
	if textOnly(nil) != nil {
		t.Fatal("textOnly(nil) returned a non-nil EventFunc")
	}
}
