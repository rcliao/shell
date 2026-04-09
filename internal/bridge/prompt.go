package bridge

import (
	"fmt"
	"time"
)

// skillsSystemPrompt returns the compact skills listing for the system prompt.
func (b *Bridge) skillsSystemPrompt() string {
	if b.skills == nil {
		return ""
	}
	prompt := b.skills.CatalogPrompt()
	prompt += "\n\n## Important: Use tools for bridge operations\n\n" +
		"Do NOT emit text directives like `[pm]`, `[tunnel]`, `[schedule]`, `[relay]`, " +
		"`[remember]`, `[heartbeat-learning]`, `[task-complete]`, or `[browser]` in your response. " +
		"These are deprecated.\n\n" +
		"For process management and tunnels, use the `shell_pm` and `shell_tunnel` MCP tools directly.\n" +
		"For scheduling, memory, relay, and tasks, use the corresponding skill scripts via Bash.\n\n" +
		"**CRITICAL:** NEVER run long-running processes (servers, watchers) directly via Bash " +
		"(e.g. `python3 server.py &`, `node app.js &`, `nohup ...`). They will become orphaned. " +
		"Always use the `shell_pm` tool to start them so they are properly tracked, have logs, and can be stopped.\n"
	return prompt
}

// skillOverrides returns true if a skill with the given name is loaded,
// meaning the built-in directive should be suppressed in favor of the skill.
func (b *Bridge) skillOverrides(name string) bool {
	return b.skills != nil && b.skills.Has(name)
}

// timestampSystemPrompt returns the current timestamp so the agent always
// knows what time it is, regardless of whether the scheduler is enabled.
func (b *Bridge) timestampSystemPrompt() string {
	tz := b.schedulerTZ
	if tz == "" {
		tz = "UTC"
	}
	loc, _ := time.LoadLocation(tz)
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	return "\n\n## Current Time\n\n" +
		"**Now:** " + now.Format("Monday, 2006-01-02 15:04:05 -07:00") + " (" + tz + ")\n" +
		"Use this as the authoritative current time. Ignore any other date references that may conflict.\n"
}

// injectCurrentTime prepends a precise timestamp to the user message when the
// scheduler is enabled, so Claude always knows the exact current time for
// computing relative schedule expressions like "in 30 minutes".
func (b *Bridge) injectCurrentTime(msg string) string {
	if !b.schedulerEnabled {
		return msg
	}
	loc, _ := time.LoadLocation(b.schedulerTZ)
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	return fmt.Sprintf("[Current time: %s (%s)]\n%s", now.Format(time.RFC3339), b.schedulerTZ, msg)
}
