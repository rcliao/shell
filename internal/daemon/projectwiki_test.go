package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

// shortLockWait makes a held page lock fail fast in tests.
func shortLockWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := project.NotionLockWait
	project.NotionLockWait = d
	t.Cleanup(func() { project.NotionLockWait = prev })
}

// drainRenders runs a's queued tasks the way the scheduler does: lease, run,
// complete or FailTask (which requeues with no backoff).
func drainRenders(t *testing.T, w wikiDaemon, d projectRenderDeps) {
	t.Helper()
	for {
		task, err := w.st.LeaseTask("worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if task == nil {
			return
		}
		_, herr := d.handleProjectRender(context.Background(),
			scheduler.LeasedTask{ID: task.ID, Kind: task.Kind, Payload: task.Payload, Attempt: task.Attempts})
		if herr == nil {
			_ = w.st.CompleteTask(task.ID, "ok")
			continue
		}
		if _, err := w.st.FailTask(task.ID, herr.Error()); err != nil {
			t.Fatal(err)
		}
	}
}

func (w wikiDaemon) ownerPoll(t *testing.T) {
	t.Helper()
	fake := &pollFakeNotion{enabled: true, me: "bot-user",
		lastEdited: time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC),
		comments:   map[string][]project.NotionComment{}}
	d := projectResearchDeps{store: w.st, workspaceDir: w.ws, agentName: w.name, notion: fake, notionID: &notionIdentity{}}
	if _, err := d.runNotionPoll(context.Background()); err != nil {
		t.Fatal(err)
	}
}

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

// A render that finds the other daemon rendering waits for it.
func TestWikiRenderWaitsOutBusyPage(t *testing.T) {
	shortLockWait(t, 5*time.Second)
	a, _ := newWikiDaemons(t)
	api := &renderFakeNotion{}
	a.render(t, api)
	dir, _ := project.ManagedDocDir(a.ws, a.slug)

	head := a.write(t, "# Trip\n\n## 決定\n\n- a picked the dates\n")
	unlock, err := project.TryLockNotion(dir)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(300 * time.Millisecond); unlock() }()
	a.render(t, api)
	if bd, _ := project.LoadBinding(dir); bd == nil || bd.RenderedRev != head {
		t.Fatalf("binding = %+v, want rendered %s", bd, head)
	}
}

// A render that failed for good (page busy past the wait) is retried by the
// owner's poll under a fresh key, and the page catches up (review finding 1).
func TestWikiFailedRenderCaughtUpByOwnerPoll(t *testing.T) {
	shortLockWait(t, 20*time.Millisecond)
	a, _ := newWikiDaemons(t)
	api := &renderFakeNotion{}
	a.render(t, api)
	for _, q := range mustQueued(t, a.st) {
		_ = a.st.CompleteTask(q.ID, "settle")
	}
	dir, _ := project.ManagedDocDir(a.ws, a.slug)
	d := projectRenderDeps{store: a.st, workspaceDir: a.ws, agentName: a.name,
		renderer: project.NewRenderer(api, "parent-page")}

	head := a.write(t, "# Trip\n\n## 決定\n\n- a picked the dates\n")
	unlock, err := project.TryLockNotion(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := project.EnqueueRender(a.st, a.slug, head); err != nil {
		t.Fatal(err)
	}
	drainRenders(t, a, d)
	unlock()
	if tk, _ := a.st.GetTaskByIdempotencyKey(store.DeriveIdempotencyKey(project.RenderKind, a.slug, head)); tk == nil || tk.State != store.TaskFailed {
		t.Fatalf("setup: render for head should have failed, got %+v", tk)
	}

	a.ownerPoll(t)
	if len(mustQueued(t, a.st)) == 0 {
		t.Fatal("owner poll queued no retry for the failed render")
	}
	drainRenders(t, a, d)
	if bd, _ := project.LoadBinding(dir); bd == nil || bd.RenderedRev != head {
		t.Fatalf("page still behind: binding %+v, head %s", bd, head)
	}
}

func mustQueued(t *testing.T, st *store.Store) []store.Task {
	t.Helper()
	qs, err := st.ListTasks(store.TaskQueued, 50)
	if err != nil {
		t.Fatal(err)
	}
	return qs
}

type disabledNotion struct{ *renderFakeNotion }

func (disabledNotion) Enabled() bool { return false }

// A render that never reached Notion (no token on that daemon) does not move
// rendered_rev, so the owner's reconcile cannot roll the other agent's write
// back to a page that never showed it (review finding 2).
func TestWikiNoopRenderNotRecorded(t *testing.T) {
	a, b := newWikiDaemons(t)
	a.render(t, &renderFakeNotion{})
	dir, _ := project.ManagedDocDir(a.ws, a.slug)
	pageDoc, _ := project.ReadDoc(dir)

	head := b.write(t, "# Trip\n\n## 決定\n\n- b booked the hotel\n")
	b.render(t, disabledNotion{&renderFakeNotion{}})
	bd, _ := project.LoadBinding(dir)
	if bd.RenderedRev == head {
		t.Fatalf("binding says the page shows %s but nothing was rendered", head)
	}

	// The page still shows pageDoc; a human touches it and the owner reconciles.
	_, secs := project.ParseDoc(pageDoc)
	var refs []project.NotionBlockRef
	n := 0
	for _, s := range secs {
		for _, blk := range s.Blocks {
			n++
			refs = append(refs, project.NotionBlockRef{ID: fmt.Sprintf("p%d", n), Type: blk.Type, Rich: blk.Rich})
		}
	}
	refs = append(refs, project.NotionBlockRef{ID: project.ParseBlockMap(bd.BlockMap).Footer, Type: "paragraph"})
	fake := &pollFakeNotion{enabled: true, me: "bot-user", children: refs, comments: map[string][]project.NotionComment{}}
	pd := projectResearchDeps{store: a.st, workspaceDir: a.ws, agentName: a.name, notion: fake, notionID: &notionIdentity{}}
	out, err := pd.runPageEditReconcile(context.Background(), project.EventPayload{Event: project.EventNotionPageEdited, Slug: a.slug})
	if err != nil {
		t.Fatal(err)
	}
	if doc, _ := project.ReadDoc(dir); !strings.Contains(doc, "b booked the hotel") {
		t.Fatalf("reconcile %q rolled back b's write:\n%s", out, doc)
	}
}
