package project

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckBudget(t *testing.T) {
	small := "# T\n\n## A\n" + strings.Repeat("x", 100)
	big := "# T\n\n## A\n" + strings.Repeat("x", 3000) + "\n\n## B\n" + strings.Repeat("y", 500)
	bigger := big + strings.Repeat("z", 200)

	cases := []struct {
		name       string
		prev, next string
		refused    bool
	}{
		{"under budget, growing", small, small + "more", false},
		{"crossing the budget is refused", small, big, true},
		{"over budget and growing", big, bigger, true},
		{"over budget but shrinking is the way out", bigger, big, false},
		{"over budget, same size (edit in place)", big, big, false},
		{"back under budget", big, small, false},
	}
	for _, c := range cases {
		err := CheckBudget(c.prev, c.next, 2048)
		if (err != nil) != c.refused {
			t.Errorf("%s: err = %v, refused want %v", c.name, err, c.refused)
		}
	}

	var be *BudgetError
	if err := CheckBudget(small, big, 2048); !errors.As(err, &be) {
		t.Fatalf("want *BudgetError, got %v", err)
	}
	if len(be.Largest) == 0 || be.Largest[0].Title != "A" {
		t.Errorf("largest sections = %+v, want A first", be.Largest)
	}
	if msg := be.Error(); !strings.Contains(msg, "shrinks the doc is accepted") || !strings.Contains(msg, `"A"`) {
		t.Errorf("error must name the way out and the biggest section: %s", msg)
	}
}

func TestBudgetPromptLine(t *testing.T) {
	under := BudgetPromptLine(strings.Repeat("x", 1024), 4096)
	if !strings.Contains(under, "1.0 KB of a 4.0 KB budget") || strings.Contains(under, "OVER") {
		t.Errorf("under-budget line: %s", under)
	}
	over := BudgetPromptLine(strings.Repeat("x", 8192), 4096)
	if !strings.Contains(over, "OVER") || !strings.Contains(over, "consolidation pass FIRST") {
		t.Errorf("over-budget line: %s", over)
	}
	if got := BudgetPromptLine("x", 0); !strings.Contains(got, "24.0 KB") {
		t.Errorf("zero budget must fall back to the default: %s", got)
	}
}

// The production shape: a doc well under its total budget whose update log
// alone is oversized. The log may shrink, may be edited in place, and may not
// grow — and a doc with a small log is never bothered.
func TestCheckBudgetCapsTheLogSection(t *testing.T) {
	doc := func(logBytes int) string {
		return "# T\n\n## 目標\n\nship it\n\n## 決定\n\n- 2026-09-01 go with plan B\n\n## " + LogSection + "\n\n" + strings.Repeat("x", logBytes) + "\n"
	}
	small, atCap, over, overMore := doc(1000), doc(LogBudget-200), doc(LogBudget+1000), doc(LogBudget+1500)
	if len(overMore) > DefaultDocBudget {
		t.Fatalf("fixture must stay under the DOC budget to isolate the log rule (%d)", len(overMore))
	}

	cases := []struct {
		name       string
		prev, next string
		refused    bool
	}{
		{"small log grows", small, atCap, false},
		{"log crosses its cap", atCap, over, true},
		{"over-cap log grows", over, overMore, true},
		{"over-cap log shrinks (the way out)", overMore, over, false},
		{"over-cap log untouched while another section is edited", over, strings.Replace(over, "ship it", "ship it soon", 1), false},
		{"back under the cap", over, small, false},
	}
	for _, c := range cases {
		err := CheckBudget(c.prev, c.next, 0)
		if (err != nil) != c.refused {
			t.Errorf("%s: err = %v, refused want %v", c.name, err, c.refused)
		}
	}

	var be *BudgetError
	if err := CheckBudget(atCap, over, 0); !errors.As(err, &be) || be.Section != LogSection {
		t.Fatalf("want a BudgetError for the log section, got %v", err)
	}
	msg := be.Error()
	for _, want := range []string{LogSection, DecisionsSection, "ONE dated summary line", "shrinks the section is accepted"} {
		if !strings.Contains(msg, want) {
			t.Errorf("log refusal must say %q: %s", want, msg)
		}
	}
	if SectionBytes(small, "沒有這一節") != 0 {
		t.Error("an absent section must measure 0")
	}
}
