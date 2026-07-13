package bridge

import "testing"

// fakeResolver maps task_type → model, mirroring config.ClaudeConfig.ResolveModel.
type fakeResolver map[string]string

func (f fakeResolver) ResolveModel(taskType string) string { return f[taskType] }
func (f fakeResolver) ResolveEffort(taskType string) string { return f[taskType+"_effort"] }

func TestResolveExecutionProfile(t *testing.T) {
	r := fakeResolver{
		"conversation":   "claude-opus-4-8",
		"heartbeat":      "claude-sonnet-5",
		"heartbeat_deep": "claude-opus-4-8",
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
			name: "light heartbeat → its model, persistent, no effort",
			kind: turnKind{isHeartbeat: true},
			want: ExecutionProfile{Model: "claude-sonnet-5", Effort: "", Ephemeral: false, TaskType: "heartbeat"},
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
