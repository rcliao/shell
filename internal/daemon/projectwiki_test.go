package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/store"
)

// Two daemons, one shared doc (docs/DESIGN-PROJECT-WIKI.md): a owns it and
// runs research, b contributes.
type wikiDaemon struct {
	name string
	st   *store.Store
	ws   string
	slug string
}

func newWikiDaemons(t *testing.T) (a, b wikiDaemon) {
	t.Helper()
	root := t.TempDir()
	const post = 900000000000000031
	mk := func(name string, research bool) wikiDaemon {
		st, ws := newResearchFixture(t)
		p, err := st.CreateProject(store.Project{Title: "Trip", ChatID: -100200300, MessageThreadID: post})
		if err != nil {
			t.Fatal(err)
		}
		dir, err := project.EnsureDocRepo(ws, p.Slug)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := project.ScaffoldDoc(dir, "Trip", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := project.LinkShared(root, ws, p.Slug, name, post, research); err != nil {
			t.Fatal(err)
		}
		return wikiDaemon{name: name, st: st, ws: ws, slug: p.Slug}
	}
	return mk("a", true), mk("b", false)
}

func (w wikiDaemon) render(t *testing.T, api project.NotionAPI) string {
	t.Helper()
	dir, _ := project.ManagedDocDir(w.ws, w.slug)
	rev, err := project.Head(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := projectRenderDeps{store: w.st, workspaceDir: w.ws, agentName: w.name,
		renderer: project.NewRenderer(api, "parent-page")}
	res, err := d.handleProjectRender(context.Background(), renderTask(t, w.st, w.slug, rev))
	if err != nil {
		t.Fatalf("%s render: %v", w.name, err)
	}
	return res
}

func (w wikiDaemon) write(t *testing.T, content string) string {
	t.Helper()
	dir, _ := project.ManagedDocDir(w.ws, w.slug)
	rev, err := project.WriteDoc(dir, content, w.name)
	if err != nil {
		t.Fatal(err)
	}
	return rev
}

func TestWikiOneDocOnePage(t *testing.T) {
	a, b := newWikiDaemons(t)
	api := &renderFakeNotion{}

	if res := b.render(t, api); res != "skipped: waiting for the owner's binding" {
		t.Fatalf("non-owner first render = %q", res)
	}
	if api.pages != 0 {
		t.Fatalf("non-owner created a page")
	}
	a.render(t, api)
	if api.pages != 1 {
		t.Fatalf("owner render: pages = %d", api.pages)
	}

	rev := b.write(t, "# Trip\n\n## 決定\n\n- b booked the hotel\n")
	appendsBefore := api.appends
	b.render(t, api)
	if api.pages != 1 || api.appends == appendsBefore {
		t.Fatalf("non-owner write: pages=%d appends %d→%d", api.pages, appendsBefore, api.appends)
	}
	dir, _ := project.ManagedDocDir(b.ws, b.slug)
	bd, err := project.LoadBinding(dir)
	if err != nil || bd == nil || bd.RenderedRev != rev || bd.PageID != "page-abc" {
		t.Fatalf("binding = %+v (want rendered %s) err=%v", bd, rev, err)
	}
	for _, w := range []wikiDaemon{a, b} {
		p, _ := w.st.GetProjectBySlug(w.slug)
		if p.ExportRef != "page-abc" {
			t.Errorf("%s row export_ref = %q", w.name, p.ExportRef)
		}
	}
}

func TestWikiOnlyOwnerPollsAndCatchesUp(t *testing.T) {
	a, b := newWikiDaemons(t)
	a.render(t, &renderFakeNotion{})

	poll := func(w wikiDaemon) *pollFakeNotion {
		fake := &pollFakeNotion{enabled: true, me: "bot-user",
			lastEdited: time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC),
			comments:   map[string][]project.NotionComment{}}
		d := projectResearchDeps{store: w.st, workspaceDir: w.ws, agentName: w.name,
			notion: fake, notionID: &notionIdentity{}}
		if _, err := d.runNotionPoll(context.Background()); err != nil {
			t.Fatalf("%s poll: %v", w.name, err)
		}
		return fake
	}

	if f := poll(b); f.listCalls != 0 {
		t.Errorf("non-owner polled the shared page: %d comment listings", f.listCalls)
	}

	// b writes but its render never runs: a's poll notices the page is behind.
	head := b.write(t, "# Trip\n\n## 決定\n\n- b changed the date\n")
	if f := poll(a); f.listCalls == 0 {
		t.Errorf("owner did not poll the shared page")
	}
	key := store.DeriveIdempotencyKey(project.RenderKind, a.slug, head)
	task, err := a.st.GetTaskByIdempotencyKey(key)
	if err != nil || task == nil {
		t.Fatalf("owner queued no catch-up render for %s: %v", head, err)
	}
}
