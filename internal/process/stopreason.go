package process

// Why a turn ended.
//
// Until now a turn's outcome was `(SendResult, error)` and nothing more: a
// model that declined, a run that hit a turn cap, and a transport failure all
// arrived as either a plain result or an error string, and callers that cared
// had to match on prose. That is the shape that lets "the agent said no" and
// "the subprocess died" get handled identically.
//
// Named after ACP's StopReason, which solves the same problem, but this is our
// own type — the bridge calls a Go interface, not a protocol.
//
// Note what this does NOT change: rotation is driven by token thresholds
// (`rotate_max_tokens` against Usage), not by observing a stop. StopMaxTokens
// records that a run was cut short; it is not the rotation trigger.
type StopReason string

const (
	// StopEndTurn is the normal case: the model finished what it was saying.
	StopEndTurn StopReason = "end_turn"
	// StopMaxTurns means the runtime cut the run off at its turn cap. The text
	// so far is real and worth delivering; it is simply incomplete.
	StopMaxTurns StopReason = "max_turns"
	// StopCancelled means the caller cancelled — context or drain. Distinct
	// from an error because nothing malfunctioned.
	StopCancelled StopReason = "cancelled"
	// StopError means the run failed: transport, subprocess, or a runtime
	// error the CLI reported.
	StopError StopReason = "error"
	// StopUnknown is for a subtype we do not recognise. Deliberately not
	// folded into StopEndTurn: a new runtime outcome should be visible as
	// unhandled rather than silently reported as success.
	StopUnknown StopReason = "unknown"
)

// stopReasonFor maps a `result` event onto a reason.
//
// The CLI reports outcome as a subtype plus an is_error flag. Known subtypes
// are mapped explicitly; anything else becomes StopUnknown rather than being
// assumed benign, so an unfamiliar outcome shows up in the ledger instead of
// passing as a normal turn.
func stopReasonFor(subtype string, isError bool) StopReason {
	switch subtype {
	case "success":
		return StopEndTurn
	case "error_max_turns":
		return StopMaxTurns
	case "error_during_execution":
		return StopError
	case "":
		// No subtype at all. Older CLI builds omit it; fall back to the flag.
		if isError {
			return StopError
		}
		return StopEndTurn
	default:
		if isError {
			return StopError
		}
		return StopUnknown
	}
}
