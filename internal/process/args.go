package process

import "strings"

// claudeArgOpts carries the Manager-level CLI settings that both spawn paths
// (persistent and one-shot) share, so buildClaudeArgs can be a pure, testable
// function independent of a live Manager.
type claudeArgOpts struct {
	defaultModel   string
	allowedTools   []string
	settingSources []string
	settingsPath   string
	permissionMode string
	mcpConfigPath  string
	extraArgs      []string
}

func (m *Manager) claudeArgOpts() claudeArgOpts {
	return claudeArgOpts{
		defaultModel:   m.model,
		allowedTools:   m.allowedTools,
		settingSources: m.settingSources,
		settingsPath:   m.settingsPath,
		permissionMode: m.permissionMode,
		mcpConfigPath:  m.mcpConfigPath,
		extraArgs:      m.extraArgs,
	}
}

// buildClaudeArgs constructs the `claude` CLI argv for a request. It is the
// single source of truth for BOTH the persistent (spawnPersistent) and one-shot
// (runClaudeBidirectional) spawn paths — previously duplicated, and the sole
// difference (persistent silently dropped --effort) was the root of the
// deep-heartbeat effort bug. It returns the resolved model so the caller can
// record it on the proc for the mismatch check.
//
// Spawn-time bindings: --model, --effort, and --append-system-prompt take effect
// only at spawn. A resumed session (--resume) already carries its system prompt
// in history, so we skip it there. Effort is honored whenever the caller sets
// it; the bridge is responsible for only setting Effort on requests that spawn
// fresh (ephemeral turns) — see docs/MODEL-SESSION-CONFIG.md.
func buildClaudeArgs(req AgentRequest, opts claudeArgOpts) (args []string, resolvedModel string) {
	args = []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
	}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	model := req.Model
	if model == "" {
		model = opts.defaultModel
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	// Only append system prompt on fresh sessions — resumed sessions already
	// have the system prompt in their conversation history.
	if req.SystemPrompt != "" && req.SessionID == "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}
	if len(opts.allowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(opts.allowedTools, ","))
	}
	if len(opts.settingSources) > 0 {
		args = append(args, "--setting-sources", strings.Join(opts.settingSources, ","))
	}
	if opts.settingsPath != "" {
		args = append(args, "--settings", opts.settingsPath)
	}
	args = append(args, "--permission-mode", opts.permissionMode)
	if opts.mcpConfigPath != "" {
		args = append(args, "--mcp-config", opts.mcpConfigPath)
	}
	args = append(args, opts.extraArgs...)
	return args, model
}
