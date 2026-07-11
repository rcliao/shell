package bridge

// ExecutionProfile is the resolved per-turn execution decision: which model, at
// what reasoning effort, and whether the turn runs as an isolated one-shot
// (ephemeral, fresh spawn) or on the chat's persistent session. It exists so the
// model / effort / persistence decision lives in ONE typed place instead of
// scattered `if isDeepHeartbeat` / `if fableTurn` branches across the send path.
// See docs/MODEL-SESSION-CONFIG.md (Layer 1: Execution Profile).
type ExecutionProfile struct {
	Model     string // resolved model for this turn
	Effort    string // "" = CLI default; "max" for deep reflection
	Ephemeral bool   // one-shot fresh spawn that never mutates the persistent session
	TaskType  string // for cost attribution / logging (conversation|heartbeat|heartbeat_deep)
}

// turnKind classifies a turn enough to determine its execution profile.
type turnKind struct {
	isHeartbeat     bool
	isDeepHeartbeat bool
	fableTurn       bool
}

// modelResolver is the slice of config.ClaudeConfig that profile resolution
// needs, kept as an interface so resolveExecutionProfile is unit-testable
// without a full Bridge/config.
type modelResolver interface {
	ResolveModel(taskType string) string
}

// resolveExecutionProfile derives the ExecutionProfile from a turn's kind. This
// is the single source of truth for the per-turn model/effort/persistence
// decision.
//
// Invariants encoded here:
//   - Deep reflection runs at max effort AND must be ephemeral, because --effort
//     is only emitted on a fresh spawn (the persistent process ignores per-turn
//     effort). Ephemeral is the only way effort=max actually reaches the CLI.
//   - The fable keyword is a one-shot experiment on a distinct model, isolated
//     from the persistent session.
//   - Everything else is a normal conversation turn on the persistent session.
func resolveExecutionProfile(r modelResolver, k turnKind) ExecutionProfile {
	taskType := "conversation"
	switch {
	case k.isDeepHeartbeat:
		taskType = "heartbeat_deep"
	case k.isHeartbeat:
		taskType = "heartbeat"
	}

	p := ExecutionProfile{
		Model:    r.ResolveModel(taskType),
		TaskType: taskType,
	}
	if k.isDeepHeartbeat {
		p.Effort = "max"
		p.Ephemeral = true
	}
	if k.fableTurn {
		p.Model = fableModel
		p.Ephemeral = true
	}
	return p
}
