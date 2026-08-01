package bridge

import (
	"strings"
	"testing"
)

// The block must report the cut the agent will ACTUALLY experience, and must
// stay quiet when the pin set fits — a deep-beat prompt that always nags is a
// deep-beat prompt that gets ignored.
func TestBuildPinAuditBlockCutAndSilence(t *testing.T) {
	// 100-token pins, 250-token budget => 2 admitted, rest below the cut.
	mk := func(n int) []pinRow {
		var out []pinRow
		for i := 0; i < n; i++ {
			out = append(out, pinRow{key: "pin", imp: 1.0 - float64(i)*0.1, tok: 100})
		}
		return out
	}
	if got := renderPinAudit(mk(2), 250); got != "" {
		t.Errorf("pin set that fits should produce no block, got:\n%s", got)
	}
	// 2 admitted + 2 dropped is under pinAuditMinDropped: still silent.
	if got := renderPinAudit(mk(4), 250); got != "" {
		t.Errorf("small overflow should stay silent, got:\n%s", got)
	}
	got := renderPinAudit(mk(8), 250)
	if got == "" {
		t.Fatal("6 dropped pins should produce a block")
	}
	if !strings.Contains(got, "only the top 2 are visible to those calls") {
		t.Errorf("block should name the real admitted count; got:\n%s", got)
	}
	if !strings.Contains(got, "Below the cut, never retrieved (6)") {
		t.Errorf("block should name the real dropped count; got:\n%s", got)
	}
}

// Locked pins must be labelled, not silently listed as things to re-rank —
// curate refuses to touch them and an agent that tries just burns its beat.
func TestBuildPinAuditBlockFlagsLocked(t *testing.T) {
	rows := []pinRow{
		{key: "a", imp: 0.9, tok: 200}, {key: "b", imp: 0.8, tok: 200},
		{key: "c", imp: 0.7, tok: 200}, {key: "locked-one", imp: 0.1, tok: 200, locked: true},
	}
	got := renderPinAudit(rows, 250)
	if !strings.Contains(got, "locked-one") || !strings.Contains(got, "[locked") {
		t.Errorf("locked pin below the cut should be labelled; got:\n%s", got)
	}
}
