package bridge

import (
	"strconv"
	"testing"
)

// fakeResolver maps task_type → model, mirroring config.ClaudeConfig.ResolveModel.
type fakeResolver map[string]string

func (f fakeResolver) ResolveModel(taskType string) string { return f[taskType] }
func (f fakeResolver) ResolveChatModel(taskType string, chatID int64) string {
	if taskType == "conversation" {
		if m := f["chat:"+strconv.FormatInt(chatID, 10)]; m != "" {
			return m
		}
	}
	return f[taskType]
}
func (f fakeResolver) ResolveEffort(taskType string) string { return f[taskType+"_effort"] }

func TestResolveExecutionProfile(t *testing.T) {
	r := fakeResolver{
		"conversation":    "claude-opus-4-8",
		"heartbeat":       "claude-sonnet-5",
		"heartbeat_deep":  "claude-opus-4-8",
		"chat:-100200300": "claude-fable-5-1", // a family group routed to a higher tier
	}

	cases := []struct {
		name string
		kind turnKind
		want ExecutionProfile
	}{
		{
			name: "conversation → persistent, no effort",
			kind: turnKind{},
			want: ExecutionProfile{Model: "claude-opus-4-8", Effort: "", Ephemeral: false, TaskType: "conversation"},
		},
		{
			name: "conversation in a chat_models chat → that chat's model, still persistent",
			kind: turnKind{chatID: -100200300},
			want: ExecutionProfile{Model: "claude-fable-5-1", Effort: "", Ephemeral: false, TaskType: "conversation"},
		},
		{
			name: "conversation in an unlisted chat → default conversation model",
			kind: turnKind{chatID: 42},
			want: ExecutionProfile{Model: "claude-opus-4-8", Effort: "", Ephemeral: false, TaskType: "conversation"},
		},
		{
			name: "heartbeat ignores chat_models (chat 0 is never a family chat)",
			kind: turnKind{isHeartbeat: true, chatID: -100200300},
			want: ExecutionProfile{Model: "claude-sonnet-5", Effort: "", Ephemeral: true, Timeout: deepHeartbeatTimeout, TaskType: "heartbeat"},
		},
		{
			name: "fable keyword still wins over the chat's model",
			kind: turnKind{fableTurn: true, chatID: -100200300},
			want: ExecutionProfile{Model: fableModel, Effort: "", Ephemeral: true, TaskType: "conversation"},
		},
		{
			name: "light heartbeat → its model, ephemeral (no --resume replay), no effort",
			kind: turnKind{isHeartbeat: true},
			want: ExecutionProfile{Model: "claude-sonnet-5", Effort: "", Ephemeral: true, Timeout: deepHeartbeatTimeout, TaskType: "heartbeat"},
		},
		{
			name: "project turn → persistent on the chat's model, 20m timeout",
			kind: turnKind{projectTurn: true, chatID: -100200300},
			want: ExecutionProfile{Model: "claude-fable-5-1", Effort: "", Ephemeral: false, Timeout: projectTurnTimeout, TaskType: "conversation"},
		},
		{
			name: "deep heartbeat → high effort, ephemeral, longer timeout (S1 + timeout fix)",
			kind: turnKind{isHeartbeat: true, isDeepHeartbeat: true},
			want: ExecutionProfile{Model: "claude-opus-4-8", Effort: "high", Ephemeral: true, Timeout: deepHeartbeatTimeout, TaskType: "heartbeat_deep"},
		},
		{
			name: "fable keyword → fable model, ephemeral, isolated",
			kind: turnKind{fableTurn: true},
			want: ExecutionProfile{Model: fableModel, Effort: "", Ephemeral: true, TaskType: "conversation"},
		},
		{
			name: "fable overrides even a deep heartbeat's model, stays ephemeral+high+timeout",
			kind: turnKind{isHeartbeat: true, isDeepHeartbeat: true, fableTurn: true},
			want: ExecutionProfile{Model: fableModel, Effort: "high", Ephemeral: true, Timeout: deepHeartbeatTimeout, TaskType: "heartbeat_deep"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveExecutionProfile(r, c.kind)
			if got != c.want {
				t.Errorf("resolveExecutionProfile(%+v) = %+v, want %+v", c.kind, got, c.want)
			}
		})
	}
}
