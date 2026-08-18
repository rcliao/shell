// Notion blocks → markdown inverse converter (P3 Wave D, plan "Feedback via
// Notion"). When a human edits the mirrored page directly, the reconciler
// reads the page back and folds the change into the canonical doc. KEPT
// MINIMAL on purpose — exactly render.go's subset (headings, bullets,
// paragraphs, bold, links); a section containing anything it cannot convert
// keeps its canonical text unchanged, with a note for the log.
package project

import (
	"strconv"
	"strings"
)

// invertibleTypes is the block set the inverse converter speaks — the mirror
// of what render.go emits.
var invertibleTypes = map[string]bool{
	"paragraph":          true,
	"heading_1":          true,
	"heading_2":          true,
	"heading_3":          true,
	"bulleted_list_item": true,
}

// ReconcileDoc folds the CURRENT page state into the canonical doc.
//
// Sections are compared semantically (converted block structure, not bytes):
// a section whose blocks match canonical keeps its canonical markdown
// verbatim, so formatting never churns on untouched sections. A differing
// section is replaced by markdown reconstructed from its page blocks; a
// section containing unconvertible blocks keeps canonical unchanged (noted).
// Section order and existence follow the PAGE — a human deleting or
// reordering sections is an edit like any other.
//
// changed=false means the page and the doc agree; merged is then the
// canonical text untouched.
func ReconcileDoc(canonical string, page []NotionBlockRef, footerID string) (merged string, changed bool, notes []string) {
	title, canonSections := ParseDoc(canonical)
	canonByTitle := make(map[string]DocSection, len(canonSections))
	for _, s := range canonSections {
		canonByTitle[s.Title] = s
	}

	secs := partitionPage(page, footerID)

	same := len(secs) == len(canonSections)
	pieces := make([]string, 0, len(secs)+1)
	for i, sec := range secs {
		canon, exists := canonByTitle[sec.title]
		inOrder := same && i < len(canonSections) && canonSections[i].Title == sec.title
		switch {
		case exists && !allInvertible(sec.blocks):
			// Unconvertible content — not ours to interpret. Keep canonical.
			pieces = append(pieces, canon.Raw)
			notes = append(notes, "kept canonical section "+strconv.Quote(sec.title)+": unconvertible block on page")
			if !inOrder {
				same = false
			}
		case exists && blocksEqualRefs(canon.Blocks, sec.blocks):
			pieces = append(pieces, canon.Raw)
			if !inOrder {
				same = false
			}
		default:
			same = false
			if !allInvertible(sec.blocks) {
				notes = append(notes, "dropped unconvertible blocks in new section "+strconv.Quote(sec.title))
			}
			pieces = append(pieces, sectionMarkdown(sec.blocks))
		}
	}
	if same {
		return canonical, false, notes
	}

	if title != "" {
		pieces = append([]string{"# " + title}, pieces...)
	}
	for i := range pieces {
		pieces[i] = strings.TrimRight(pieces[i], "\n \t")
	}
	return strings.Join(pieces, "\n\n") + "\n", true, notes
}

// pageSection is one heading_2-delimited run of page blocks.
type pageSection struct {
	title  string // heading text (markdown form); preambleSection before the first heading_2
	blocks []NotionBlockRef
}

// partitionPage splits page blocks into sections the way ParseDoc splits the
// doc: a heading_2 starts a section; blocks before the first one are the
// preamble. The footer and empty spacing paragraphs are dropped — canonical
// never renders either, so they must not read as edits. Duplicate titles get
// the same " #n" suffix ParseDoc applies, keeping the keys aligned.
func partitionPage(page []NotionBlockRef, footerID string) []pageSection {
	var secs []pageSection
	cur := pageSection{title: preambleSection}
	flush := func() {
		if cur.title == preambleSection && len(cur.blocks) == 0 {
			return
		}
		secs = append(secs, cur)
	}
	for _, b := range page {
		if b.ID != "" && b.ID == footerID {
			continue
		}
		if b.Type == "paragraph" && plainOf(b.Rich) == "" {
			continue
		}
		if b.Type == "heading_2" {
			flush()
			cur = pageSection{title: markdownFromRich(b.Rich), blocks: []NotionBlockRef{b}}
			continue
		}
		cur.blocks = append(cur.blocks, b)
	}
	flush()

	seen := map[string]int{}
	for i := range secs {
		n := seen[secs[i].title]
		seen[secs[i].title] = n + 1
		if n > 0 {
			secs[i].title += " #" + strconv.Itoa(n+1)
		}
	}
	return secs
}

// allInvertible reports whether every block is a type the converter speaks.
func allInvertible(blocks []NotionBlockRef) bool {
	for _, b := range blocks {
		if !invertibleTypes[b.Type] {
			return false
		}
	}
	return true
}

// sectionMarkdown reconstructs a section's markdown from its page blocks.
// Only reached for CHANGED sections, so the formatting is normalized, not a
// byte round-trip; unsupported block types are skipped (caller noted them).
func sectionMarkdown(blocks []NotionBlockRef) string {
	var lines []string
	sep := func() {
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = append(lines, "")
		}
	}
	for _, b := range blocks {
		text := markdownFromRich(b.Rich)
		switch b.Type {
		case "heading_1":
			sep()
			lines = append(lines, "# "+text, "")
		case "heading_2":
			lines = append(lines, "## "+text, "")
		case "heading_3":
			sep()
			lines = append(lines, "### "+text, "")
		case "bulleted_list_item":
			lines = append(lines, "- "+text)
		case "paragraph":
			if text == "" {
				continue
			}
			sep()
			lines = append(lines, strings.Split(text, "\n")...)
			lines = append(lines, "")
		}
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// markdownFromRich is parseInline's inverse: bold and links only, everything
// else plain. A run that is both link and bold renders as a link — matching
// the forward converter, which cannot parse bold inside links either.
func markdownFromRich(rich []NotionRichText) string {
	var b strings.Builder
	for _, r := range normalizeRuns(rich) {
		switch {
		case r.Link != "":
			b.WriteString("[" + r.Text + "](" + r.Link + ")")
		case r.Bold:
			b.WriteString("**" + r.Text + "**")
		default:
			b.WriteString(r.Text)
		}
	}
	return b.String()
}

// plainOf concatenates a rich run's plain text.
func plainOf(rich []NotionRichText) string {
	var b strings.Builder
	for _, r := range rich {
		b.WriteString(r.Text)
	}
	return b.String()
}

// normalizeRuns drops empty runs and merges adjacent runs with identical
// styling — Notion fragments text arbitrarily, so run boundaries carry no
// meaning and must not read as differences.
func normalizeRuns(rich []NotionRichText) []NotionRichText {
	var out []NotionRichText
	for _, r := range rich {
		if r.Text == "" {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Bold == r.Bold && out[n-1].Link == r.Link {
			out[n-1].Text += r.Text
			continue
		}
		out = append(out, r)
	}
	return out
}

// blocksEqualRefs compares a canonical section's converted blocks against the
// page's current blocks: same types, same normalized rich text.
func blocksEqualRefs(canon []NotionBlock, page []NotionBlockRef) bool {
	if len(canon) != len(page) {
		return false
	}
	for i := range canon {
		if canon[i].Type != page[i].Type {
			return false
		}
		a, b := normalizeRuns(canon[i].Rich), normalizeRuns(page[i].Rich)
		if len(a) != len(b) {
			return false
		}
		for j := range a {
			if a[j] != b[j] {
				return false
			}
		}
	}
	return true
}
