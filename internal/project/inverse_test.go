package project

import (
	"strings"
	"testing"
)

// pageFromDoc renders a canonical doc into the block refs a page read-back
// would return — the identity fixture for reconcile tests.
func pageFromDoc(doc string) []NotionBlockRef {
	_, sections := ParseDoc(doc)
	var refs []NotionBlockRef
	n := 0
	for _, sec := range sections {
		for _, b := range sec.Blocks {
			n++
			refs = append(refs, NotionBlockRef{ID: "b" + string(rune('0'+n)), Type: b.Type, Rich: b.Rich})
		}
	}
	return refs
}

const reconcileDoc = "# Demo\n\n## 目標\n\nfind a place\n\n## 選項\n\n- **option A** at [link](https://example.com)\n- option B\n"

func TestReconcileDocNoChange(t *testing.T) {
	page := pageFromDoc(reconcileDoc)
	merged, changed, notes := ReconcileDoc(reconcileDoc, page, "")
	if changed {
		t.Fatalf("identical page reported changed; merged=\n%s", merged)
	}
	if merged != reconcileDoc {
		t.Error("unchanged reconcile must return canonical verbatim")
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want none", notes)
	}
}

func TestReconcileDocFragmentedRunsAreNotEdits(t *testing.T) {
	// Notion fragments rich text arbitrarily; run boundaries must not diff.
	page := pageFromDoc(reconcileDoc)
	for i, ref := range page {
		if ref.Type == "paragraph" && len(ref.Rich) == 1 {
			text := ref.Rich[0].Text
			page[i].Rich = []NotionRichText{{Text: text[:4]}, {Text: text[4:]}}
		}
	}
	if _, changed, _ := ReconcileDoc(reconcileDoc, page, ""); changed {
		t.Error("re-fragmented runs must not read as an edit")
	}
}

func TestReconcileDocEditedBulletReplacesOnlyThatSection(t *testing.T) {
	page := pageFromDoc(reconcileDoc)
	for i, ref := range page {
		if plainOf(ref.Rich) == "option B" {
			page[i].Rich = []NotionRichText{{Text: "option B (cheaper)"}}
		}
	}
	merged, changed, _ := ReconcileDoc(reconcileDoc, page, "")
	if !changed {
		t.Fatal("edited bullet must report changed")
	}
	if !strings.Contains(merged, "- option B (cheaper)") {
		t.Errorf("merged missing the human edit:\n%s", merged)
	}
	// The untouched section keeps its canonical markdown verbatim.
	if !strings.Contains(merged, "## 目標\n\nfind a place") {
		t.Errorf("untouched section text churned:\n%s", merged)
	}
	// Bold/link inverse survives the reconstruction of the edited section.
	if !strings.Contains(merged, "**option A**") || !strings.Contains(merged, "[link](https://example.com)") {
		t.Errorf("inline markdown lost in reconstruction:\n%s", merged)
	}
	if !strings.HasPrefix(merged, "# Demo\n") {
		t.Errorf("title line lost:\n%s", merged)
	}
}

func TestReconcileDocUnconvertibleKeepsCanonical(t *testing.T) {
	page := pageFromDoc(reconcileDoc)
	// A human dropped a table into 選項 AND edited a bullet there — the whole
	// section stays canonical because we cannot represent the table.
	for i, ref := range page {
		if plainOf(ref.Rich) == "option B" {
			page[i].Rich = []NotionRichText{{Text: "option B edited"}}
		}
	}
	page = append(page, NotionBlockRef{ID: "tbl", Type: "table"})
	merged, changed, notes := ReconcileDoc(reconcileDoc, page, "")
	if changed {
		t.Fatalf("unconvertible section must not change canonical; merged=\n%s", merged)
	}
	if len(notes) == 0 || !strings.Contains(notes[0], "選項") {
		t.Errorf("notes = %v, want the kept section named", notes)
	}
}

func TestReconcileDocFooterAndSpacingIgnored(t *testing.T) {
	page := pageFromDoc(reconcileDoc)
	page = append(page, NotionBlockRef{ID: "footer-1", Type: "paragraph", Rich: []NotionRichText{{Text: pageFooter}}})
	// Human-inserted empty spacing paragraph.
	page = append(page[:2], append([]NotionBlockRef{{ID: "sp", Type: "paragraph"}}, page[2:]...)...)
	if _, changed, _ := ReconcileDoc(reconcileDoc, page, "footer-1"); changed {
		t.Error("footer + empty paragraphs must not read as edits")
	}
}

func TestReconcileDocDeletedSection(t *testing.T) {
	page := pageFromDoc(reconcileDoc)
	var kept []NotionBlockRef
	drop := false
	for _, ref := range page {
		if ref.Type == "heading_2" {
			drop = plainOf(ref.Rich) == "目標"
		}
		if !drop {
			kept = append(kept, ref)
		}
	}
	merged, changed, _ := ReconcileDoc(reconcileDoc, kept, "")
	if !changed {
		t.Fatal("deleted section must report changed")
	}
	if strings.Contains(merged, "目標") {
		t.Errorf("deleted section survived:\n%s", merged)
	}
	if !strings.Contains(merged, "## 選項") {
		t.Errorf("remaining section lost:\n%s", merged)
	}
}

func TestSectionForBlock(t *testing.T) {
	bm := ParseBlockMap(`{"sections":{"選項":{"hash":"h","blocks":["b1","b2"]},"_preamble":{"hash":"p","blocks":["b0"]}},"order":["_preamble","選項"]}`)
	if got := bm.SectionForBlock("b2"); got != "選項" {
		t.Errorf("SectionForBlock(b2) = %q", got)
	}
	if got := bm.SectionForBlock("b0"); got != "" {
		t.Errorf("preamble block must resolve to page-level, got %q", got)
	}
	if got := bm.SectionForBlock("nope"); got != "" {
		t.Errorf("unknown block must resolve to page-level, got %q", got)
	}
}
