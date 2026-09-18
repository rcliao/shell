package project

import (
	"context"
	"strings"
	"testing"

	"github.com/rcliao/shell/internal/store"
)

func TestParseNotionPageID(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	cases := []struct{ in, want string }{
		{id, id},
		{"01234567-89ab-cdef-0123-456789abcdef", id},
		{"https://www.notion.so/" + id, id},
		{"https://www.notion.so/workspace/Family-Trip-2026-" + id, id},
		{"https://app.notion.com/p/workspace/Family-Trip-" + strings.ToUpper(id) + "?source=copy_link", id},
		// The query can carry OTHER ids (a view, a peeked page) — never pick those.
		{"https://www.notion.so/Trip-" + id + "?v=ffffffffffffffffffffffffffffffff&p=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", id},
		{"https://www.notion.so/Trip-" + id + "/", id},
		// A title ending in digits must not donate them to the id.
		{"https://www.notion.so/ws/Budget-20260918-" + id + "?pvs=4", id},
		{"https://www.notion.so/ws/Trip-2026-" + id, id},
	}
	for _, c := range cases {
		got, err := ParseNotionPageID(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseNotionPageID(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "https://www.notion.so/just-a-title", "not an id"} {
		if got, err := ParseNotionPageID(bad); err == nil {
			t.Errorf("ParseNotionPageID(%q) = %q, want an error", bad, got)
		}
	}
}

func TestAdoptedBlockMapRoundTrips(t *testing.T) {
	bm := AdoptedBlockMap([]NotionBlockRef{{ID: "b1"}, {ID: ""}, {ID: "b2"}})
	back := ParseBlockMap(bm.encode())
	if !back.Adopted || !back.Rendered() {
		t.Fatalf("round-trip lost the adopted flag or the poll targets: %+v", back)
	}
	if got := back.Sections[adoptedSection].Blocks; len(got) != 2 || got[0] != "b1" || got[1] != "b2" {
		t.Errorf("blocks = %v, want [b1 b2]", got)
	}
	// An empty page still adopts: page-level comments need no blocks.
	if empty := ParseBlockMap(AdoptedBlockMap(nil).encode()); !empty.Adopted || !empty.Rendered() {
		t.Errorf("empty adopted page must still be pollable: %+v", empty)
	}
	// Fail closed: a mangled flag must not turn a human's page into a
	// "rendered" one — the reserved section alone marks it adopted.
	if !ParseBlockMap(`{"sections":{"_adopted":{"blocks":["b1"]}},"adopted":"yes"}`).Adopted {
		t.Error("a type-mismatched adopted flag failed OPEN")
	}
	if !ParseBlockMap(`{"sections":{"_adopted":{"blocks":["b1"]}}}`).Adopted {
		t.Error("a missing adopted flag failed OPEN")
	}
	// A rendered map never reads as adopted.
	if ParseBlockMap(`{"sections":{"A":{"hash":"h","blocks":["x"]}}}`).Adopted {
		t.Error("a rendered map parsed as adopted")
	}
}

// The guard that protects a family's hand-made page: the renderer must not
// create, append, update or delete ANYTHING on an adopted page — whatever the
// canonical doc says, and even though the map looks "rendered".
func TestRendererNeverTouchesAdoptedPage(t *testing.T) {
	st, p := newSyncFixture(t)
	kind, ref := "notion", "human-page"
	bm := AdoptedBlockMap([]NotionBlockRef{{ID: "table-1"}, {ID: "todo-1"}}).encode()
	if err := st.UpdateProjectFields(p.Slug, store.ProjectFieldUpdate{
		ExportKind: &kind, ExportRef: &ref, BlockMap: &bm,
	}); err != nil {
		t.Fatal(err)
	}
	p = reload(t, st, p.Slug)

	api := &fakeNotion{enabled: true}
	if err := NewRenderer(api, "parent-page").SyncProjectPage(context.Background(), st, p, sampleDoc); err != nil {
		t.Fatal(err)
	}
	if n := api.mutations(); n != 0 {
		t.Fatalf("renderer made %d mutations on an adopted page (appends=%d deleted=%v pages=%d)",
			n, len(api.appends), api.deleted, len(api.pages))
	}
	if got := reload(t, st, p.Slug); got.BlockMap != bm {
		t.Errorf("adopted block map was rewritten: %s", got.BlockMap)
	}
}

func TestAdoptPageChecksAccessFirst(t *testing.T) {
	if _, _, err := AdoptPage(context.Background(), &fakeNotion{enabled: false}, "p"); err == nil {
		t.Error("adopt must fail without a token")
	}
	encoded, n, err := AdoptPage(context.Background(), &fakeNotion{enabled: true}, "p")
	if err != nil || n != 0 || !ParseBlockMap(encoded).Adopted {
		t.Errorf("AdoptPage = %q, %d, %v", encoded, n, err)
	}
}

func TestAdoptedCommentPromptForbidsDocWrite(t *testing.T) {
	p := AdoptedCommentPrompt("trip", "Trip", "", "zh", "page-1", "what time do we leave?", "blk-9")
	for _, want := range []string{"page-1", "blk-9", "what time do we leave?", "Do NOT use the project skill's doc-write", "touch nothing else", "zh"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}
