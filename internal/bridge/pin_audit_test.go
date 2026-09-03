package bridge

import (
	"strings"
	"testing"
)

func mkRows(n, tok int) []pinRow {
	var out []pinRow
	for i := 0; i < n; i++ {
		out = append(out, pinRow{key: "pin", imp: 1.0 - float64(i)*0.1, tok: tok})
	}
	return out
}

// The retrieval half must report the cut the agent will ACTUALLY experience,
// and stay quiet when the pin set fits — a deep-beat prompt that always nags
// is a deep-beat prompt that gets ignored.
func TestPinAuditRetrievalCutAndSilence(t *testing.T) {
	none := systemCut{}
	if got := renderPinAudit(none, mkRows(2, 100), 250); got != "" {
		t.Errorf("pin set that fits should produce no block, got:\n%s", got)
	}
	// 2 admitted + 2 dropped is under pinAuditMinDropped: still silent.
	if got := renderPinAudit(none, mkRows(4, 100), 250); got != "" {
		t.Errorf("small overflow should stay silent, got:\n%s", got)
	}
	got := renderPinAudit(none, mkRows(8, 100), 250)
	if got == "" {
		t.Fatal("6 dropped pins should produce a block")
	}
	if !strings.Contains(got, "only the top 2 are visible to those calls") {
		t.Errorf("block should name the real admitted count; got:\n%s", got)
	}
	if !strings.Contains(got, "Below the retrieval cut (6)") {
		t.Errorf("block should name the real dropped count; got:\n%s", got)
	}
	if strings.Contains(got, "system prompt]") {
		t.Errorf("no system-prompt section when nothing is dropped there; got:\n%s", got)
	}
}

// One pin outside the system prompt is one rule the agent does not have:
// the system-prompt half has no minimum and must lead with the arithmetic
// and the dropped keys.
func TestPinAuditSystemPromptCutLeadsAndHasNoFloor(t *testing.T) {
	sys := systemCut{
		kept:    []pinRow{{key: "new-rule", tok: 900}, {key: "newer-rule", tok: 800}},
		dropped: []pinRow{{key: "old-health-rule", tok: 500}},
		budget:  2000,
	}
	got := renderPinAudit(sys, nil, 0)
	if got == "" {
		t.Fatal("a single dropped operating pin must produce the system-prompt section")
	}
	if !strings.HasPrefix(strings.TrimSpace(got), "---\n**[Pinned memory audit — system prompt]**") {
		t.Errorf("system-prompt section must come first; got:\n%s", got)
	}
	for _, want := range []string{"Budget 2000 tokens", "2 pins are in (1700 tok)", "1 pins are OUT (500 tok)", "old-health-rule (500 tok)", "newest-CREATED first", "ghost_consolidate"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "still in your system prompt") {
		t.Errorf("the block must never claim all pins are in the system prompt; got:\n%s", got)
	}
}

// Locked pins must be labelled, not silently listed as things to re-rank —
// curate refuses to touch them and an agent that tries just burns its beat.
func TestPinAuditFlagsLocked(t *testing.T) {
	rows := []pinRow{
		{key: "a", imp: 0.9, tok: 200}, {key: "b", imp: 0.8, tok: 200},
		{key: "c", imp: 0.7, tok: 200}, {key: "locked-one", imp: 0.1, tok: 200, locked: true},
	}
	got := renderPinAudit(systemCut{}, rows, 250)
	if !strings.Contains(got, "locked-one") || !strings.Contains(got, "[locked") {
		t.Errorf("locked pin below the cut should be labelled; got:\n%s", got)
	}
	sys := systemCut{dropped: []pinRow{{key: "locked-two", tok: 100, locked: true}}, budget: 100}
	if got := renderPinAudit(sys, nil, 0); !strings.Contains(got, "locked-two (100 tok) [locked") {
		t.Errorf("locked pin outside the system prompt should be labelled; got:\n%s", got)
	}
}
