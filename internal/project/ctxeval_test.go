package project

import (
	"strings"
	"testing"
)

func TestContextEvalParts(t *testing.T) {
	doc := "# Trip\n\n## 目標\n\nBlossoms.\n\n## 現況\n\nlong research\n\n## 決定\n\n- Fly JAL\n\n## 待決定\n\n1. April?\n"
	ex := ContextExcerpt(doc)
	if !strings.Contains(ex, "Fly JAL") || !strings.Contains(ex, "April?") || strings.Contains(ex, "long research") {
		t.Fatalf("excerpt = %q", ex)
	}
	if p := ContextJudgePrompt("Trip", ex, "which airline?", "Let's compare ANA and JAL"); !strings.Contains(p, "reasked") {
		t.Fatal("prompt lacks verdicts")
	}
	v, why, err := ParseContextVerdict("sure:\n{\"verdict\": \"reasked\", \"why\": \"JAL was decided\"}\n")
	if err != nil || v != "reasked" || why == "" {
		t.Fatalf("%q %q %v", v, why, err)
	}
	if _, _, err := ParseContextVerdict(`{"verdict":"great"}`); err == nil {
		t.Fatal("unknown verdict accepted")
	}
}
