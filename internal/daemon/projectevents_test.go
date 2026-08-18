package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

func newResearchFixture(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "shell.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, filepath.Join(dir, "workspace")
}

func researchTask(t *testing.T, slug string, chatID, threadID int64) scheduler.LeasedTask {
	t.Helper()
	msg, err := project.ResearchScheduleMessage(slug, chatID, threadID)
	if err != nil {
		t.Fatal(err)
	}
	_, payload, err := scheduler.ParseEventMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	return scheduler.LeasedTask{ID: 1, Kind: project.EventKind, Payload: payload, Attempt: 1}
}

func TestProjectResearchRunsTurnAndDelivers(t *testing.T) {
	st, ws := newResearchFixture(t)
	if _, err := st.CreateProject(store.Project{
		Title: "Demo", ChatID: -100200300, MessageThreadID: 7,
		Instructions: "keep it sorted", Lang: "zh",
	}); err != nil {
		t.Fatal(err)
	}

	var gotPrompt string
	var deliveredChat, deliveredThread int64
	var deliveredText string
	var homeRefreshed int64
	deps := projectResearchDeps{
		store: st, workspaceDir: ws,
		runTurn: func(ctx context.Context, chatID, threadID int64, prompt string) (string, error) {
			gotPrompt = prompt
			if chatID != -100200300 || threadID != 7 {
				t.Errorf("turn ran on (%d, %d), want the project's (chat, thread)", chatID, threadID)
			}
			return "updated two options", nil
		},
		deliver: func(chatID, threadID int64, text string) {
			deliveredChat, deliveredThread, deliveredText = chatID, threadID, text
		},
		refreshHome: func(chatID int64) { homeRefreshed = chatID },
	}

	result, err := deps.handleProjectEvent(context.Background(), researchTask(t, "demo", -100200300, 7))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "delivered=true") {
		t.Errorf("result = %q", result)
	}
	for _, want := range []string{"Demo", "keep it sorted", "zh", "ONE bounded research pass"} {
		if !strings.Contains(gotPrompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if deliveredChat != -100200300 || deliveredThread != 7 || deliveredText != "updated two options" {
		t.Errorf("delta delivered to (%d, %d) %q, want the project's thread", deliveredChat, deliveredThread, deliveredText)
	}
	if homeRefreshed != -100200300 {
		t.Errorf("home refreshed for %d, want the project's chat", homeRefreshed)
	}

	p, _ := st.GetProjectBySlug("demo")
	if p.LastResearchAt == nil {
		t.Error("last_research_at not stamped after the pass")
	}
}

func TestProjectResearchIncludesManagedDoc(t *testing.T) {
	st, ws := newResearchFixture(t)
	if _, err := st.CreateProject(store.Project{Title: "Docful", ChatID: 42}); err != nil {
		t.Fatal(err)
	}
	dir, err := project.EnsureDocRepo(ws, "docful")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := project.ScaffoldDoc(dir, "Docful", "seed instructions"); err != nil {
		t.Fatal(err)
	}

	var gotPrompt string
	deps := projectResearchDeps{
		store: st, workspaceDir: ws,
		runTurn: func(ctx context.Context, chatID, threadID int64, prompt string) (string, error) {
			gotPrompt = prompt
			return "[noop]", nil
		},
	}
	if _, err := deps.handleProjectEvent(context.Background(), researchTask(t, "docful", 42, 0)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotPrompt, "seed instructions") {
		t.Error("managed doc content should ride in the prompt")
	}
}

func TestProjectResearchSkipsMissingAndInactive(t *testing.T) {
	st, ws := newResearchFixture(t)
	if _, err := st.CreateProject(store.Project{Title: "Paused", ChatID: 42, Status: "paused"}); err != nil {
		t.Fatal(err)
	}

	turns := 0
	deps := projectResearchDeps{
		store: st, workspaceDir: ws,
		runTurn: func(ctx context.Context, chatID, threadID int64, prompt string) (string, error) {
			turns++
			return "", nil
		},
	}

	result, err := deps.handleProjectEvent(context.Background(), researchTask(t, "no-such", 42, 0))
	if err != nil || !strings.Contains(result, "not found") {
		t.Errorf("missing project: result=%q err=%v, want skip", result, err)
	}
	result, err = deps.handleProjectEvent(context.Background(), researchTask(t, "paused", 42, 0))
	if err != nil || !strings.Contains(result, "status=paused") {
		t.Errorf("paused project: result=%q err=%v, want skip", result, err)
	}
	if turns != 0 {
		t.Errorf("turns = %d, want none for skipped events", turns)
	}
}

func TestProjectResearchReplayGuard(t *testing.T) {
	st, ws := newResearchFixture(t)
	if _, err := st.CreateProject(store.Project{Title: "Fresh", ChatID: 42}); err != nil {
		t.Fatal(err)
	}
	recent := time.Now().UTC().Add(-10 * time.Minute)
	if err := st.UpdateProjectFields("fresh", store.ProjectFieldUpdate{LastResearchAt: &recent}); err != nil {
		t.Fatal(err)
	}

	deps := projectResearchDeps{
		store: st, workspaceDir: ws,
		runTurn: func(ctx context.Context, chatID, threadID int64, prompt string) (string, error) {
			t.Fatal("a replay within the redo window must not run the turn again")
			return "", nil
		},
	}
	result, err := deps.handleProjectEvent(context.Background(), researchTask(t, "fresh", 42, 0))
	if err != nil || !strings.Contains(result, "already ran") {
		t.Errorf("result=%q err=%v, want already-done skip", result, err)
	}
}

func TestProjectResearchNoopSuppressesDelivery(t *testing.T) {
	st, ws := newResearchFixture(t)
	if _, err := st.CreateProject(store.Project{Title: "Quiet", ChatID: 42}); err != nil {
		t.Fatal(err)
	}

	delivered := false
	deps := projectResearchDeps{
		store: st, workspaceDir: ws,
		runTurn: func(ctx context.Context, chatID, threadID int64, prompt string) (string, error) {
			return "[noop]", nil
		},
		deliver: func(chatID, threadID int64, text string) { delivered = true },
	}
	result, err := deps.handleProjectEvent(context.Background(), researchTask(t, "quiet", 42, 0))
	if err != nil {
		t.Fatal(err)
	}
	if delivered {
		t.Error("[noop] reply must not be delivered")
	}
	if !strings.Contains(result, "delivered=false") {
		t.Errorf("result = %q", result)
	}
	// The pass still counts: a noop research run is a completed research run.
	p, _ := st.GetProjectBySlug("quiet")
	if p.LastResearchAt == nil {
		t.Error("last_research_at should be stamped even for a noop pass")
	}
}

func TestProjectResearchTurnFailureRetries(t *testing.T) {
	st, ws := newResearchFixture(t)
	if _, err := st.CreateProject(store.Project{Title: "Flaky", ChatID: 42}); err != nil {
		t.Fatal(err)
	}

	deps := projectResearchDeps{
		store: st, workspaceDir: ws,
		runTurn: func(ctx context.Context, chatID, threadID int64, prompt string) (string, error) {
			return "", fmt.Errorf("session busy")
		},
	}
	if _, err := deps.handleProjectEvent(context.Background(), researchTask(t, "flaky", 42, 0)); err == nil {
		t.Fatal("a failed turn must return an error so the queue can retry it")
	}
	// No stamp: the retry must be allowed to run the turn.
	p, _ := st.GetProjectBySlug("flaky")
	if p.LastResearchAt != nil {
		t.Error("last_research_at must not be stamped on failure")
	}
}

func TestProjectEventUnknownEventIgnored(t *testing.T) {
	st, ws := newResearchFixture(t)
	deps := projectResearchDeps{store: st, workspaceDir: ws}

	result, err := deps.handleProjectEvent(context.Background(), scheduler.LeasedTask{
		ID: 1, Kind: project.EventKind, Payload: `{"event":"future.unknown","slug":"x"}`,
	})
	if err != nil || !strings.Contains(result, "ignored") {
		t.Errorf("result=%q err=%v, want ignore without retry", result, err)
	}
}
