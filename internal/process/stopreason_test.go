package process

import "testing"

func TestStopReasonMapsKnownSubtypes(t *testing.T) {
	for _, c := range []struct {
		subtype string
		isError bool
		want    StopReason
	}{
		{"success", false, StopEndTurn},
		{"error_max_turns", false, StopMaxTurns},
		{"error_during_execution", true, StopError},
		{"error_during_execution", false, StopError}, // subtype wins over the flag
	} {
		if got := stopReasonFor(c.subtype, c.isError); got != c.want {
			t.Errorf("stopReasonFor(%q, %v) = %q, want %q", c.subtype, c.isError, got, c.want)
		}
	}
}

// Older CLI builds omit the subtype. Falling back to is_error keeps those
// readable instead of classifying every turn as unknown.
func TestStopReasonFallsBackToTheErrorFlag(t *testing.T) {
	if got := stopReasonFor("", false); got != StopEndTurn {
		t.Errorf("no subtype, no error = %q, want end_turn", got)
	}
	if got := stopReasonFor("", true); got != StopError {
		t.Errorf("no subtype, error = %q, want error", got)
	}
}

// An unrecognised outcome must NOT read as success. If a runtime starts
// reporting something new, it should surface as unhandled rather than pass
// silently as a completed turn — that is the whole point of naming the reason.
func TestUnknownSubtypeIsNotReportedAsSuccess(t *testing.T) {
	got := stopReasonFor("error_context_window_exhausted", false)
	if got == StopEndTurn {
		t.Fatal("an unknown subtype was reported as a normal end_turn")
	}
	if got != StopUnknown {
		t.Errorf("got %q, want unknown", got)
	}
}
