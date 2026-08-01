package beat

import (
	"context"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	ctx := With(context.Background(), Meta{RunID: 42, Count: 18, Deep: true})
	got := From(ctx)
	if got.RunID != 42 || got.Count != 18 || !got.Deep {
		t.Fatalf("round-trip lost data: %+v", got)
	}
}

// A turn with no metadata must still work — a manually invoked beat is
// unlinked, not broken.
func TestMissingMetaIsZeroNotPanic(t *testing.T) {
	if got := From(context.Background()); got != (Meta{}) {
		t.Fatalf("want zero Meta for a bare context, got %+v", got)
	}
	if got := From(nil); got != (Meta{}) { //nolint:staticcheck // nil ctx is the guarded case
		t.Fatalf("want zero Meta for a nil context, got %+v", got)
	}
}

// Metadata must survive the derived contexts the bridge builds — it wraps the
// scheduler's context in WithCancel for preemption, and re-derives with
// timeouts. If values were lost there, capture would silently record zeros,
// which is exactly the gap this package closes.
func TestMetaSurvivesDerivedContexts(t *testing.T) {
	ctx := With(context.Background(), Meta{RunID: 7, Count: 3, Deep: true})

	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if got := From(cancelCtx); got.RunID != 7 {
		t.Errorf("lost through WithCancel: %+v", got)
	}

	valCtx := context.WithValue(cancelCtx, struct{ other int }{}, "unrelated")
	if got := From(valCtx); got.Count != 3 {
		t.Errorf("lost through an unrelated WithValue: %+v", got)
	}

	if got := From(context.WithoutCancel(valCtx)); !got.Deep {
		t.Errorf("lost through WithoutCancel (the detached-turn path): %+v", got)
	}
}

// The key is unexported and typed, so an outside package storing its own value
// under a lookalike key cannot collide with ours.
func TestKeyIsCollisionFree(t *testing.T) {
	type otherKey struct{}
	ctx := context.WithValue(context.Background(), otherKey{}, Meta{RunID: 99})
	if got := From(ctx); got.RunID != 0 {
		t.Fatalf("a foreign key leaked into our lookup: %+v", got)
	}
}
