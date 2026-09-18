package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

// pollFakeNotion is a stateful NotionAPI for the Wave D consumer tests.
type pollFakeNotion struct {
	enabled    bool
	me         string
	meErr      error
	meCalls    int
	lastEdited time.Time
	comments   map[string][]project.NotionComment // keyed by list target id
	children   []project.NotionBlockRef
	replies    []string // "discussion|text" of every CreateComment
	replyErr   error
}

func (f *pollFakeNotion) Enabled() bool { return f.enabled }
func (f *pollFakeNotion) CreatePage(_ context.Context, _, _, _ string) (string, error) {
	return "", fmt.Errorf("not used")
}
func (f *pollFakeNotion) AppendBlocks(_ context.Context, _ string, _ []project.NotionBlock, _ string) ([]string, error) {
	return nil, fmt.Errorf("not used")
}
func (f *pollFakeNotion) UpdateBlock(_ context.Context, _ string, _ project.NotionBlock) error {
	return nil
}
func (f *pollFakeNotion) DeleteBlock(_ context.Context, _ string) error { return nil }
func (f *pollFakeNotion) GetBlockChildren(_ context.Context, _ string) ([]project.NotionBlockRef, error) {
	return f.children, nil
}
func (f *pollFakeNotion) GetPageLastEdited(_ context.Context, _ string) (time.Time, error) {
	return f.lastEdited, nil
}
func (f *pollFakeNotion) ListComments(_ context.Context, id, _ string) ([]project.NotionComment, string, error) {
	return f.comments[id], "", nil
}
func (f *pollFakeNotion) CreateComment(_ context.Context, discussionID string, rich []project.NotionRichText) (string, error) {
	if f.replyErr != nil {
		return "", f.replyErr
	}
	var text strings.Builder
	for _, r := range rich {
		text.WriteString(r.Text)
	}
	f.replies = append(f.replies, discussionID+"|"+text.String())
	return "reply-1", nil
}
func (f *pollFakeNotion) Me(_ context.Context) (string, error) {
	f.meCalls++
	return f.me, f.meErr
}

// notionFixture builds a store with one rendered notion project plus deps.
func notionFixture(t *testing.T) (*store.Store, string, *pollFakeNotion, projectResearchDeps) {
	t.Helper()
	st, ws := newResearchFixture(t)
	if _, err := st.CreateProject(store.Project{
		Title: "Demo", ChatID: -100200300, MessageThreadID: 7, Lang: "zh",
	}); err != nil {
		t.Fatal(err)
	}
	kind, ref := "notion", "page-1"
	bm := `{"sections":{"選項":{"hash":"h1","blocks":["blk-1","blk-2"]}},"order":["選項"],"footer":"ft-1"}`
	if err := st.UpdateProjectFields("demo", store.ProjectFieldUpdate{
		ExportKind: &kind, ExportRef: &ref, BlockMap: &bm,
	}); err != nil {
		t.Fatal(err)
	}
	fake := &pollFakeNotion{
		enabled:    true,
		me:         "bot-user",
		lastEdited: time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC),
		comments:   map[string][]project.NotionComment{},
	}
	deps := projectResearchDeps{
		store: st, workspaceDir: ws,
		notion: fake, notionID: &notionIdentity{},
	}
	return st, ws, fake, deps
}

func pollTask() scheduler.LeasedTask {
	return scheduler.LeasedTask{ID: 1, Kind: project.EventKind, Payload: `{"event":"notion.poll"}`, Attempt: 1}
}

func queuedEvents(t *testing.T, st *store.Store, event string) []project.EventPayload {
	t.Helper()
	tasks, err := st.ListTasks(store.TaskQueued, 50)
	if err != nil {
		t.Fatal(err)
	}
	var out []project.EventPayload
	for _, task := range tasks {
		if task.Kind != project.EventKind {
			continue
		}
		var p project.EventPayload
		if err := json.Unmarshal([]byte(task.Payload), &p); err != nil {
			t.Fatalf("bad payload %q: %v", task.Payload, err)
		}
		if p.Event == event {
			out = append(out, p)
		}
	}
	return out
}

func TestNotionPollEnqueuesCommentEvents(t *testing.T) {
	st, _, fake, deps := notionFixture(t)
	base := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	// One block-anchored human discussion, one page-level discussion the bot
	// answered last, one already-handled discussion.
	fake.comments["blk-2"] = []project.NotionComment{
		{ID: "c1", DiscussionID: "d1", ParentID: "blk-2", Plain: "add option C", CreatedByID: "user-1", CreatedTime: base},
	}
	fake.comments["page-1"] = []project.NotionComment{
		{ID: "c2", DiscussionID: "d2", ParentID: "page-1", Plain: "thanks", CreatedByID: "user-1", CreatedTime: base},
		{ID: "c3", DiscussionID: "d2", ParentID: "page-1", Plain: "done", CreatedByID: "bot-user", CreatedTime: base.Add(time.Minute)},
		{ID: "c4", DiscussionID: "d3", ParentID: "page-1", Plain: "old", CreatedByID: "user-1", CreatedTime: base},
	}
	if err := st.AppendHandledDiscussion("demo", "d3"); err != nil {
		t.Fatal(err)
	}

	result, err := deps.handleProjectEvent(context.Background(), pollTask())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "1 comment events") {
		t.Errorf("result = %q", result)
	}

	events := queuedEvents(t, st, project.EventNotionCommentCreated)
	if len(events) != 1 {
		t.Fatalf("comment events = %+v, want exactly the human d1", events)
	}
	ev := events[0]
	if ev.Slug != "demo" || ev.DiscussionID != "d1" || ev.CommentPlain != "add option C" || ev.AnchorBlock != "blk-2" {
		t.Errorf("event = %+v", ev)
	}
	if ev.ChatID != -100200300 || ev.ThreadID != 7 {
		t.Errorf("partition binding = (%d, %d)", ev.ChatID, ev.ThreadID)
	}

	p, _ := st.GetProjectBySlug("demo")
	if p.NotionWatermark == "" {
		t.Error("watermark not stored")
	}
	if p.LastHumanActivityAt == nil {
		t.Error("last_human_activity_at not bumped for a new discussion")
	}

	// Replay: the same poll again must not enqueue a second event, and the
	// bot id is cached across polls.
	if _, err := deps.handleProjectEvent(context.Background(), pollTask()); err != nil {
		t.Fatal(err)
	}
	if events := queuedEvents(t, st, project.EventNotionCommentCreated); len(events) != 1 {
		t.Errorf("re-poll duplicated events: %+v", events)
	}
	if fake.meCalls != 1 {
		t.Errorf("me calls = %d, want cached single fetch", fake.meCalls)
	}
}

func TestNotionPollMeFailureEnqueuesNothing(t *testing.T) {
	st, _, fake, deps := notionFixture(t)
	fake.meErr = fmt.Errorf("401")
	fake.comments["page-1"] = []project.NotionComment{
		{ID: "c1", DiscussionID: "d1", ParentID: "page-1", Plain: "hi", CreatedByID: "user-1", CreatedTime: time.Now().UTC()},
	}
	result, err := deps.handleProjectEvent(context.Background(), pollTask())
	if err != nil {
		t.Fatal(err) // per-project failures do not fail the sweep
	}
	if !strings.Contains(result, "1 failures") {
		t.Errorf("result = %q", result)
	}
	if events := queuedEvents(t, st, project.EventNotionCommentCreated); len(events) != 0 {
		t.Errorf("events enqueued without knowing the bot id: %+v", events)
	}
}

func TestNotionPollPageEditedHeuristic(t *testing.T) {
	st, _, fake, deps := notionFixture(t)

	// First poll establishes the watermark — no edit event without a baseline.
	if _, err := deps.handleProjectEvent(context.Background(), pollTask()); err != nil {
		t.Fatal(err)
	}
	if events := queuedEvents(t, st, project.EventNotionPageEdited); len(events) != 0 {
		t.Fatalf("baseline poll enqueued edits: %+v", events)
	}

	// Page moved, nothing of ours explains it → edit event.
	fake.lastEdited = fake.lastEdited.Add(time.Hour)
	if _, err := deps.handleProjectEvent(context.Background(), pollTask()); err != nil {
		t.Fatal(err)
	}
	events := queuedEvents(t, st, project.EventNotionPageEdited)
	if len(events) != 1 || events[0].Slug != "demo" {
		t.Fatalf("edit events = %+v, want one for demo", events)
	}

	// Same last_edited on the next poll → no duplicate (watermark caught up).
	if _, err := deps.handleProjectEvent(context.Background(), pollTask()); err != nil {
		t.Fatal(err)
	}
	if events := queuedEvents(t, st, project.EventNotionPageEdited); len(events) != 1 {
		t.Errorf("edit events duplicated: %+v", events)
	}
}

func TestNotionPollRenderExplainsEdit(t *testing.T) {
	st, _, fake, deps := notionFixture(t)

	// Baseline.
	if _, err := deps.handleProjectEvent(context.Background(), pollTask()); err != nil {
		t.Fatal(err)
	}

	// Our own render for the CURRENT doc rev completed after the watermark:
	// the page movement is ours, not a human's.
	rev := "abc123"
	if err := st.UpdateProjectFields("demo", store.ProjectFieldUpdate{DocRev: &rev}); err != nil {
		t.Fatal(err)
	}
	if _, err := project.EnqueueRender(st, "demo", rev); err != nil {
		t.Fatal(err)
	}
	leased, err := st.LeaseTask("test-worker", time.Minute)
	if err != nil || leased == nil || leased.Kind != project.RenderKind {
		t.Fatalf("lease render task: %+v %v", leased, err)
	}
	if err := st.CompleteTask(leased.ID, "rendered"); err != nil {
		t.Fatal(err)
	}

	fake.lastEdited = fake.lastEdited.Add(time.Hour)
	if _, err := deps.handleProjectEvent(context.Background(), pollTask()); err != nil {
		t.Fatal(err)
	}
	if events := queuedEvents(t, st, project.EventNotionPageEdited); len(events) != 0 {
		t.Errorf("our own render read as a human edit: %+v", events)
	}
}

func TestNotionPollSkipsUnrenderedAndInactive(t *testing.T) {
	st, _, fake, deps := notionFixture(t)
	// A project with an export_ref but NO rendered block map (human page) and
	// a paused one must both stay out of the sweep.
	if _, err := st.CreateProject(store.Project{Title: "Human Page", ChatID: 42}); err != nil {
		t.Fatal(err)
	}
	kind, ref := "notion", "page-2"
	if err := st.UpdateProjectFields("human-page", store.ProjectFieldUpdate{ExportKind: &kind, ExportRef: &ref}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateProjectStatus("demo", "paused"); err != nil {
		t.Fatal(err)
	}
	fake.comments["page-1"] = []project.NotionComment{
		{ID: "c1", DiscussionID: "d1", ParentID: "page-1", Plain: "hi", CreatedByID: "user-1", CreatedTime: time.Now().UTC()},
	}
	result, err := deps.handleProjectEvent(context.Background(), pollTask())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "polled 0 projects") {
		t.Errorf("result = %q", result)
	}
}

func commentTask(t *testing.T, p project.EventPayload) scheduler.LeasedTask {
	t.Helper()
	p.Event = project.EventNotionCommentCreated
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return scheduler.LeasedTask{ID: 2, Kind: project.EventKind, Payload: string(data), Attempt: 1}
}

func TestCommentRevisionRunsTurnAndReplies(t *testing.T) {
	st, _, fake, deps := notionFixture(t)
	var gotPrompt string
	var turnChat, turnThread int64
	homeRefreshed := int64(0)
	deps.runTurn = func(ctx context.Context, chatID, threadID int64, prompt string) (string, error) {
		gotPrompt, turnChat, turnThread = prompt, chatID, threadID
		return "已加入 option C", nil
	}
	deps.refreshHome = func(chatID int64) { homeRefreshed = chatID }

	result, err := deps.handleProjectEvent(context.Background(), commentTask(t, project.EventPayload{
		Slug: "demo", DiscussionID: "d1", CommentPlain: "add option C", AnchorBlock: "blk-2",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "replied=true") {
		t.Errorf("result = %q", result)
	}
	if turnChat != -100200300 || turnThread != 7 {
		t.Errorf("turn ran on (%d, %d), want the project's (chat, thread)", turnChat, turnThread)
	}
	for _, want := range []string{"add option C", "選項", "ONE bounded revision pass", "zh"} {
		if !strings.Contains(gotPrompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if len(fake.replies) != 1 || fake.replies[0] != "d1|已加入 option C" {
		t.Errorf("replies = %v", fake.replies)
	}
	p, _ := st.GetProjectBySlug("demo")
	if !store.HandledDiscussionSet(p.HandledDiscussions)["d1"] {
		t.Error("discussion not marked handled")
	}
	if homeRefreshed != -100200300 {
		t.Errorf("home refreshed for %d", homeRefreshed)
	}

	// Replay after handling: no second turn, no second reply.
	deps.runTurn = func(ctx context.Context, chatID, threadID int64, prompt string) (string, error) {
		t.Fatal("handled discussion must not run a second turn")
		return "", nil
	}
	result, err = deps.handleProjectEvent(context.Background(), commentTask(t, project.EventPayload{
		Slug: "demo", DiscussionID: "d1", CommentPlain: "add option C",
	}))
	if err != nil || !strings.Contains(result, "already handled") {
		t.Errorf("replay: result=%q err=%v", result, err)
	}
}

func TestCommentRevisionFailuresRetryUnmarked(t *testing.T) {
	st, _, fake, deps := notionFixture(t)
	deps.runTurn = func(ctx context.Context, chatID, threadID int64, prompt string) (string, error) {
		return "", fmt.Errorf("session busy")
	}
	if _, err := deps.handleProjectEvent(context.Background(), commentTask(t, project.EventPayload{
		Slug: "demo", DiscussionID: "d1", CommentPlain: "hi",
	})); err == nil {
		t.Fatal("turn failure must return an error for the queue to retry")
	}

	// Reply failure: turn succeeded but the thread reply did not land — the
	// discussion must stay unhandled so the retry can post it.
	deps.runTurn = func(ctx context.Context, chatID, threadID int64, prompt string) (string, error) {
		return "done", nil
	}
	fake.replyErr = fmt.Errorf("503")
	if _, err := deps.handleProjectEvent(context.Background(), commentTask(t, project.EventPayload{
		Slug: "demo", DiscussionID: "d1", CommentPlain: "hi",
	})); err == nil {
		t.Fatal("reply failure must return an error")
	}
	p, _ := st.GetProjectBySlug("demo")
	if store.HandledDiscussionSet(p.HandledDiscussions)["d1"] {
		t.Error("failed thread must not be marked handled")
	}
}

func TestPageEditReconcileWritesHumanEdit(t *testing.T) {
	st, ws, fake, deps := notionFixture(t)
	dir, err := project.EnsureDocRepo(ws, "demo")
	if err != nil {
		t.Fatal(err)
	}
	canonical := "# Demo\n\n## 選項\n\n- option A\n- option B\n"
	if _, err := project.WriteDoc(dir, canonical, "seed"); err != nil {
		t.Fatal(err)
	}

	// Page state: the human edited option B in place.
	fake.children = []project.NotionBlockRef{
		{ID: "h1", Type: "heading_2", Rich: []project.NotionRichText{{Text: "選項"}}},
		{ID: "b1", Type: "bulleted_list_item", Rich: []project.NotionRichText{{Text: "option A"}}},
		{ID: "b2", Type: "bulleted_list_item", Rich: []project.NotionRichText{{Text: "option B (cheaper)"}}},
	}

	editTask := scheduler.LeasedTask{ID: 3, Kind: project.EventKind,
		Payload: `{"event":"notion.page.edited","slug":"demo"}`, Attempt: 1}
	result, err := deps.handleProjectEvent(context.Background(), editTask)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "reconciled: rev ") {
		t.Fatalf("result = %q", result)
	}

	doc, err := project.ReadDoc(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, "option B (cheaper)") {
		t.Errorf("human edit not folded into canonical:\n%s", doc)
	}
	log, err := project.DocLog(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log, "human-edit(notion)") {
		t.Errorf("commit not attributed as human edit:\n%s", log)
	}
	p, _ := st.GetProjectBySlug("demo")
	if p.DocRev == "" || !strings.Contains(result, p.DocRev) {
		t.Errorf("doc_rev not updated: %q vs result %q", p.DocRev, result)
	}
	// The normalizing re-render is queued for the new rev.
	tasks, _ := st.ListTasks(store.TaskQueued, 20)
	found := false
	for _, task := range tasks {
		if task.Kind == project.RenderKind {
			found = true
		}
	}
	if !found {
		t.Error("no render task enqueued after reconcile")
	}

	// No-diff replay: the reconciler no-ops.
	result, err = deps.handleProjectEvent(context.Background(), editTask)
	if err != nil || !strings.Contains(result, "no diff") {
		t.Errorf("no-diff replay: result=%q err=%v", result, err)
	}
}

func TestRegisterNotionPollScheduleIdempotent(t *testing.T) {
	st, _ := newResearchFixture(t)
	registerNotionPollSchedule(st, "UTC")
	registerNotionPollSchedule(st, "UTC")

	sc, err := st.FindScheduleByDedupKey(project.NotionPollDedupKey)
	if err != nil || sc == nil {
		t.Fatalf("poll schedule not registered: %v", err)
	}
	if sc.Mode != scheduler.ModeEvent || sc.Type != "cron" || !sc.Enabled {
		t.Errorf("schedule = %+v", sc)
	}
	kind, payload, err := scheduler.ParseEventMessage(sc.Message)
	if err != nil || kind != project.EventKind {
		t.Fatalf("message envelope: kind=%q err=%v", kind, err)
	}
	p, err := project.DecodeEventPayload(payload)
	if err != nil || p.Event != project.EventNotionPoll {
		t.Errorf("payload = %+v err=%v", p, err)
	}
}
