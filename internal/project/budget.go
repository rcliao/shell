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
	"unicode"
	"unicode/utf8"
)

// DefaultDocBudget is the soft size limit for a project doc, in bytes. Sized
// from production: the docs people actually read stayed under ~10 KB, and the
// ones that drew "this is a mess" complaints were past 40 KB.
const DefaultDocBudget = 24 * 1024

// The update log gets its own cap. When the doc budget first fired in
// production, 99% of a 101 KB doc was this one section — dated entries
// appended daily and never trimmed — while goals, constraints and open items
// had not changed in a month. A doc under its total budget can still be
// mostly log, so the log is bounded separately.
const (
	LogSection = "更新紀錄"
	LogBudget  = 8 * 1024
)

// DecisionsSection holds dated one-line decisions: the project's shared
// memory. Consolidation must never cut it (the skill text says so; nothing
// here caps it).
const DecisionsSection = "決定"

// BudgetError is an agent write refused for growing an over-budget doc, or —
// when Section is set — an over-budget section of it.
type BudgetError struct {
	Size, Prev, Budget int
	Section            string // "" = the whole doc
	Largest            []SectionSize
}

// SectionSize is one section's share of a doc.
type SectionSize struct {
	Title string
	Bytes int
}

func (e *BudgetError) Error() string {
	var b strings.Builder
	if e.Section != "" {
		fmt.Fprintf(&b, "section %q is over its cap: %s, cap %s, and this write makes it larger (was %s). ",
			e.Section, kb(e.Size), kb(e.Budget), kb(e.Prev))
		b.WriteString("It is a log, not the doc: keep the most recent entries in full, fold older ones into ONE dated summary line ")
		fmt.Fprintf(&b, "(git history keeps the detail), and move anything that was decided into %q. ", DecisionsSection)
		b.WriteString("A write that shrinks the section is accepted.")
		return b.String()
	}
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
	if len(next) > budget && len(next) > len(prev) {
		return &BudgetError{Size: len(next), Prev: len(prev), Budget: budget, Largest: LargestSections(next, 3)}
	}
	// Same asymmetry for the log, plus one more way out: the cap only bites
	// when the WHOLE doc grew too. A consolidation that shrinks the doc while
	// adding its own "consolidated today" line, or one that renames a
	// variant heading this scanner could not measure before, is a step in the
	// right direction and is never refused. What is refused is the plain
	// append: more log, bigger doc.
	if nl, pl := SectionBytes(next, LogSection), SectionBytes(prev, LogSection); nl > LogBudget && nl > pl && len(next) > len(prev) {
		return &BudgetError{Size: nl, Prev: pl, Budget: LogBudget, Section: LogSection}
	}
	return nil
}

// rawSection is one "## " section as written: its heading text and the lines
// under it.
type rawSection struct {
	Title string
	Body  []string
}

// scanSections splits md at "## " headings, ignoring any inside a fenced code
// block. Deliberately NOT ParseDoc: that parser feeds the renderer's section
// hashes, renames duplicate headings ("x #2"), and has no fence awareness —
// all fine for rendering, all wrong for measuring. A briefing log is exactly
// where a fenced snippet containing "## " shows up.
func scanSections(md string) []rawSection {
	var out []rawSection
	inFence := false
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "## ") {
			out = append(out, rawSection{Title: strings.TrimSpace(strings.TrimPrefix(line, "## "))})
			continue
		}
		if len(out) > 0 {
			out[len(out)-1].Body = append(out[len(out)-1].Body, line)
		}
	}
	return out
}

// sectionsNamed returns every section whose heading names title (see
// headingNamed). Not plain equality: live docs decorate headings ("📝 更新紀錄", "更新紀錄 (log)"), and
// a doc can carry the same heading twice. Measuring only an exact first match
// reads such a section as absent — and then a write that merges or renames it
// looks like growth from zero.
func sectionsNamed(md, title string) []rawSection {
	var out []rawSection
	for _, sec := range scanSections(md) {
		if headingNamed(sec.Title, title) {
			out = append(out, sec)
		}
	}
	return out
}

// headingNamed reports whether a "## " heading names title: equal, or title
// decorated on either side by non-letters ("📝 更新紀錄", "更新紀錄 (log)").
// A boundary is required because 待決定 contains 決定 — a substring match
// would file open questions under decisions.
func headingNamed(heading, title string) bool {
	heading = strings.TrimSpace(heading)
	for from := 0; ; {
		i := strings.Index(heading[from:], title)
		if i < 0 {
			return false
		}
		i += from
		before, _ := utf8.DecodeLastRuneInString(heading[:i])
		after, _ := utf8.DecodeRuneInString(heading[i+len(title):])
		if (i == 0 || !unicode.IsLetter(before)) && (i+len(title) == len(heading) || !unicode.IsLetter(after)) {
			return true
		}
		from = i + len(title)
	}
}

// SectionBytes is the total size of the section(s) named title in md — heading
// lines included, duplicates summed — and 0 when absent.
func SectionBytes(md, title string) int {
	n := 0
	for _, sec := range sectionsNamed(md, title) {
		n += len("## "+sec.Title) + 1
		for _, l := range sec.Body {
			n += len(l) + 1
		}
	}
	return n
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
