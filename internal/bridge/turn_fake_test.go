package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/store"
	_ "modernc.org/sqlite"
)

// The point of phase 3: a real bridge turn, end to end, with no subprocess and
// no network. Before the fake existed this could not be written at all — every
// path through HandleMessageStreaming needed a live Claude CLI, so the 429
// lines of turn preparation were exercised only in production.
func turnBridge(t *testing.T, f *process.Fake) *Bridge {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(f, st, nil, nil, false, "", nil, nil, nil, nil)
}

func TestBridgeRunsATurnThroughTheFake(t *testing.T) {
	f := process.NewFake(process.ScriptedTurn{
		Events: []process.StreamEvent{
			process.TextDelta{Text: "the parcel "},
			process.TextDelta{Text: "arrived"},
		},
	})
	b := turnBridge(t, f)

	var streamed []string
	resp, err := b.HandleMessageStreaming(context.Background(), -100200300, 0,
		"did the parcel arrive?", "someone", nil, nil,
		func(delta string) { streamed = append(streamed, delta) })
	if err != nil {
		t.Fatalf("HandleMessageStreaming: %v", err)
	}

	if resp.Text != "the parcel arrived" {
		t.Errorf("reply = %q", resp.Text)
	}
	if strings.Join(streamed, "") != "the parcel arrived" {
		t.Errorf("streamed %q", streamed)
	}
}

// What the fake is really for: asserting on the request the bridge BUILT. This
// is the prompt-assembly path — six systemPrompt sources, the sender framing —
// which nothing could previously observe without reading production logs.
func TestBridgeBuildsARequestWeCanInspect(t *testing.T) {
	f := process.NewFake(process.ScriptedTurn{
		Events: []process.StreamEvent{process.TextDelta{Text: "ok"}},
	})
	b := turnBridge(t, f)

	if _, err := b.HandleMessageStreaming(context.Background(), -100200300, 1419,
		"what is for lunch?", "someone", nil, nil, nil); err != nil {
		t.Fatalf("HandleMessageStreaming: %v", err)
	}

	req, ok := f.LastRequest()
	if !ok {
		t.Fatal("bridge never called the agent")
	}
	if req.ChatID != -100200300 || req.MessageThreadID != 1419 {
		t.Errorf("routing lost: chat=%d thread=%d", req.ChatID, req.MessageThreadID)
	}
	if !strings.Contains(req.Text, "what is for lunch?") {
		t.Errorf("user text missing from prompt: %q", req.Text)
	}
	if req.SystemPrompt == "" {
		t.Error("no system prompt was assembled")
	}
}

// A runtime failure must surface as an error rather than a silent empty reply —
// the caller has to know the difference between "said nothing" and "broke".
func TestBridgeSurfacesRuntimeFailure(t *testing.T) {
	f := process.NewFake(process.ScriptedTurn{Err: errRuntime})
	b := turnBridge(t, f)

	_, err := b.HandleMessageStreaming(context.Background(), -100200300, 0,
		"hello", "someone", nil, nil, nil)
	if err == nil {
		t.Fatal("a failing runtime produced no error")
	}
}

var errRuntime = &runtimeErr{}

type runtimeErr struct{}

func (*runtimeErr) Error() string { return "runtime exploded" }
