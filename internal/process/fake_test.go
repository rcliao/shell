package process

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestFakeReplaysScriptAndRecordsRequests(t *testing.T) {
	f := NewFake(
		ScriptedTurn{Events: []StreamEvent{TextDelta{Text: "hello "}, TextDelta{Text: "world"}}},
	)
	var streamed []string
	res, err := f.Send(context.Background(), AgentRequest{
		ChatID: -100200300, Text: "hi", Model: "claude-opus-5",
	}, func(s string) { streamed = append(streamed, s) })
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Text != "hello world" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.StopReason != StopEndTurn {
		t.Errorf("StopReason = %q, want end_turn by default", res.StopReason)
	}
	if strings.Join(streamed, "") != "hello world" {
		t.Errorf("streamed %q", streamed)
	}
	// The point of the fake for most tests: what did the caller actually build?
	got, ok := f.LastRequest()
	if !ok || got.Model != "claude-opus-5" || got.ChatID != -100200300 {
		t.Fatalf("LastRequest = %+v ok=%v", got, ok)
	}
}

// Running off the end of the script must fail loudly. Silently repeating the
// last turn would let a test assert on an exchange that never happened.
func TestFakeFailsWhenTheScriptRunsOut(t *testing.T) {
	f := NewFake(ScriptedTurn{Events: []StreamEvent{TextDelta{Text: "one"}}})
	if _, err := f.Send(context.Background(), AgentRequest{Text: "a"}, nil); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	_, err := f.Send(context.Background(), AgentRequest{Text: "b"}, nil)
	if err == nil {
		t.Fatal("second turn succeeded with no script — a test could assert on a turn that never ran")
	}
}

// A mismatched prompt must fail rather than return the wrong canned answer.
func TestFakeRefusesAPromptItDidNotExpect(t *testing.T) {
	f := NewFake(ScriptedTurn{Expect: "weather", Events: []StreamEvent{TextDelta{Text: "sunny"}}})
	_, err := f.Send(context.Background(), AgentRequest{Text: "what time is it"}, nil)
	if err == nil {
		t.Fatal("fake answered a question it was not scripted for")
	}
	if !strings.Contains(err.Error(), "weather") {
		t.Errorf("error should name the expectation, got %v", err)
	}
}

// Cancellation is reported as cancelled, not as a generic error — the
// distinction the drain path needs.
func TestFakeReportsCancellationDistinctly(t *testing.T) {
	f := NewFake(ScriptedTurn{Events: []StreamEvent{TextDelta{Text: "never sent"}}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := f.Send(ctx, AgentRequest{Text: "hi"}, nil)
	if err == nil {
		t.Fatal("cancelled context produced no error")
	}
	if res.StopReason != StopCancelled {
		t.Errorf("StopReason = %q, want cancelled", res.StopReason)
	}
}

func TestFakeSurfacesScriptedErrors(t *testing.T) {
	want := errors.New("runtime exploded")
	f := NewFake(ScriptedTurn{Err: want})
	res, err := f.Send(context.Background(), AgentRequest{Text: "hi"}, nil)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if res.StopReason != StopError {
		t.Errorf("StopReason = %q, want error", res.StopReason)
	}
}

// A fake that declares no capabilities must refuse injection, so a test can
// assert the caller degrades instead of assuming support.
func TestFakeWithoutInjectionRefusesIt(t *testing.T) {
	f := &Fake{Script: []ScriptedTurn{{}}} // zero Caps: supports nothing
	if _, err := f.InjectUserText(SessionKey{}, "x"); err == nil {
		t.Fatal("a fake declaring no injection accepted an injection")
	}
	if f.Capabilities().Streaming {
		t.Error("zero-value Caps claimed streaming support")
	}
}
