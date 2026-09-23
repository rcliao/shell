// Doc budget (P3.5). A project doc is a working page, not a log: the research
// pass used to append and never cut, and one doc went from under 1 KB to over
// 100 KB in a month. The whole doc rides in every research prompt and comes
// back whole through doc-write, so the bloat slowed the turns that caused it
// until they timed out — and every human "tidy this up" request ended up
// removing close to half the page.
//
// The budget is soft and the rule is asymmetric on purpose: an over-budget
// doc may always get SMALLER, so the way out is never blocked; it just may
// not get bigger. Git history is the archive — cutting loses nothing.
package project

import (
	"fmt"
	"sort"
	"strings"
)

// DefaultDocBudget is the soft size limit for a project doc, in bytes. Sized
// from production: the docs people actually read stayed under ~10 KB, and the
// ones that drew "this is a mess" complaints were past 40 KB.
const DefaultDocBudget = 24 * 1024

// BudgetError is an agent write refused for growing an over-budget doc.
type BudgetError struct {
	Size, Prev, Budget int
	Largest            []SectionSize
}

// SectionSize is one section's share of a doc.
type SectionSize struct {
	Title string
	Bytes int
}

func (e *BudgetError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "doc is over budget: %s, budget %s, and this write makes it larger (was %s). ",
		kb(e.Size), kb(e.Budget), kb(e.Prev))
	b.WriteString("Consolidate before adding: keep decisions and current facts, cut superseded drafts, ")
	b.WriteString("revision logs and resolved to-dos (git history keeps them). A write that shrinks the doc is accepted.")
	if len(e.Largest) > 0 {
		b.WriteString(" Largest sections:")
		for _, s := range e.Largest {
			fmt.Fprintf(&b, " %q %s;", s.Title, kb(s.Bytes))
		}
	}
	return b.String()
}

// CheckBudget refuses next only when it is over budget AND larger than prev.
// budget <= 0 means DefaultDocBudget.
func CheckBudget(prev, next string, budget int) error {
	if budget <= 0 {
		budget = DefaultDocBudget
	}
	if len(next) <= budget || len(next) <= len(prev) {
		return nil
	}
	return &BudgetError{Size: len(next), Prev: len(prev), Budget: budget, Largest: LargestSections(next, 3)}
}

// LargestSections returns the n biggest sections of md, largest first.
func LargestSections(md string, n int) []SectionSize {
	_, sections := ParseDoc(md)
	sizes := make([]SectionSize, 0, len(sections))
	for _, s := range sections {
		sizes = append(sizes, SectionSize{Title: s.Title, Bytes: len(s.Raw)})
	}
	sort.SliceStable(sizes, func(i, j int) bool { return sizes[i].Bytes > sizes[j].Bytes })
	if len(sizes) > n {
		sizes = sizes[:n]
	}
	return sizes
}

// BudgetPromptLine is the size line for research and revision prompts. The
// prompt carries the rule so the weekly research pass doubles as the
// maintenance job; CheckBudget is the backstop for when the prompt is ignored.
func BudgetPromptLine(doc string, budget int) string {
	if budget <= 0 {
		budget = DefaultDocBudget
	}
	if len(doc) > budget {
		return fmt.Sprintf("- The doc is %s, OVER its %s budget. This pass is a consolidation pass FIRST: rewrite it shorter — keep decisions and current facts, cut superseded drafts, revision logs and resolved to-dos (git history keeps them). A write that grows the doc will be refused.\n",
			kb(len(doc)), kb(budget))
	}
	return fmt.Sprintf("- Doc size %s of a %s budget. Replace stale content rather than appending under it; the doc is a working page, not a log.\n",
		kb(len(doc)), kb(budget))
}

func kb(n int) string {
	return fmt.Sprintf("%.1f KB", float64(n)/1024)
}
