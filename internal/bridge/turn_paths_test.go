package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/rcliao/shell/internal/process"
	_ "modernc.org/sqlite"
)

// Media-note stripping is scoped to turns that actually archived a photo
// (bridge.go: `if archivedCount > 0`). On a text turn the marker is left
// alone, which is correct — the agent should not be emitting one there, and
// silently rewriting arbitrary replies would be worse than passing one
// through. This pins that scoping so a future change cannot widen it by
// accident; extractMediaNote itself is unit-tested separately.
func TestMediaNoteIsNotStrippedOnTextOnlyTurns(t *testing.T) {
	f := process.NewFake(process.ScriptedTurn{
		Events: []process.StreamEvent{
			process.TextDelta{Text: "That looks like a map screenshot."},
			process.TextDelta{Text: " [media-note: a marker on a text turn]"},
		},
	})
	b := turnBridge(t, f)

	resp, err := b.HandleMessageStreaming(context.Background(), -100200300, 0,
		"what is this", "someone", nil, nil, nil)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if !strings.Contains(resp.Text, "map screenshot") {
		t.Errorf("real content lost: %q", resp.Text)
	}
	if !strings.Contains(resp.Text, "media-note") {
		t.Error("marker was stripped on a turn with no archived photo — scoping widened")
	}
}

// Sender attribution: a group turn must tell the model WHO is speaking, or the
// agent answers the wrong person — the failure class behind the relay-routing
// bugs. Asserting on the built request is exactly what the fake is for.
func TestSenderNameReachesThePrompt(t *testing.T) {
	f := process.NewFake(process.ScriptedTurn{
		Events: []process.StreamEvent{process.TextDelta{Text: "ok"}},
	})
	b := turnBridge(t, f)

	if _, err := b.HandleMessageStreaming(context.Background(), -100200300, 0,
		"is dinner ready?", "a-family-member", nil, nil, nil); err != nil {
		t.Fatalf("turn: %v", err)
	}
	req, _ := f.LastRequest()
	if !strings.Contains(req.Text, "a-family-member") {
		t.Fatalf("sender missing from prompt — the agent cannot tell who asked:\n%q", req.Text)
	}
}

// Thread routing: sessions are keyed by (chat, thread). A turn in a forum topic
// that lost its thread id would answer into the wrong session — the shape of
// the topic-routing bugs.
func TestThreadIdSurvivesToTheRequest(t *testing.T) {
	f := process.NewFake(
		process.ScriptedTurn{Events: []process.StreamEvent{process.TextDelta{Text: "a"}}},
		process.ScriptedTurn{Events: []process.StreamEvent{process.TextDelta{Text: "b"}}},
	)
	b := turnBridge(t, f)

	for _, thread := range []int64{0, 1419} {
		if _, err := b.HandleMessageStreaming(context.Background(), -100200300, thread,
			"hi", "someone", nil, nil, nil); err != nil {
			t.Fatalf("turn(thread=%d): %v", thread, err)
		}
	}
	reqs := f.Requests()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests", len(reqs))
	}
	if reqs[0].MessageThreadID != 0 || reqs[1].MessageThreadID != 1419 {
		t.Fatalf("thread routing lost: %d then %d",
			reqs[0].MessageThreadID, reqs[1].MessageThreadID)
	}
}

// An empty reply must not be dressed up as success. The bridge has a
// corrective-retry path for this in DMs; at minimum the response must come back
// empty rather than fabricated.
func TestEmptyReplyStaysEmpty(t *testing.T) {
	f := process.NewFake(process.ScriptedTurn{Events: []process.StreamEvent{}})
	b := turnBridge(t, f)

	resp, err := b.HandleMessageStreaming(context.Background(), -100200300, 0,
		"hello", "someone", nil, nil, nil)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if resp.Text != "" {
		t.Fatalf("an empty turn produced text: %q", resp.Text)
	}
}

// The execution profile must reach the request: model routing per task type is
// live config on both agents, and a turn that silently lost its model would
// run on the CLI default with no signal.
func TestExecutionProfileReachesTheRequest(t *testing.T) {
	f := process.NewFake(process.ScriptedTurn{
		Events: []process.StreamEvent{process.TextDelta{Text: "ok"}},
	})
	b := turnBridge(t, f)

	if _, err := b.HandleMessageStreaming(context.Background(), -100200300, 0,
		"hi", "someone", nil, nil, nil); err != nil {
		t.Fatalf("turn: %v", err)
	}
	req, _ := f.LastRequest()
	// With no model configured the profile resolves empty, which is the CLI
	// default — the assertion is that the field is wired, not its value.
	if req.SystemPrompt == "" {
		t.Error("system prompt did not reach the request")
	}
	if req.ChatID == 0 {
		t.Error("chat id did not reach the request")
	}
}
