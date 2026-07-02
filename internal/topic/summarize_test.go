package topic

import (
	"testing"
	"time"
)

func TestExtractCommitments(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name       string
		input      string
		wantN      int
		wantDueAt  time.Time // approximate; we check day only
	}{
		{
			name:  "in N days",
			input: "I'll check the leaves in 5 days, then we look at roots.",
			wantN: 1, wantDueAt: now.AddDate(0, 0, 5),
		},
		{
			name:  "by ISO date",
			input: "let's revisit by 2026-06-14 to see if soil dried.",
			wantN: 1, wantDueAt: time.Date(2026, 6, 14, 0, 0, 0, 0, time.Local),
		},
		{
			name:  "tomorrow",
			input: "I'll check back tomorrow morning to see how leaves look.",
			wantN: 1, wantDueAt: now.AddDate(0, 0, 1),
		},
		{
			name:  "no commitment (no date)",
			input: "interesting question; let me think about it",
			wantN: 0,
		},
		{
			name:  "multiple commitments",
			input: "I'll check in 5 days. We'll repot by next week if needed.",
			wantN: 2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractCommitments(c.input)
			if len(got) != c.wantN {
				t.Errorf("got %d commitments, want %d: %+v", len(got), c.wantN, got)
				return
			}
			if c.wantN > 0 && !c.wantDueAt.IsZero() {
				delta := got[0].DueAt.Sub(c.wantDueAt)
				if delta < -24*time.Hour || delta > 24*time.Hour {
					t.Errorf("first commitment due %v, want ~%v (delta %v)", got[0].DueAt, c.wantDueAt, delta)
				}
			}
		})
	}
}

func TestSummarizeFromTurn(t *testing.T) {
	priorEmpty := ""
	response := "Sounds like overwatering. Check soil 2 inches below surface. If wet, let it dry — no water for 5 days."
	summary := SummarizeFromTurn(priorEmpty, response)
	if summary == "" {
		t.Errorf("expected non-empty summary")
	}
	// Updated summary should mention "soil" or "water" (high-score tokens).
	low := summary
	hasSignal := false
	for _, kw := range []string{"soil", "water", "check", "dry"} {
		if contains(low, kw) {
			hasSignal = true
			break
		}
	}
	if !hasSignal {
		t.Errorf("expected summary to contain action token; got %q", summary)
	}

	// Subsequent turn appends.
	prior := summary
	updated := SummarizeFromTurn(prior, "Confirmed wet 2 inches down. Logging followup for 5/14.")
	if !contains(updated, " | ") {
		t.Errorf("expected | separator joining prior and new summary; got %q", updated)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			a, b := haystack[i+j], needle[j]
			if a >= 'A' && a <= 'Z' {
				a += 32
			}
			if b >= 'A' && b <= 'Z' {
				b += 32
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
