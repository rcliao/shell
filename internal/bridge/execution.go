package bridge

import "time"

const (
	// deepHeartbeatEffort: the deep-reflection heartbeat's reasoning effort. Set
	// to "high" (not "max") — max effort on opus reliably overran the 5m timeout
	// and produced nothing; "high" is much deeper than a normal turn yet finishes.
	deepHeartbeatEffort = "high"
	// deepHeartbeatTimeout: background reflection isn't user-facing, so it gets a
	// longer budget than the 5m user-turn timeout as a safety margin.
	deepHeartbeatTimeout = 12 * time.Minute
)

// ExecutionProfile is the resolved per-turn execution decision: which model, at
// what reasoning effort, and whether the turn runs as an isolated one-shot
// (ephemeral, fresh spawn) or on the chat's persistent session. It exists so the
// model / effort / persistence decision lives in ONE typed place instead of
// scattered `if isDeepHeartbeat` / `if fableTurn` branches across the send path.
// See docs/MODEL-SESSION-CONFIG.md (Layer 1: Execution Profile).
type ExecutionProfile struct {
	Model     string        // resolved model for this turn
	Effort    string        // "" = CLI default; "high" for deep reflection
	Ephemeral bool          // one-shot fresh spawn that never mutates the persistent session
	Timeout   time.Duration // per-request timeout override (0 = manager default); honored on ephemeral spawns
	TaskType  string        // for cost attribution / logging (conversation|heartbeat|heartbeat_deep)
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
		p.Effort = deepHeartbeatEffort
		p.Ephemeral = true
		p.Timeout = deepHeartbeatTimeout
	}
	if k.fableTurn {
		p.Model = fableModel
		p.Ephemeral = true
	}
	return p
}
