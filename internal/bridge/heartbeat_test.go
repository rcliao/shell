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
