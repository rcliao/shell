package daemon

import (
	"encoding/json"
	"testing"
)

func TestAgentSandboxCopiesUserBlockAndAddsBrowser(t *testing.T) {
	var user map[string]any
	json.Unmarshal([]byte(`{"enabled": true,
		"filesystem": {"allowWrite": ["/tmp"]},
		"network": {"allowedDomains": ["github.com"]},
		"excludedCommands": ["docker *", "head *"]}`), &user)

	got := agentSandbox(user, "/home/u")
	if got == nil {
		t.Fatal("nil for an enabled sandbox")
	}
	if got["enabled"] != true {
		t.Error("lost enabled")
	}
	if fs, _ := got["filesystem"].(map[string]any); fs == nil || len(fs["allowWrite"].([]any)) != 1 {
		t.Errorf("lost filesystem rules: %v", got["filesystem"])
	}
	if nw, _ := got["network"].(map[string]any); nw == nil {
		t.Error("lost network rules")
	}
	cmds := map[string]int{}
	for _, c := range got["excludedCommands"].([]any) {
		cmds[c.(string)]++
	}
	for _, want := range []string{"docker *", "~/.shell/skills/browser/scripts/browser *",
		"/home/u/.shell/skills/browser/scripts/browser *", "head *", "grep *"} {
		if cmds[want] != 1 {
			t.Errorf("excludedCommands[%q] = %d, want exactly 1 (%v)", want, cmds[want], cmds)
		}
	}
	// The user's settings map must not be modified.
	if n := len(user["excludedCommands"].([]any)); n != 2 {
		t.Errorf("user block mutated: %d entries", n)
	}
}

func TestAgentSandboxNilWhenUserHasNone(t *testing.T) {
	if agentSandbox(nil, "/h") != nil {
		t.Error("no sandbox block → want nil")
	}
	if agentSandbox(map[string]any{"enabled": false}, "/h") != nil {
		t.Error("disabled sandbox → want nil (nothing to exclude from)")
	}
}
