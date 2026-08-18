package project

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// fakeNotion implements NotionAPI in memory, recording every mutation.
type fakeNotion struct {
	enabled    bool
	nextID     int
	appends    []fakeAppend
	deleted    []string
	pages      []fakePage
	failAppend error // returned by the NEXT AppendBlocks call, then cleared
	failDelete error // returned by every DeleteBlock call while set
}

type fakeAppend struct {
	parent string
	after  string
	ids    []string
	blocks []NotionBlock
}

type fakePage struct {
	parent, title, icon, id string
}

func (f *fakeNotion) Enabled() bool { return f.enabled }

func (f *fakeNotion) CreatePage(_ context.Context, parent, title, icon string) (string, error) {
	f.nextID++
	id := fmt.Sprintf("page-%d", f.nextID)
	f.pages = append(f.pages, fakePage{parent, title, icon, id})
	return id, nil
}

func (f *fakeNotion) AppendBlocks(_ context.Context, parent string, blocks []NotionBlock, after string) ([]string, error) {
	if f.failAppend != nil {
		err := f.failAppend
		f.failAppend = nil
		return nil, err
	}
	ids := make([]string, 0, len(blocks))
	for range blocks {
		f.nextID++
		ids = append(ids, fmt.Sprintf("blk-%d", f.nextID))
	}
	f.appends = append(f.appends, fakeAppend{parent: parent, after: after, ids: ids, blocks: blocks})
	return ids, nil
}

func (f *fakeNotion) UpdateBlock(_ context.Context, _ string, _ NotionBlock) error { return nil }
func (f *fakeNotion) DeleteBlock(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return f.failDelete
}
func (f *fakeNotion) GetBlockChildren(_ context.Context, _ string) ([]NotionBlockRef, error) {
	return nil, nil
}
func (f *fakeNotion) GetPageLastEdited(_ context.Context, _ string) (time.Time, error) {
	return time.Time{}, nil
}
func (f *fakeNotion) ListComments(_ context.Context, _, _ string) ([]NotionComment, string, error) {
	return nil, "", nil
}
func (f *fakeNotion) CreateComment(_ context.Context, _ string, _ []NotionRichText) (string, error) {
	return "", nil
}
func (f *fakeNotion) Me(_ context.Context) (string, error) { return "bot-user", nil }

func (f *fakeNotion) mutations() int { return len(f.appends) + len(f.deleted) + len(f.pages) }

func newSyncFixture(t *testing.T) (*store.Store, *store.Project) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	p, err := st.CreateProject(store.Project{Title: "Demo Trip", Emoji: "🏝", ChatID: -100200300})
	if err != nil {
		t.Fatal(err)
	}
	return st, p
}

func reload(t *testing.T, st *store.Store, slug string) *store.Project {
	t.Helper()
	p, err := st.GetProjectBySlug(slug)
	if err != nil || p == nil {
		t.Fatalf("reload %q: %v", slug, err)
	}
	return p
}

func TestSyncCreatesPageAndMap(t *testing.T) {
	st, p := newSyncFixture(t)
	api := &fakeNotion{enabled: true}
	r := NewRenderer(api, "parent-page")

	if err := r.SyncProjectPage(context.Background(), st, p, sampleDoc); err != nil {
		t.Fatal(err)
	}

	if len(api.pages) != 1 {
		t.Fatalf("pages created = %d", len(api.pages))
	}
	pg := api.pages[0]
	if pg.parent != "parent-page" || pg.title != "Demo Trip" || pg.icon != "🏝" {
		t.Errorf("page = %+v", pg)
	}

	fresh := reload(t, st, p.Slug)
	if fresh.ExportKind != "notion" || fresh.ExportRef != pg.id {
		t.Errorf("export = %s:%s", fresh.ExportKind, fresh.ExportRef)
	}
	bm := ParseBlockMap(fresh.BlockMap)
	if !bm.Rendered() {
		t.Fatal("block map not rendered")
	}
	if len(bm.Order) != 4 {
		t.Errorf("order = %v", bm.Order)
	}
	if bm.Sections["目標"].Hash == "" || len(bm.Sections["目標"].Blocks) != 3 {
		t.Errorf("目標 entry = %+v", bm.Sections["目標"])
	}
	if bm.Footer == "" {
		t.Error("no footer block recorded")
	}
	// Footer is the LAST append and carries the maintenance note.
	last := api.appends[len(api.appends)-1]
	if len(last.blocks) != 1 || !strings.Contains(last.blocks[0].Rich[0].Text, "agent") {
		t.Errorf("last append is not the footer: %+v", last)
	}

	// Real Notion ids are dashed UUIDs; the URL strips the dashes.
	if url := NotionPageURL(*fresh); url != "https://notion.so/"+strings.ReplaceAll(pg.id, "-", "") {
		t.Errorf("url = %q", url)
	}
}

func TestSyncUnchangedDocTouchesNothing(t *testing.T) {
	st, p := newSyncFixture(t)
	api := &fakeNotion{enabled: true}
	r := NewRenderer(api, "parent-page")
	ctx := context.Background()

	if err := r.SyncProjectPage(ctx, st, p, sampleDoc); err != nil {
		t.Fatal(err)
	}
	before := api.mutations()
	if err := r.SyncProjectPage(ctx, st, reload(t, st, p.Slug), sampleDoc); err != nil {
		t.Fatal(err)
	}
	if api.mutations() != before {
		t.Errorf("unchanged doc caused %d extra mutations", api.mutations()-before)
	}
}

func TestSyncSurgicalEditReplacesOnlyThatSection(t *testing.T) {
	st, p := newSyncFixture(t)
	api := &fakeNotion{enabled: true}
	r := NewRenderer(api, "parent-page")
	ctx := context.Background()

	if err := r.SyncProjectPage(ctx, st, p, sampleDoc); err != nil {
		t.Fatal(err)
	}
	fresh := reload(t, st, p.Slug)
	oldGoal := ParseBlockMap(fresh.BlockMap).Sections["目標"]

	edited := strings.Replace(sampleDoc, "see the castle", "see the museum", 1)
	appendsBefore, deletedBefore := len(api.appends), len(api.deleted)
	if err := r.SyncProjectPage(ctx, st, fresh, edited); err != nil {
		t.Fatal(err)
	}

	// Exactly one append, placed after the section's own old last block.
	if got := len(api.appends) - appendsBefore; got != 1 {
		t.Fatalf("appends = %d, want 1", got)
	}
	ap := api.appends[len(api.appends)-1]
	if ap.after != oldGoal.Blocks[len(oldGoal.Blocks)-1] {
		t.Errorf("append after = %q, want old last block %q", ap.after, oldGoal.Blocks[len(oldGoal.Blocks)-1])
	}
	if ap.parent != fresh.ExportRef {
		t.Errorf("append parent = %q", ap.parent)
	}
	// The old section blocks (and ONLY those) were archived.
	gotDeleted := api.deleted[deletedBefore:]
	if len(gotDeleted) != len(oldGoal.Blocks) {
		t.Fatalf("deleted %v, want %v", gotDeleted, oldGoal.Blocks)
	}
	for i, id := range oldGoal.Blocks {
		if gotDeleted[i] != id {
			t.Errorf("deleted[%d] = %q, want %q", i, gotDeleted[i], id)
		}
	}

	bm := ParseBlockMap(reload(t, st, p.Slug).BlockMap)
	if bm.Sections["目標"].Hash == oldGoal.Hash {
		t.Error("hash not updated")
	}
	if bm.Sections["目標"].Blocks[0] != ap.ids[0] {
		t.Error("map does not point at the fresh blocks")
	}
}

func TestSyncNewAndRemovedSections(t *testing.T) {
	st, p := newSyncFixture(t)
	api := &fakeNotion{enabled: true}
	r := NewRenderer(api, "parent-page")
	ctx := context.Background()

	if err := r.SyncProjectPage(ctx, st, p, sampleDoc); err != nil {
		t.Fatal(err)
	}
	fresh := reload(t, st, p.Slug)
	bm := ParseBlockMap(fresh.BlockMap)
	optLast := bm.Sections["選項"].Blocks[len(bm.Sections["選項"].Blocks)-1]
	removed := bm.Sections["更新紀錄"].Blocks

	// Drop 更新紀錄, add 待決定 after 選項.
	edited := strings.Replace(sampleDoc, "## 更新紀錄\n", "## 待決定\n\n- pick a hotel\n", 1)
	if err := r.SyncProjectPage(ctx, st, fresh, edited); err != nil {
		t.Fatal(err)
	}

	ap := api.appends[len(api.appends)-1]
	if ap.after != optLast {
		t.Errorf("new section appended after %q, want preceding section's last %q", ap.after, optLast)
	}
	for _, id := range removed {
		found := false
		for _, d := range api.deleted {
			if d == id {
				found = true
			}
		}
		if !found {
			t.Errorf("removed section block %q not archived", id)
		}
	}
	bm2 := ParseBlockMap(reload(t, st, p.Slug).BlockMap)
	if _, ok := bm2.Sections["更新紀錄"]; ok {
		t.Error("removed section still in map")
	}
	if _, ok := bm2.Sections["待決定"]; !ok {
		t.Error("new section missing from map")
	}
	if want := []string{preambleSection, "目標", "選項", "待決定"}; strings.Join(bm2.Order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", bm2.Order, want)
	}
}

func TestSyncSkipsForeignExportRef(t *testing.T) {
	st, p := newSyncFixture(t)
	// Pre-P3 shape: an export_ref (e.g. a DATABASE id) with no block map.
	kind, ref := "notion", "db-000000"
	if err := st.UpdateProjectFields(p.Slug, store.ProjectFieldUpdate{ExportKind: &kind, ExportRef: &ref}); err != nil {
		t.Fatal(err)
	}
	api := &fakeNotion{enabled: true}
	r := NewRenderer(api, "parent-page")

	if err := r.SyncProjectPage(context.Background(), st, reload(t, st, p.Slug), sampleDoc); err != nil {
		t.Fatal(err)
	}
	if api.mutations() != 0 {
		t.Fatalf("foreign export_ref was touched: %+v", api)
	}
	if NotionPageURL(*reload(t, st, p.Slug)) != "" {
		t.Error("URL constructed for a page we did not render")
	}
}

func TestSyncSkipsUnconfigured(t *testing.T) {
	st, p := newSyncFixture(t)
	ctx := context.Background()

	// No token: no-op, no error.
	r := NewRenderer(&fakeNotion{enabled: false}, "parent-page")
	if err := r.SyncProjectPage(ctx, st, p, sampleDoc); err != nil {
		t.Fatal(err)
	}
	// No parent page: no-op, no error.
	api := &fakeNotion{enabled: true}
	r = NewRenderer(api, "")
	if err := r.SyncProjectPage(ctx, st, p, sampleDoc); err != nil {
		t.Fatal(err)
	}
	if api.mutations() != 0 {
		t.Error("unconfigured renderer touched the API")
	}
	// Non-notion export kind: skip.
	kind := "gdoc"
	if err := st.UpdateProjectFields(p.Slug, store.ProjectFieldUpdate{ExportKind: &kind}); err != nil {
		t.Fatal(err)
	}
	r = NewRenderer(api, "parent-page")
	if err := r.SyncProjectPage(ctx, st, reload(t, st, p.Slug), sampleDoc); err != nil {
		t.Fatal(err)
	}
	if api.mutations() != 0 {
		t.Error("non-notion export kind was rendered")
	}
}

func TestSyncConflictFallsBackToRebuildOnce(t *testing.T) {
	st, p := newSyncFixture(t)
	api := &fakeNotion{enabled: true}
	r := NewRenderer(api, "parent-page")
	ctx := context.Background()

	if err := r.SyncProjectPage(ctx, st, p, sampleDoc); err != nil {
		t.Fatal(err)
	}
	fresh := reload(t, st, p.Slug)
	oldMap := ParseBlockMap(fresh.BlockMap)

	// The surgical append 404s (someone deleted our blocks in the UI) — the
	// renderer must archive everything mapped and re-render in full.
	api.failAppend = &NotionAPIError{Status: 404, Body: "block not found"}
	edited := strings.Replace(sampleDoc, "see the castle", "see the museum", 1)
	if err := r.SyncProjectPage(ctx, st, fresh, edited); err != nil {
		t.Fatal(err)
	}

	for _, entry := range oldMap.Sections {
		for _, id := range entry.Blocks {
			found := false
			for _, d := range api.deleted {
				if d == id {
					found = true
				}
			}
			if !found {
				t.Errorf("rebuild left mapped block %q unarchived", id)
			}
		}
	}
	bm := ParseBlockMap(reload(t, st, p.Slug).BlockMap)
	if !bm.Rendered() || len(bm.Order) != 4 {
		t.Fatalf("rebuilt map = %+v", bm)
	}
	for title, entry := range bm.Sections {
		for _, id := range entry.Blocks {
			for _, old := range oldMap.Sections[title].Blocks {
				if id == old {
					t.Errorf("rebuilt map reuses stale id %q", id)
				}
			}
		}
	}
	// Only one page was ever created — rebuild reuses export_ref.
	if len(api.pages) != 1 {
		t.Errorf("rebuild created a page: %d", len(api.pages))
	}
}

func TestSyncArchivedDeleteIsNotAConflict(t *testing.T) {
	st, p := newSyncFixture(t)
	api := &fakeNotion{enabled: true}
	r := NewRenderer(api, "parent-page")
	ctx := context.Background()

	if err := r.SyncProjectPage(ctx, st, p, sampleDoc); err != nil {
		t.Fatal(err)
	}
	fresh := reload(t, st, p.Slug)
	appendsBefore := len(api.appends)

	// Deleting a block another render already archived returns Notion's
	// "can't edit archived block" 400 — the surgical update must treat it
	// as done and NOT trigger the full-rebuild fallback.
	api.failDelete = &NotionAPIError{Status: 400, Body: `{"code":"validation_error","message":"Can't edit block that is archived."}`}
	edited := strings.Replace(sampleDoc, "see the castle", "see the museum", 1)
	if err := r.SyncProjectPage(ctx, st, fresh, edited); err != nil {
		t.Fatalf("archived delete should not fail the sync: %v", err)
	}
	// One changed section => exactly one new append; a rebuild would re-append
	// every section.
	if got := len(api.appends) - appendsBefore; got != 1 {
		t.Fatalf("expected 1 surgical append, got %d (rebuild suspected)", got)
	}
}
