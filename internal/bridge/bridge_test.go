package bridge

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rcliao/teeny-relay/internal/process"
	"github.com/rcliao/teeny-relay/internal/store"
)

func testBridge(t *testing.T) *Bridge {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	proc := process.NewManager(process.ManagerConfig{Binary: "echo"})
	return New(proc, s, nil, nil, false, "")
}

func TestHandleReaction_NoPlan(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	resp, err := b.HandleReaction(ctx, 123, "👍")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "" {
		t.Errorf("expected empty response with no plan, got %q", resp)
	}
}

func TestHandleReaction_UnsupportedEmoji(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	// Set up a drafting plan so the emoji filter is the only gate.
	b.planMu.Lock()
	b.planRuns[123] = &planRun{state: planStateDrafting, draftPlan: "- task 1"}
	b.planMu.Unlock()

	for _, emoji := range []string{"❤️", "😂", "🔥", "🎉", "✅"} {
		resp, err := b.HandleReaction(ctx, 123, emoji)
		if err != nil {
			t.Fatalf("unexpected error for %s: %v", emoji, err)
		}
		if resp != "" {
			t.Errorf("expected empty response for unsupported emoji %s, got %q", emoji, resp)
		}
	}
}

func TestHandleReaction_ThumbsDown_Drafting(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	b.planMu.Lock()
	b.planRuns[123] = &planRun{state: planStateDrafting, draftPlan: "- task 1", intent: "do something"}
	b.planMu.Unlock()

	resp, err := b.HandleReaction(ctx, 123, "👎")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "Plan cancelled." {
		t.Errorf("expected 'Plan cancelled.', got %q", resp)
	}

	// Plan should be removed.
	b.planMu.Lock()
	_, exists := b.planRuns[123]
	b.planMu.Unlock()
	if exists {
		t.Error("expected plan to be removed after cancellation")
	}
}

func TestHandleReaction_ThumbsDown_Blocked(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	b.planMu.Lock()
	b.planRuns[123] = &planRun{state: planStateBlocked, draftPlan: "- task 1", intent: "do something"}
	b.planMu.Unlock()

	resp, err := b.HandleReaction(ctx, 123, "👎")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "Plan cancelled." {
		t.Errorf("expected 'Plan cancelled.', got %q", resp)
	}

	b.planMu.Lock()
	_, exists := b.planRuns[123]
	b.planMu.Unlock()
	if exists {
		t.Error("expected plan to be removed after cancellation")
	}
}

func TestHandleReaction_IgnoredInNonInteractiveStates(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	for _, state := range []planState{planStateIdle, planStateExecuting, planStateDone} {
		b.planMu.Lock()
		b.planRuns[123] = &planRun{state: state, draftPlan: "- task 1"}
		b.planMu.Unlock()

		resp, err := b.HandleReaction(ctx, 123, "👍")
		if err != nil {
			t.Fatalf("unexpected error for state %s: %v", state, err)
		}
		if resp != "" {
			t.Errorf("expected empty response for state %s, got %q", state, resp)
		}
	}
}

func TestHandleReaction_ThumbsUp_Drafting_NoTasks(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	// Draft plan with no parseable checklist tasks → "No tasks found in plan."
	b.planMu.Lock()
	b.planRuns[123] = &planRun{state: planStateDrafting, draftPlan: "just some text", intent: "do something"}
	b.planMu.Unlock()

	resp, err := b.HandleReaction(ctx, 123, "👍")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "No tasks found in plan." {
		t.Errorf("expected 'No tasks found in plan.', got %q", resp)
	}
}
