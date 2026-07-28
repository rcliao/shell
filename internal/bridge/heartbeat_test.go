package bridge

import (
	"context"
	"strings"
	"testing"
)

// Deep heartbeats must include the V2-H47 lesson-to-action block; light
// heartbeats must not.
func TestEnrichHeartbeatPrompt_LessonToActionDeepOnly(t *testing.T) {
	b := testBridgeWithMemory(t)
	ctx := context.Background()

	deep := b.enrichHeartbeatPrompt(ctx, SystemChatID, "check in", true)
	if !strings.Contains(deep, "Lesson to action") {
		t.Error("deep heartbeat prompt missing lesson-to-action block")
	}
	if !strings.Contains(deep, "[lesson-action]") {
		t.Error("deep heartbeat prompt missing [lesson-action] ledger instruction")
	}
	if !strings.Contains(deep, "never send media") {
		t.Error("deep heartbeat prompt missing media guardrail")
	}

	light := b.enrichHeartbeatPrompt(ctx, SystemChatID, "check in", false)
	if strings.Contains(light, "Lesson to action") {
		t.Error("light heartbeat prompt must not include lesson-to-action block")
	}
	if strings.Contains(light, "[lesson-action]") {
		t.Error("light heartbeat prompt must not include [lesson-action] instruction")
	}
}

// The lesson_to_action_disabled kill switch must remove the block from deep
// heartbeats without disturbing the rest of the deep-reflection prompt.
func TestEnrichHeartbeatPrompt_LessonToActionKillSwitch(t *testing.T) {
	b := testBridgeWithMemory(t)
	b.SetLessonToActionDisabled(true)
	ctx := context.Background()

	deep := b.enrichHeartbeatPrompt(ctx, SystemChatID, "check in", true)
	if strings.Contains(deep, "Lesson to action") {
		t.Error("kill switch on: deep heartbeat must not include lesson-to-action block")
	}
	if strings.Contains(deep, "[lesson-action]") {
		t.Error("kill switch on: deep heartbeat must not include [lesson-action] instruction")
	}
	// Rest of the deep-reflection enrichment stays intact.
	if !strings.Contains(deep, "Deep Reflection") {
		t.Error("kill switch must not remove the deep-reflection section")
	}
}

// The numbered "Priority"/checklist scaffolding was removed in favor of a
// context-then-goal framing: the model judges what the beat needs instead of
// executing a fixed list. Guard against it creeping back in.
func TestEnrichHeartbeatPrompt_NoPriorityScaffolding(t *testing.T) {
	b := testBridgeWithMemory(t)
	ctx := context.Background()

	// Seed one real-chat exchange so the light beat enriches too (an empty
	// light beat short-circuits and returns the message unchanged).
	if err := b.store.SaveSession(555, 0, "s-555"); err != nil {
		t.Fatalf("save session: %v", err)
	}
	b.memory.LogExchange(ctx, 555, "the user asked something", "the agent answered")

	for _, isDeep := range []bool{true, false} {
		p := b.enrichHeartbeatPrompt(ctx, SystemChatID, "check in", isDeep)
		if strings.Contains(p, "Priority") || strings.Contains(p, "priorities") {
			t.Errorf("isDeep=%v: prompt still contains priority scaffolding", isDeep)
		}
		if strings.Contains(p, "Behavioral Self-Evaluation") {
			t.Errorf("isDeep=%v: prompt still contains the old self-evaluation checklist header", isDeep)
		}
		// Judgment framing must be present in its place.
		if !strings.Contains(p, "not a to-do list") && !strings.Contains(p, "not a checklist") {
			t.Errorf("isDeep=%v: prompt missing the judgment (context-not-checklist) framing", isDeep)
		}
		// The load-bearing noop norm stays.
		if !strings.Contains(p, "[noop]") {
			t.Errorf("isDeep=%v: prompt missing the [noop] outcome", isDeep)
		}
	}
}

// The chat-id attribution contract on recent exchanges is load-bearing (a
// heartbeat has no current chat; replies must target the tagged source chat).
// The instruction only renders when exchanges exist, so verify the static
// strings that carry it are intact in the source of the enriched prompt for a
// bridge with recorded exchanges.
func TestEnrichHeartbeatPrompt_ChatIDRelayInstruction(t *testing.T) {
	b := testBridgeWithMemory(t)
	ctx := context.Background()

	// Record an exchange in a real (non-system) chat so the history block renders.
	if err := b.store.SaveSession(777, 0, "s-777"); err != nil {
		t.Fatalf("save session: %v", err)
	}
	b.memory.LogExchange(ctx, 777, "the user asked something", "the agent answered")

	deep := b.enrichHeartbeatPrompt(ctx, SystemChatID, "check in", true)
	if !strings.Contains(deep, "(chat 777)") {
		t.Fatal("recent exchange missing per-chat attribution tag")
	}
	if !strings.Contains(deep, "BACK TO THAT SAME chat id") {
		t.Error("prompt missing the relay-back-to-source-chat instruction")
	}
	if !strings.Contains(deep, "Never default to the group") {
		t.Error("prompt missing the never-default-to-group instruction")
	}
}
