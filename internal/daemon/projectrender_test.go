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

// renderFakeNotion is a minimal in-memory NotionAPI for the consumer tests.
type renderFakeNotion struct {
	nextID  int
	pages   int
	appends int
}

func (f *renderFakeNotion) Enabled() bool { return true }
func (f *renderFakeNotion) CreatePage(_ context.Context, _, _, _ string) (string, error) {
	f.pages++
	return "page-abc", nil
}
func (f *renderFakeNotion) AppendBlocks(_ context.Context, _ string, blocks []project.NotionBlock, _ string) ([]string, error) {
	f.appends++
	ids := make([]string, 0, len(blocks))
	for range blocks {
		f.nextID++
		ids = append(ids, fmt.Sprintf("blk-%d", f.nextID))
	}
	return ids, nil
}
func (f *renderFakeNotion) UpdateBlock(_ context.Context, _ string, _ project.NotionBlock) error {
	return nil
}
func (f *renderFakeNotion) DeleteBlock(_ context.Context, _ string) error { return nil }
func (f *renderFakeNotion) GetBlockChildren(_ context.Context, _ string) ([]project.NotionBlockRef, error) {
	return nil, nil
}
func (f *renderFakeNotion) GetPageLastEdited(_ context.Context, _ string) (time.Time, error) {
	return time.Time{}, nil
}
func (f *renderFakeNotion) ListComments(_ context.Context, _, _ string) ([]project.NotionComment, string, error) {
	return nil, "", nil
}
func (f *renderFakeNotion) CreateComment(_ context.Context, _ string, _ []project.NotionRichText) (string, error) {
	return "", nil
}
func (f *renderFakeNotion) Me(_ context.Context) (string, error) { return "bot-user", nil }

func renderTask(t *testing.T, st *store.Store, slug, rev string) scheduler.LeasedTask {
	t.Helper()
	created, err := project.EnqueueRender(st, slug, rev)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatalf("render task for (%s, %s) not created", slug, rev)
	}
	tasks, err := st.ListTasks(store.TaskQueued, 5)
	if err != nil || len(tasks) == 0 {
		t.Fatalf("no queued task: %v", err)
	}
	return scheduler.LeasedTask{ID: tasks[0].ID, Kind: tasks[0].Kind, Payload: tasks[0].Payload, Attempt: 1}
}

func TestProjectRenderConsumerRendersManagedDoc(t *testing.T) {
	st, ws := newResearchFixture(t)
	p, err := st.CreateProject(store.Project{Title: "Demo", Emoji: "🏝", ChatID: -100200300})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := project.EnsureDocRepo(ws, p.Slug)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := project.ScaffoldDoc(dir, "Demo", "keep it small")
	if err != nil {
		t.Fatal(err)
	}

	api := &renderFakeNotion{}
	deps := projectRenderDeps{store: st, workspaceDir: ws, renderer: project.NewRenderer(api, "parent-1")}
	result, err := deps.handleProjectRender(context.Background(), renderTask(t, st, p.Slug, rev))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "https://notion.so/pageabc") {
		t.Errorf("result = %q, want the page url", result)
	}
	if api.pages != 1 || api.appends == 0 {
		t.Errorf("api usage: pages=%d appends=%d", api.pages, api.appends)
	}
	fresh, _ := st.GetProjectBySlug(p.Slug)
	if fresh.ExportRef != "page-abc" || fresh.ExportKind != "notion" {
		t.Errorf("export = %s:%s", fresh.ExportKind, fresh.ExportRef)
	}
	if !project.ParseBlockMap(fresh.BlockMap).Rendered() {
		t.Error("block map not persisted")
	}
}

func TestProjectRenderConsumerSkipsGracefully(t *testing.T) {
	st, ws := newResearchFixture(t)
	api := &renderFakeNotion{}
	deps := projectRenderDeps{store: st, workspaceDir: ws, renderer: project.NewRenderer(api, "parent-1")}

	// Unknown project: recorded skip, not a retriable error.
	result, err := deps.handleProjectRender(context.Background(),
		renderTask(t, st, "ghost-project", "rev-1"))
	if err != nil || !strings.Contains(result, "skipped") {
		t.Errorf("missing project: result=%q err=%v", result, err)
	}

	// Registered project with an EXTERNAL doc_path (no managed repo): skip.
	if _, err := st.CreateProject(store.Project{
		Title: "External", ChatID: -100200300, DocPath: "/elsewhere/doc.md",
	}); err != nil {
		t.Fatal(err)
	}
	result, err = deps.handleProjectRender(context.Background(),
		renderTask(t, st, "external", "rev-2"))
	if err != nil || !strings.Contains(result, "skipped") {
		t.Errorf("external doc: result=%q err=%v", result, err)
	}
	if api.pages != 0 || api.appends != 0 {
		t.Errorf("skip paths touched the API: %+v", api)
	}
}

func TestEnqueueRenderIdempotentPerRev(t *testing.T) {
	st, _ := newResearchFixture(t)
	created, err := project.EnqueueRender(st, "demo", "rev-a")
	if err != nil || !created {
		t.Fatalf("first enqueue: created=%t err=%v", created, err)
	}
	created, err = project.EnqueueRender(st, "demo", "rev-a")
	if err != nil || created {
		t.Fatalf("same rev must dedup: created=%t err=%v", created, err)
	}
	created, err = project.EnqueueRender(st, "demo", "rev-b")
	if err != nil || !created {
		t.Fatalf("new rev must enqueue: created=%t err=%v", created, err)
	}
	tasks, err := st.ListTasks(store.TaskQueued, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.Kind != project.RenderKind || task.PartitionKey != "render:demo" {
			t.Errorf("task = %+v", task)
		}
	}
}
