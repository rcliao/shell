// Package tool provides a unified registry for all agent capabilities:
// MCP tools, skill scripts, and user commands.
package tool

import (
	"fmt"
	"strings"
)

// Kind distinguishes how a tool is invoked.
type Kind string

const (
	KindMCP     Kind = "mcp"     // Claude calls via MCP protocol
	KindSkill   Kind = "skill"   // Claude calls via Bash scripts
	KindCommand Kind = "command" // User calls via /command in Telegram
)

// Tool describes a registered capability.
type Tool struct {
	Name         string
	Description  string
	Kind         Kind
	AllowedTools []string // tools to auto-approve (for MCP/skills)
}

// Registry holds all tools from all sources.
type Registry struct {
	tools  []Tool
	byName map[string]Tool
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{
		byName: make(map[string]Tool),
	}
}

// Register adds a tool to the registry. Overwrites if name exists.
func (r *Registry) Register(t Tool) {
	if _, exists := r.byName[t.Name]; !exists {
		r.tools = append(r.tools, t)
	} else {
		for i, existing := range r.tools {
			if existing.Name == t.Name {
				r.tools[i] = t
				break
			}
		}
	}
	r.byName[t.Name] = t
}

// All returns all registered tools.
func (r *Registry) All() []Tool {
	return r.tools
}

// ByKind returns all tools of a given kind.
func (r *Registry) ByKind(k Kind) []Tool {
	var result []Tool
	for _, t := range r.tools {
		if t.Kind == k {
			result = append(result, t)
		}
	}
	return result
}

// Has returns true if a tool with the given name is registered.
func (r *Registry) Has(name string) bool {
	_, ok := r.byName[name]
	return ok
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// Count returns the number of registered tools.
func (r *Registry) Count() int {
	return len(r.tools)
}

// AllowedTools returns all tools that should be auto-approved,
// aggregated from all registered tools.
func (r *Registry) AllowedTools() []string {
	var result []string
	for _, t := range r.tools {
		result = append(result, t.AllowedTools...)
	}
	return result
}

// SystemPrompt generates the system prompt section describing all
// MCP tools and skill scripts available to the agent.
func (r *Registry) SystemPrompt() string {
	mcpTools := r.ByKind(KindMCP)
	skills := r.ByKind(KindSkill)

	if len(mcpTools) == 0 && len(skills) == 0 {
		return ""
	}

	var sb strings.Builder

	if len(mcpTools) > 0 {
		sb.WriteString("\n\n## MCP Tools\n\n")
		sb.WriteString("These tools are available as native MCP tool calls:\n\n")
		for _, t := range mcpTools {
			sb.WriteString(fmt.Sprintf("- **%s** — %s\n", t.Name, t.Description))
		}
	}

	if len(skills) > 0 {
		sb.WriteString("\n\n## Skill Scripts\n\n")
		sb.WriteString("These skills are available via Bash:\n\n")
		for _, t := range skills {
			sb.WriteString(fmt.Sprintf("- **%s** — %s\n", t.Name, t.Description))
		}
	}

	return sb.String()
}
