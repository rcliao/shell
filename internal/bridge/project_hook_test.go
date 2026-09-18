package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcliao/shell/internal/store"
)

func newProjectHookBridge(t *testing.T) (*Bridge, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Bridge{store: st}, st
}

func TestProjectsBlockEmptyWhenNoProjects(t *testing.T) {
	b, _ := newProjectHookBridge(t)
	if got := b.buildProjectsBlock(42, 0); got != "" {
		t.Errorf("expected zero bytes for a chat with no projects, got %q", got)
	}
	// The phantom system chat never gets a block.
	if got := b.buildProjectsBlock(0, 0); got != "" {
		t.Errorf("expected zero bytes for chat 0, got %q", got)
	}
}

func TestProjectsBlockListsActiveOnly(t *testing.T) {
	b, st := newProjectHookBridge(t)

	if _, err := st.CreateProject(store.Project{
		Title: "Housing Search", ChatID: 42, Emoji: "🏠",
		ExportKind: "notion", ExportRef: "notion-abc123",
		DocPath:      "workspace/projects/housing-search/doc.md",
		Instructions: "keep the options table sorted by price",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateProject(store.Project{Title: "Old Idea", ChatID: 42, Status: "archived"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateProject(store.Project{Title: "Other Chat", ChatID: -100200300}); err != nil {
		t.Fatal(err)
	}

	block := b.buildProjectsBlock(42, 0)
	if !strings.HasPrefix(block, "[Projects]\n") {
		t.Fatalf("block must start with the [Projects] header, got %q", block)
	}
	// export_ref visible — the doc-ID-amnesia fix.
	if !strings.Contains(block, "notion:notion-abc123") {
		t.Errorf("block must carry export_kind:export_ref, got %q", block)
	}
	if !strings.Contains(block, "housing-search — Housing Search") {
		t.Errorf("block must carry slug and title, got %q", block)
	}
	if !strings.Contains(block, "doc: workspace/projects/housing-search/doc.md") {
		t.Errorf("block must carry doc_path, got %q", block)
	}
	if strings.Contains(block, "old-idea") {
		t.Errorf("archived projects must not appear, got %q", block)
	}
	if strings.Contains(block, "other-chat") {
		t.Errorf("other chats' projects must not appear, got %q", block)
	}
}

func TestProjectsBlockTruncatesInstructions(t *testing.T) {
	b, st := newProjectHookBridge(t)

	long := strings.Repeat("計", 150) // CJK: rune-safe truncation matters
	if _, err := st.CreateProject(store.Project{Title: "Long Plan", ChatID: 42, Instructions: long}); err != nil {
		t.Fatal(err)
	}

	block := b.buildProjectsBlock(42, 0)
	if strings.Contains(block, long) {
		t.Error("instructions should be truncated")
	}
	if !strings.Contains(block, strings.Repeat("計", projectInstructionsMax)+"…") {
		t.Errorf("expected %d-rune excerpt with ellipsis, got %q", projectInstructionsMax, block)
	}
}

// In a project's own thread the turn sees THAT project — with its decisions
// and open questions — not the chat-wide list. The thread id decides; nothing
// is classified.
func TestProjectsBlockScopedToOwnThread(t *testing.T) {
	b, st := newProjectHookBridge(t)
	ws := t.TempDir()
	b.workspaceDir = ws

	mk := func(title string, thread int64, docPath string) {
		t.Helper()
		if _, err := st.CreateProject(store.Project{
			Title: title, ChatID: -100200300, MessageThreadID: thread,
			ExportKind: "notion", ExportRef: "ref-" + title, DocPath: docPath,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("Japan Trip", 7, "projects/japan-trip/doc.md")
	mk("Housing", 9, "workspace/projects/housing/doc.md") // legacy prefix
	mk("Loose Idea", 0, "")

	doc := "# Japan Trip\n\n## 目標\n\nsee snow\n\n## 決定\n\n- 2026-09-01 ryokan, not hotel\n\n## 現況\n\nlong status that must NOT be quoted\n\n## 待決定\n\n- which week in February?\n\n## 更新紀錄\n\n- noise\n"
	if err := os.MkdirAll(filepath.Join(ws, "projects", "japan-trip"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "projects", "japan-trip", "doc.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	scoped := b.buildProjectsBlock(-100200300, 7)
	if !strings.HasPrefix(scoped, "[Project] ") {
		t.Fatalf("own thread must get the scoped block, got %q", scoped)
	}
	for _, want := range []string{"japan-trip — Japan Trip", "notion:ref-Japan Trip", "ryokan, not hotel", "which week in February?", "housing", "loose-idea"} {
		if !strings.Contains(scoped, want) {
			t.Errorf("scoped block missing %q:\n%s", want, scoped)
		}
	}
	for _, not := range []string{"must NOT be quoted", "noise", "see snow", "ref-Housing"} {
		if strings.Contains(scoped, not) {
			t.Errorf("scoped block leaked %q:\n%s", not, scoped)
		}
	}

	// A doc the bridge cannot read still scopes — just without quotes.
	if got := b.buildProjectsBlock(-100200300, 9); !strings.HasPrefix(got, "[Project] ") || !strings.Contains(got, "housing — Housing") {
		t.Errorf("thread 9 must scope to housing even with no doc on disk: %q", got)
	}
	// The general thread, and a thread no project owns, keep the full list.
	for _, thread := range []int64{0, 555} {
		list := b.buildProjectsBlock(-100200300, thread)
		if !strings.HasPrefix(list, "[Projects]\n") || strings.Count(list, "\n- ")+1 < 3 {
			t.Errorf("thread %d must get the chat-wide list: %q", thread, list)
		}
	}
	// Two projects claiming one thread is ambiguous: list, never a guess.
	mk("Squatter", 7, "")
	if got := b.buildProjectsBlock(-100200300, 7); !strings.HasPrefix(got, "[Projects]\n") {
		t.Errorf("ambiguous thread must fall back to the list: %q", got)
	}
}

func TestReadProjectDocRefusesEscapes(t *testing.T) {
	b, _ := newProjectHookBridge(t)
	b.workspaceDir = t.TempDir()
	for _, p := range []string{"../../etc/passwd", "/etc/passwd", "workspace/../../x"} {
		if got := b.readProjectDoc(p); got != "" {
			t.Errorf("readProjectDoc(%q) read outside the workspace", p)
		}
	}
}

func TestScopedBlockOnMessyState(t *testing.T) {
	b, st := newProjectHookBridge(t)
	ws := t.TempDir()
	b.workspaceDir = ws

	// Legacy "workspace/" doc_path, read from disk for real. 待決定 must not
	// be quoted as 決定 (substring!), a decorated second 決定 section counts,
	// and a fenced "## " line neither ends the section nor starts one.
	doc := "# H\n\n## 待決定\n\n- OPEN-Q\n\n## 決定\n\n- DECISION-1\n```\n## fenced heading\n- DECISION-IN-FENCE\n```\n- DECISION-2\n\n## 現況\n\nSTATUS\n\n## 📌 決定\n\n- DECISION-3\n"
	if err := os.MkdirAll(filepath.Join(ws, "projects", "housing"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "projects", "housing", "doc.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateProject(store.Project{Title: "Housing", ChatID: -100200300, MessageThreadID: 9,
		DocPath: "workspace/projects/housing/doc.md"}); err != nil {
		t.Fatal(err)
	}
	// A paused project on the same thread must NOT make it ambiguous.
	if _, err := st.CreateProject(store.Project{Title: "Paused Twin", ChatID: -100200300, MessageThreadID: 9, Status: "paused"}); err != nil {
		t.Fatal(err)
	}

	got := b.buildProjectsBlock(-100200300, 9)
	if !strings.HasPrefix(got, "[Project] ") {
		t.Fatalf("a paused twin must not defeat scoping: %q", got)
	}
	decisions := got[strings.Index(got, "\n決定:"):strings.Index(got, "\n待決定:")]
	for _, want := range []string{"DECISION-1", "DECISION-IN-FENCE", "DECISION-2", "DECISION-3"} {
		if !strings.Contains(decisions, want) {
			t.Errorf("decisions quote missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(decisions, "OPEN-Q") || strings.Contains(got, "STATUS") {
		t.Errorf("待決定 leaked into 決定, or status was quoted:\n%s", got)
	}
	if !strings.Contains(got[strings.Index(got, "\n待決定:"):], "OPEN-Q") {
		t.Errorf("open questions missing:\n%s", got)
	}
}

func TestReadProjectDocCannotReachAPlantedSecret(t *testing.T) {
	b, _ := newProjectHookBridge(t)
	root := t.TempDir()
	b.workspaceDir = filepath.Join(root, "workspace")
	if err := os.MkdirAll(b.workspaceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret.md"), []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../secret.md", "workspace/../../secret.md", "projects/../../secret.md", "./../secret.md", filepath.Join(root, "secret.md")} {
		if got := b.readProjectDoc(p); got != "" {
			t.Errorf("readProjectDoc(%q) escaped the workspace and read %q", p, got)
		}
	}
}
