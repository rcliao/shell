package daemon

import (
	"encoding/json"
	"path/filepath"
)

// agentSandboxExcluded are the commands an agent's Bash sandbox lets run
// outside the sandbox. The browser skill starts Google Chrome, which the macOS
// sandbox stops from starting (37 of 52 sandboxed calls failed over 30 days,
// ~/.shell/evolve-reviews/browser-preferred-action-2026-10-05.md); the agents
// had taken to turning the sandbox off per call to get around it.
//
// A pipeline leaves the sandbox only if EVERY command in it is excluded, and
// agents pipe the browser through head/grep, so the read-only filters are
// listed too. They cannot write without a redirect, and a redirect keeps the
// whole call sandboxed regardless.
func agentSandboxExcluded(home string) []string {
	browser := "/.shell/skills/browser/scripts/browser"
	return []string{
		"~" + browser, "~" + browser + " *",
		filepath.Join(home, browser), filepath.Join(home, browser) + " *",
		"head", "head *", "tail *", "grep *", "cut *", "wc", "wc *",
	}
}

// agentSandbox returns the sandbox block for an agent's --settings file: a
// copy of the user's own block plus the agent exclusions. It copies the
// whole block because a `sandbox` key in a higher-precedence settings file
// may replace the user's block instead of merging with it; writing only
// excludedCommands could silently drop "enabled" and the network and
// filesystem rules. Returns nil when the user has no enabled sandbox (then
// there is nothing to exclude from). The input is never modified.
func agentSandbox(userSandbox any, home string) map[string]any {
	src, ok := userSandbox.(map[string]any)
	if !ok {
		return nil
	}
	if enabled, _ := src["enabled"].(bool); !enabled {
		return nil
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	var cmds []any
	seen := map[string]bool{}
	if prev, ok := out["excludedCommands"].([]any); ok {
		for _, c := range prev {
			if s, ok := c.(string); ok && !seen[s] {
				seen[s] = true
				cmds = append(cmds, s)
			}
		}
	}
	for _, c := range agentSandboxExcluded(home) {
		if !seen[c] {
			seen[c] = true
			cmds = append(cmds, c)
		}
	}
	out["excludedCommands"] = cmds
	return out
}
