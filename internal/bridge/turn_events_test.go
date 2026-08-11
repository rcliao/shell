package bridge

import (
	"context"
	"testing"

	"github.com/rcliao/shell/internal/process"
	_ "modernc.org/sqlite"
)

// The payoff for phase 2: a caller can see tool activity WHILE the turn runs.
// Until this, tool calls appeared only in SendResult once everything was over,
// so the Telegram placeholder had nothing to say but "Thinking…" and had to
// guess reassurance it had no evidence for.
func TestTurnReportsToolActivityAsItHappens(t *testing.T) {
	f := process.NewFake(process.ScriptedTurn{
		Events: []process.StreamEvent{
			process.ToolStarted{ID: "t1", Name: "WebSearch"},
			process.ToolFinished{ID: "t1"},
			process.TextDelta{Text: "found it"},
		},
	})
	b := turnBridge(t, f)

	var order []string
	_, err := b.HandleMessageStreamingEvents(context.Background(), -100200300, 0,
		"look it up", "someone", nil, nil, func(ev process.StreamEvent) {
			switch e := ev.(type) {
			case process.ToolStarted:
				order = append(order, "start:"+e.Name)
			case process.ToolFinished:
				order = append(order, "finish")
			case process.TextDelta:
				order = append(order, "text")
			}
		})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}

	// Order matters: the tool must be observable BEFORE any text arrives,
	// which is the whole window the placeholder is trying to describe.
	if len(order) < 3 || order[0] != "start:WebSearch" || order[1] != "finish" {
		t.Fatalf("event order = %v, want tool start then finish then text", order)
	}
	for _, o := range order[:2] {
		if o == "text" {
			t.Fatal("text arrived before the tool events — placeholder would have nothing to show")
		}
	}
}

// The compatibility guarantee: callers still on HandleMessageStreaming see
// exactly the text they saw before, with no tool noise leaking into a callback
// that only ever handled prose.
func TestTextCallersSeeOnlyText(t *testing.T) {
	f := process.NewFake(process.ScriptedTurn{
		Events: []process.StreamEvent{
			process.ToolStarted{ID: "t1", Name: "Bash"},
			process.TextDelta{Text: "hello"},
			process.ToolFinished{ID: "t1"},
			process.TextDelta{Text: " there"},
		},
	})
	b := turnBridge(t, f)

	var chunks []string
	resp, err := b.HandleMessageStreaming(context.Background(), -100200300, 0,
		"hi", "someone", nil, nil, func(s string) { chunks = append(chunks, s) })
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	joined := ""
	for _, c := range chunks {
		joined += c
	}
	if joined != "hello there" {
		t.Fatalf("text caller saw %q, want %q", joined, "hello there")
	}
	if resp.Text != "hello there" {
		t.Errorf("reply = %q", resp.Text)
	}
}
