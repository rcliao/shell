package bridge

import (
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
	if got := b.buildProjectsBlock(42); got != "" {
		t.Errorf("expected zero bytes for a chat with no projects, got %q", got)
	}
	// The phantom system chat never gets a block.
	if got := b.buildProjectsBlock(0); got != "" {
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

	block := b.buildProjectsBlock(42)
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

	block := b.buildProjectsBlock(42)
	if strings.Contains(block, long) {
		t.Error("instructions should be truncated")
	}
	if !strings.Contains(block, strings.Repeat("計", projectInstructionsMax)+"…") {
		t.Errorf("expected %d-rune excerpt with ellipsis, got %q", projectInstructionsMax, block)
	}
}
