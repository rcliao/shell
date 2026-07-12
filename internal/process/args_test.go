package process

import "testing"

// flagValue returns the argument following the first occurrence of flag, and
// whether the flag was present.
func flagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
	}
	return "", false
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// The keystone regression: a deep-heartbeat-shaped request (fresh, distinct
// model, effort=max) must emit --effort max, --model, and --append-system-prompt.
// Before the fix, --effort was dropped whenever the turn reused the persistent
// process, so max-effort was applied only by accident.
func TestBuildClaudeArgs_DeepHeartbeatEmitsEffortModelAndSystemPrompt(t *testing.T) {
	req := AgentRequest{Model: "claude-opus-4-8", Effort: "max", SystemPrompt: "you are umbreon"}
	args, model := buildClaudeArgs(req, claudeArgOpts{defaultModel: "claude-sonnet-5", permissionMode: "default"})

	if model != "claude-opus-4-8" {
		t.Fatalf("resolved model = %q, want claude-opus-4-8", model)
	}
	if v, ok := flagValue(args, "--effort"); !ok || v != "max" {
		t.Errorf("--effort = %q (present=%v), want max", v, ok)
	}
	if v, ok := flagValue(args, "--model"); !ok || v != "claude-opus-4-8" {
		t.Errorf("--model = %q (present=%v), want claude-opus-4-8", v, ok)
	}
	if v, ok := flagValue(args, "--append-system-prompt"); !ok || v != "you are umbreon" {
		t.Errorf("--append-system-prompt = %q (present=%v)", v, ok)
	}
}

// A normal conversation turn sets no Effort → no --effort flag.
func TestBuildClaudeArgs_NoEffortWhenUnset(t *testing.T) {
	req := AgentRequest{Model: "claude-sonnet-5"}
	args, _ := buildClaudeArgs(req, claudeArgOpts{permissionMode: "default"})
	if hasFlag(args, "--effort") {
		t.Errorf("--effort must be absent when Effort is unset; got %v", args)
	}
}

// A resumed session must NOT re-send the system prompt (it's already in history),
// and must carry --resume.
func TestBuildClaudeArgs_ResumeSkipsSystemPrompt(t *testing.T) {
	req := AgentRequest{SessionID: "sess-123", SystemPrompt: "sys"}
	args, _ := buildClaudeArgs(req, claudeArgOpts{permissionMode: "default"})
	if v, ok := flagValue(args, "--resume"); !ok || v != "sess-123" {
		t.Errorf("--resume = %q (present=%v), want sess-123", v, ok)
	}
	if hasFlag(args, "--append-system-prompt") {
		t.Errorf("resumed session must not re-append system prompt; got %v", args)
	}
}

// Model falls back to the manager default when the request leaves it empty.
func TestBuildClaudeArgs_ModelDefaultFallback(t *testing.T) {
	req := AgentRequest{}
	args, model := buildClaudeArgs(req, claudeArgOpts{defaultModel: "claude-sonnet-5", permissionMode: "default"})
	if model != "claude-sonnet-5" {
		t.Errorf("resolved model = %q, want default claude-sonnet-5", model)
	}
	if v, _ := flagValue(args, "--model"); v != "claude-sonnet-5" {
		t.Errorf("--model = %q, want claude-sonnet-5", v)
	}
}

// Foot-gun S5 documented as behavior: empty request model AND empty default →
// no --model flag at all (the CLI silently picks its own default). This test
// pins the current behavior so a future guard can flip it deliberately.
func TestBuildClaudeArgs_EmptyModelEmitsNoModelFlag(t *testing.T) {
	req := AgentRequest{}
	args, model := buildClaudeArgs(req, claudeArgOpts{permissionMode: "default"})
	if model != "" {
		t.Errorf("resolved model = %q, want empty", model)
	}
	if hasFlag(args, "--model") {
		t.Errorf("no --model flag expected when both request and default are empty; got %v", args)
	}
}
