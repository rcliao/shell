package project

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/rcliao/shell/internal/store"
)

const wikiPost = 900000000000000021

// wikiAgent is one agent's side of a shared doc: its own store, workspace
// and row, all pointing at one post.
type wikiAgent struct {
	name string
	st   *store.Store
	ws   string
	p    *store.Project
	bs   BindingStore
}

// newWiki links two agents to one shared doc; a owns it and runs research.
func newWiki(t *testing.T) (root string, a, b *wikiAgent) {
	t.Helper()
	root = t.TempDir()
	mk := func(name string, research bool) *wikiAgent {
		dir := t.TempDir()
		st, err := store.Open(filepath.Join(dir, "shell.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		ws := filepath.Join(dir, "workspace")
		p, err := st.CreateProject(store.Project{Title: "Trip", ChatID: -100200300, MessageThreadID: wikiPost})
		if err != nil {
			t.Fatal(err)
		}
		docDir, err := EnsureDocRepo(ws, p.Slug)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ScaffoldDoc(docDir, "Trip", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := LinkShared(root, ws, p.Slug, name, wikiPost, research); err != nil {
			t.Fatal(err)
		}
		return &wikiAgent{name: name, st: st, ws: ws, p: p,
			bs: BindingStore{Store: st, WorkspaceDir: ws, Agent: name}}
	}
	return root, mk("a", true), mk("b", false)
}

func (w *wikiAgent) dir(t *testing.T) string {
	t.Helper()
	dir, ok := w.bs.SharedDocDir(w.p.Slug)
	if !ok {
		t.Fatalf("%s: not a shared doc", w.name)
	}
	return dir
}

func (w *wikiAgent) row(t *testing.T) *store.Project {
	t.Helper()
	p, err := w.st.GetProjectBySlug(w.p.Slug)
	if err != nil || p == nil {
		t.Fatalf("%s row: %v", w.name, err)
	}
	return p
}

func setExport(t *testing.T, w *wikiAgent, ref, blockMap string) {
	t.Helper()
	kind := "notion"
	if err := w.st.UpdateProjectFields(w.p.Slug, store.ProjectFieldUpdate{
		ExportKind: &kind, ExportRef: &ref, BlockMap: &blockMap,
	}); err != nil {
		t.Fatal(err)
	}
}

// The owner seeds the doc's binding from its row; the other agent leaves
// Notion alone until then, and afterwards its row points at the owner's page.
func TestBindingSeedsFromOwnerAndRepointsOthers(t *testing.T) {
	_, a, b := newWiki(t)
	setExport(t, a, "page-a", `{"sections":{},"order":[]}`)
	setExport(t, b, "page-b", `{"sections":{},"order":[]}`)

	pb := b.row(t)
	if state, err := b.bs.Overlay(pb); err != nil || state != SharedUnbound {
		t.Fatalf("non-owner before seed: state=%v err=%v", state, err)
	}

	pa := a.row(t)
	if state, err := a.bs.Overlay(pa); err != nil || state != SharedBound || pa.ExportRef != "page-a" {
		t.Fatalf("owner: state=%v ref=%q err=%v", state, pa.ExportRef, err)
	}
	if bd, _ := LoadBinding(a.dir(t)); bd == nil || bd.PageID != "page-a" {
		t.Fatalf("binding = %+v", bd)
	}

	pb = b.row(t)
	if state, err := b.bs.Overlay(pb); err != nil || state != SharedBound || pb.ExportRef != "page-a" {
		t.Fatalf("non-owner after seed: state=%v ref=%q err=%v", state, pb.ExportRef, err)
	}
	if got := b.row(t).ExportRef; got != "page-a" {
		t.Errorf("non-owner row export_ref = %q, want the owner's page", got)
	}
}

// A non-owner's write renders to the ONE page the owner created, and the
// page state goes to the binding, not the writer's row.
func TestBindingNonOwnerWriteRendersOwnersPage(t *testing.T) {
	_, a, b := newWiki(t)
	ctx := context.Background()
	api := &fakeNotion{enabled: true}
	r := NewRenderer(api, "parent-page")

	pa := a.row(t)
	if _, err := a.bs.Overlay(pa); err != nil {
		t.Fatal(err)
	}
	if err := r.SyncProjectPage(ctx, a.bs, pa, sampleDoc); err != nil {
		t.Fatal(err)
	}
	if len(api.pages) != 1 {
		t.Fatalf("pages = %d", len(api.pages))
	}
	page := api.pages[0].id

	pb, err := b.bs.GetProjectBySlug(b.p.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if pb.ExportRef != page {
		t.Fatalf("non-owner sees page %q, want %q", pb.ExportRef, page)
	}
	edited := sampleDoc + "\n## 新段落\n\n- b's addition\n"
	if err := r.SyncProjectPage(ctx, b.bs, pb, edited); err != nil {
		t.Fatal(err)
	}
	if len(api.pages) != 1 {
		t.Fatalf("non-owner render created a page: %d pages", len(api.pages))
	}
	bd, err := LoadBinding(b.dir(t))
	if err != nil || bd == nil {
		t.Fatal(err)
	}
	if _, ok := ParseBlockMap(bd.BlockMap).Sections["新段落"]; !ok {
		t.Errorf("binding block map lacks b's section: %s", bd.BlockMap)
	}
	if got := b.row(t).BlockMap; got != "{}" {
		t.Errorf("non-owner row block_map = %s, want untouched", got)
	}
}

// A comment thread handled by either agent is handled for both.
func TestBindingHandledDiscussionsShared(t *testing.T) {
	_, a, b := newWiki(t)
	setExport(t, a, "page-a", `{"sections":{},"order":[]}`)
	if _, err := a.bs.Overlay(a.row(t)); err != nil {
		t.Fatal(err)
	}
	if err := a.bs.AppendHandledDiscussion(a.p.Slug, "d1"); err != nil {
		t.Fatal(err)
	}
	if err := a.bs.AppendHandledDiscussion(a.p.Slug, "d1"); err != nil {
		t.Fatal(err)
	}
	pb, err := b.bs.GetProjectBySlug(b.p.Slug)
	if err != nil {
		t.Fatal(err)
	}
	set := store.HandledDiscussionSet(pb.HandledDiscussions)
	if !set["d1"] || len(store.ParseHandledDiscussions(pb.HandledDiscussions)) != 1 {
		t.Errorf("handled = %s", pb.HandledDiscussions)
	}
}

// A take sticks through claimOwner, and refuses when it would not.
func TestTakeOwner(t *testing.T) {
	root, a, b := newWiki(t)
	dir := b.dir(t)

	if _, err := TakeOwner(dir, "b", false); !errors.Is(err, ErrTakeNeedsResearch) {
		t.Fatalf("take without research = %v", err)
	}
	prev, err := TakeOwner(dir, "b", true)
	if err != nil || prev != "a" || Owner(dir) != "b" {
		t.Fatalf("take: prev=%q owner=%q err=%v", prev, Owner(dir), err)
	}
	// a's daemon restarts and relinks with its research: b keeps the doc.
	if owner, err := LinkShared(root, a.ws, a.p.Slug, "a", wikiPost, true); err != nil || owner {
		t.Fatalf("a reclaimed: owner=%v err=%v", owner, err)
	}
	if prev, err := TakeOwner(dir, "b", true); err != nil || prev != "b" {
		t.Fatalf("retake: prev=%q err=%v", prev, err)
	}

	solo := t.TempDir()
	soloDir, _ := EnsureDocRepo(solo, "solo")
	if _, err := TakeOwner(soloDir, "b", true); !errors.Is(err, ErrNotShared) {
		t.Fatalf("unshared take = %v", err)
	}
}
