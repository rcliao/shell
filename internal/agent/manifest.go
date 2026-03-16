// Package agent provides agent manifest definitions and multi-agent routing.
// An agent manifest is a declarative file that defines an agent's identity,
// provider, tools, memory, and chat routing.
package agent

// Manifest defines a complete agent configuration.
type Manifest struct {
	Name         string       `yaml:"name" json:"name"`                   // unique agent ID
	Description  string       `yaml:"description" json:"description"`     // one-line description
	Provider     ProviderSpec `yaml:"provider" json:"provider"`           // LLM backend config
	SystemPrompt string       `yaml:"system_prompt" json:"system_prompt"` // core identity/personality
	Skills       []string     `yaml:"skills" json:"skills"`               // skill names to enable
	Tools        ToolsSpec    `yaml:"tools" json:"tools"`                 // tool configuration
	Memory       MemorySpec   `yaml:"memory" json:"memory"`               // memory configuration
	Routing      RoutingSpec  `yaml:"routing" json:"routing"`             // which chats this agent serves
}

// ProviderSpec configures the LLM provider.
type ProviderSpec struct {
	Kind        string   `yaml:"kind" json:"kind"`               // "claude-cli" (default), "anthropic-api", "openai", etc.
	Model       string   `yaml:"model" json:"model"`             // model name
	Binary      string   `yaml:"binary" json:"binary"`           // for CLI providers
	Timeout     string   `yaml:"timeout" json:"timeout"`         // duration string (e.g. "5m")
	MaxSessions int      `yaml:"max_sessions" json:"max_sessions"`
	WorkDir     string   `yaml:"work_dir" json:"work_dir"`
	ExtraArgs   []string `yaml:"extra_args" json:"extra_args"`
}

// ToolsSpec configures available tools.
type ToolsSpec struct {
	AllowedTools []string                `yaml:"allowed_tools" json:"allowed_tools"`
	MCPServers   map[string]MCPServerSpec `yaml:"mcp_servers" json:"mcp_servers"`
}

// MCPServerSpec defines an MCP server to connect to.
type MCPServerSpec struct {
	Command string   `yaml:"command" json:"command"`
	Args    []string `yaml:"args" json:"args"`
}

// MemorySpec configures agent memory.
type MemorySpec struct {
	Enabled    bool     `yaml:"enabled" json:"enabled"`
	AgentNS    string   `yaml:"agent_ns" json:"agent_ns"`     // namespace for this agent's memories
	Budget     int      `yaml:"budget" json:"budget"`           // token budget
	Namespaces []string `yaml:"namespaces" json:"namespaces"`   // additional readable namespaces
}

// RoutingSpec determines which chats this agent serves.
type RoutingSpec struct {
	ChatIDs []int64 `yaml:"chat_ids" json:"chat_ids"` // explicit chat ID binding
	Default bool    `yaml:"default" json:"default"`     // catch-all for unmatched chats
}
