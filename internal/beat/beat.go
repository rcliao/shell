// Package beat carries per-fire heartbeat metadata from the scheduler to the
// bridge.
//
// The scheduler knows a beat's job_runs id and its persisted counter; the
// bridge is where the turn's output is captured into the reflection journal.
// Nothing in between needs those values, and the path crosses
// HandleMessageStreaming — a function called from many places that has no
// business growing heartbeat-shaped parameters.
//
// So this is request-scoped metadata crossing an API boundary without changing
// intermediate signatures, which is the case context values are actually for.
// The unexported key type keeps it collision-free, and From() degrades to a
// zero Meta so a turn that arrives without metadata still works.
package beat

import "context"

// Meta describes one heartbeat fire.
type Meta struct {
	// RunID is the job_runs row for this fire. Zero when the beat did not
	// come from the scheduler (a manual or test invocation).
	RunID int64
	// Count is the persisted heartbeat counter at fire time — the value the
	// deep-reflection cadence is derived from, so a journal row can be placed
	// in the sequence rather than guessed at by timestamp.
	Count int
	// Deep marks a deep-reflection beat.
	Deep bool
}

type ctxKey struct{}

// With returns a context carrying m.
func With(ctx context.Context, m Meta) context.Context {
	return context.WithValue(ctx, ctxKey{}, m)
}

// From extracts heartbeat metadata, returning the zero Meta when absent.
func From(ctx context.Context) Meta {
	if ctx == nil {
		return Meta{}
	}
	if m, ok := ctx.Value(ctxKey{}).(Meta); ok {
		return m
	}
	return Meta{}
}
