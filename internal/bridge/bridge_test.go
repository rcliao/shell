package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcliao/teeny-relay/internal/memory"
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
	return New(proc, s, nil, nil, false, "", nil)
}

func TestHandleReaction_NoPlan(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	resp, err := b.HandleReaction(ctx, 123, 0, "👍")
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
		resp, err := b.HandleReaction(ctx, 123, 0, emoji)
		if err != nil {
			t.Fatalf("unexpected error for %s: %v", emoji, err)
		}
		if !strings.Contains(resp, "is not mapped") {
			t.Errorf("expected hint for unsupported emoji %s, got %q", emoji, resp)
		}
		if !strings.Contains(resp, "Available reactions:") {
			t.Errorf("expected available reactions list for %s, got %q", emoji, resp)
		}
	}
}

func TestHandleReaction_ThumbsDown_Drafting(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	b.planMu.Lock()
	b.planRuns[123] = &planRun{state: planStateDrafting, draftPlan: "- task 1", intent: "do something"}
	b.planMu.Unlock()

	resp, err := b.HandleReaction(ctx, 123, 0, "👎")
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

	resp, err := b.HandleReaction(ctx, 123, 0, "👎")
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

		resp, err := b.HandleReaction(ctx, 123, 0, "👍")
		if err != nil {
			t.Fatalf("unexpected error for state %s: %v", state, err)
		}
		if resp != "" {
			t.Errorf("expected empty response for state %s, got %q", state, resp)
		}
	}
}

func TestHandleReaction_CancelEmoji(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	// ❌ maps to "cancel" which calls PlanStop — with no plan it returns "No active plan."
	resp, err := b.HandleReaction(ctx, 123, 0, "❌")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "No plan is currently active." {
		t.Errorf("expected 'No plan is currently active.', got %q", resp)
	}
}

func TestHandleReaction_StatusEmoji(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	// 📋 maps to "status" — returns session status even without a plan.
	resp, err := b.HandleReaction(ctx, 123, 0, "📋")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == "" {
		t.Error("expected non-empty status response")
	}
}

func TestHandleReaction_CustomReactionMap(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	proc := process.NewManager(process.ManagerConfig{Binary: "echo"})
	customMap := map[string]string{"🚀": "go"}
	b := New(proc, s, nil, nil, false, "", customMap)
	ctx := context.Background()

	// 🚀 should work like 👍 (mapped to "go")
	b.planMu.Lock()
	b.planRuns[123] = &planRun{state: planStateDrafting, draftPlan: "just some text", intent: "do something"}
	b.planMu.Unlock()

	resp, err := b.HandleReaction(ctx, 123, 0, "🚀")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "No tasks found in plan." {
		t.Errorf("expected 'No tasks found in plan.', got %q", resp)
	}

	// 👍 should NOT work since it's not in the custom map — returns a hint instead.
	b.planMu.Lock()
	b.planRuns[123] = &planRun{state: planStateDrafting, draftPlan: "just some text", intent: "do something"}
	b.planMu.Unlock()

	resp, err = b.HandleReaction(ctx, 123, 0, "👍")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp, "is not mapped") {
		t.Errorf("expected hint for unmapped emoji, got %q", resp)
	}
}

func TestHandleReaction_ThumbsUp_Drafting_NoTasks(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	// Draft plan with no parseable checklist tasks → "No tasks found in plan."
	b.planMu.Lock()
	b.planRuns[123] = &planRun{state: planStateDrafting, draftPlan: "just some text", intent: "do something"}
	b.planMu.Unlock()

	resp, err := b.HandleReaction(ctx, 123, 0, "👍")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "No tasks found in plan." {
		t.Errorf("expected 'No tasks found in plan.', got %q", resp)
	}
}

func TestHandleReaction_Regenerate_NoContext(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	// 🔄 maps to "regenerate" — with no message map it should return a helpful message.
	resp, err := b.HandleReaction(ctx, 123, 999, "🔄")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "Cannot regenerate: message not found." {
		t.Errorf("expected not-found message, got %q", resp)
	}
}

func TestHandleReaction_Regenerate_DuringPlan(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	// Set up an executing plan.
	b.planMu.Lock()
	b.planRuns[123] = &planRun{state: planStateExecuting, draftPlan: "- task 1"}
	b.planMu.Unlock()

	// Save a message map entry for the reacted message.
	b.store.SaveSession(123, "sess-1")
	sess, _ := b.store.GetSession(123)
	b.store.SaveMessageMap(123, 10, 20, sess.ID, "original question", "original answer")

	resp, err := b.HandleReaction(ctx, 123, 20, "🔄")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "Cannot regenerate while a plan is active." {
		t.Errorf("expected plan-active message, got %q", resp)
	}
}

func TestReactionAction(t *testing.T) {
	b := testBridge(t)

	if got := b.ReactionAction("🔄"); got != "regenerate" {
		t.Errorf("expected 'regenerate', got %q", got)
	}
	if got := b.ReactionAction("📋"); got != "status" {
		t.Errorf("expected 'status', got %q", got)
	}
	if got := b.ReactionAction("❤️"); got != "" {
		t.Errorf("expected empty for unmapped emoji, got %q", got)
	}
}

func TestRegenerateStreaming_NoMessageMap(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	resp, err := b.RegenerateStreaming(ctx, 123, 999, func(string) {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "Cannot regenerate: message not found." {
		t.Errorf("expected not-found message, got %q", resp)
	}
}

func TestRegenerateStreaming_DuringPlan(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	b.planMu.Lock()
	b.planRuns[123] = &planRun{state: planStateExecuting, draftPlan: "- task 1"}
	b.planMu.Unlock()

	b.store.SaveSession(123, "sess-1")
	sess, _ := b.store.GetSession(123)
	b.store.SaveMessageMap(123, 10, 20, sess.ID, "original question", "original answer")

	resp, err := b.RegenerateStreaming(ctx, 123, 20, func(string) {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "Cannot regenerate while a plan is active." {
		t.Errorf("expected plan-active message, got %q", resp)
	}
}

func testBridgeWithMemory(t *testing.T) *Bridge {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	memPath := filepath.Join(dir, "memory.db")
	mem, err := memory.New(memPath, 2000, nil, 500, nil, 3000, nil, nil)
	if err != nil {
		t.Fatalf("open memory: %v", err)
	}
	t.Cleanup(func() { mem.Close() })

	proc := process.NewManager(process.ManagerConfig{Binary: "echo"})
	return New(proc, s, mem, nil, false, "", nil)
}

func TestHandleReaction_Remember_NoMemory(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	// 📌 maps to "remember" — with no memory enabled.
	resp, err := b.HandleReaction(ctx, 123, 999, "📌")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "Memory is not enabled." {
		t.Errorf("expected memory-not-enabled message, got %q", resp)
	}
}

func TestHandleReaction_Remember_NoContext(t *testing.T) {
	b := testBridgeWithMemory(t)
	ctx := context.Background()

	// Memory is enabled but no message map exists for this message ID.
	resp, err := b.HandleReaction(ctx, 123, 999, "📌")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "Cannot remember: message not found." {
		t.Errorf("expected not-found message, got %q", resp)
	}
}

func TestHandleReaction_Remember_WithContext(t *testing.T) {
	b := testBridgeWithMemory(t)
	ctx := context.Background()

	// Create a session and message map.
	b.store.SaveSession(123, "sess-1")
	sess, _ := b.store.GetSession(123)
	b.store.SaveMessageMap(123, 10, 20, sess.ID, "What is Go?", "Go is a programming language.")

	resp, err := b.HandleReaction(ctx, 123, 20, "📌")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "Response saved to memory." {
		t.Errorf("expected 'Response saved to memory.', got %q", resp)
	}

	// Verify the memory was stored by listing memories.
	list, err := b.memory.ListMemories(ctx, 123)
	if err != nil {
		t.Fatalf("list memories: %v", err)
	}
	if list == "" || list == "No memories stored. Use /remember <text> to save one." {
		t.Error("expected at least one memory after remember reaction")
	}
}

func TestHandleReaction_Forget_NoContext(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	// 🗑 maps to "forget" — with no message map.
	resp, err := b.HandleReaction(ctx, 123, 999, "🗑")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "Cannot forget: message not found." {
		t.Errorf("expected not-found message, got %q", resp)
	}
}

func TestHandleReaction_Forget_WithContext(t *testing.T) {
	b := testBridge(t)
	ctx := context.Background()

	// Create a session and message map.
	b.store.SaveSession(123, "sess-1")
	sess, _ := b.store.GetSession(123)
	b.store.LogMessage(sess.ID, "user", "test question")
	b.store.LogMessage(sess.ID, "assistant", "test answer")
	b.store.SaveMessageMap(123, 10, 20, sess.ID, "test question", "test answer")

	resp, err := b.HandleReaction(ctx, 123, 20, "🗑")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "Exchange forgotten." {
		t.Errorf("expected 'Exchange forgotten.', got %q", resp)
	}

	// Verify message map is deleted.
	m, _ := b.store.GetMessageMapByBotMsg(123, 20)
	if m != nil {
		t.Error("expected message map to be deleted")
	}

	// Verify messages are deleted.
	msgs, _ := b.store.GetMessages(sess.ID, 10)
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages after forget, got %d", len(msgs))
	}
}
