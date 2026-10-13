package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/store"
)

func TestStewardBlock(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ws := t.TempDir()
	write := func(slug, doc string) {
		os.MkdirAll(filepath.Join(ws, "projects", slug), 0o755)
		os.WriteFile(filepath.Join(ws, "projects", slug, "doc.md"), []byte(doc), 0o644)
	}
	write("garden", "# Garden\n\n## 目標\n\nTomatoes by July.\n\n## 待決定\n\n1. Which bed?\n2. ~~Seeds~~\n3. Drip or hand watering?\n   - nested note\n")
	write("shared", "# Shared\n\n## 待決定\n\n- Something\n")
	os.WriteFile(filepath.Join(ws, "projects", "shared", "owner"), []byte("other\n"), 0o644)
	st.CreateProject(store.Project{Slug: "garden", Title: "Garden", ChatID: -100200300, MessageThreadID: 77, DocPath: "projects/garden/doc.md"})
	st.CreateProject(store.Project{Slug: "shared", Title: "Shared", ChatID: -100200300, DocPath: "projects/shared/doc.md"})
	b := &Bridge{store: st, workspaceDir: ws, agentName: "me"}

	if got := b.buildStewardBlock(time.Now()); got != "" {
		t.Fatalf("steward off, block = %q", got)
	}
	b.SetProjectSteward("off")
	if b.StewardOn() {
		t.Fatal("off is on")
	}
	b.SetProjectSteward("shadow")
	got := b.buildStewardBlock(time.Now())
	for _, want := range []string{"[Project steward — shadow]", "- garden (project) — Garden", "目標: Tomatoes by July.",
		"待決定 (2 open): Which bed?; Drip or hand watering?", "last human message in its thread: never",
		`source="steward"`, "DO:", "ASK:", "GOAL:", "At most 2 proposals"} {
		if !strings.Contains(got, want) {
			t.Errorf("block lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "- shared") {
		t.Error("a doc another agent owns is that agent's to steward")
	}
}
