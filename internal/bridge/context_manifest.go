package bridge

import (
	"context"

	"github.com/rcliao/shell/internal/skill"
)

// Context manifest (V2-H33/V2-H32 instrumentation): measures the composed
// system prompt (Channel A) component by component, from the LIVE bridge —
// exactly what the next fresh generation will receive via
// --append-system-prompt. Exposed over RPC so `shell context` reads truth
// from the running daemon rather than reconstructing it.
//
// Not included (outside shell's composition): the Claude CLI's own baseline
// prompt + built-in tool definitions, and MCP server tool schemas from
// mcp.json — those add to cache creation but are not shell-authored text.

// ContextComponent is one measured slice of the composed system prompt.
type ContextComponent struct {
	Name      string `json:"name"`
	Chars     int    `json:"chars"`
	EstTokens int    `json:"est_tokens"`
}

// ContextManifest returns the per-component sizes and the full composed text
// for the given chat's system prompt.
func (b *Bridge) ContextManifest(ctx context.Context, chatID int64) ([]ContextComponent, string) {
	var parts []ContextComponent
	full := ""
	add := func(name, s string) {
		parts = append(parts, ContextComponent{Name: name, Chars: len(s), EstTokens: skill.EstimateTokens(s)})
		full += s
	}
	add("identity", b.agentIdentity)
	if b.memory != nil {
		add("pinned_memories", b.memory.SystemPrompt(ctx, chatID))
	}
	add("timestamp", b.timestampSystemPrompt())
	add("skills_catalog", b.skillsSystemPrompt())
	add("session_lifecycle", b.sessionLifecyclePrompt())
	if b.transcript != nil {
		add("group_agent", b.groupAgentPrompt())
	}
	parts = append(parts, ContextComponent{Name: "TOTAL", Chars: len(full), EstTokens: skill.EstimateTokens(full)})
	return parts, full
}
