package bridge

import (
	"fmt"
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
	st.CreateProject(store.Project{Slug: "garden", Title: "Garden", ChatID: -100200300, MessageThreadID: 77, DocPath: "projects/garden/doc.md", Lang: "zh"})
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
	for _, want := range []string{"[Project steward]", "- garden (project, lang zh) — Garden", "目標: Tomatoes by July.",
		"待決定 (2 open): Which bed?; Drip or hand watering?", "last human message in its thread: never",
		`source="steward", project="<slug>"`, "start with DO, ASK or GOAL", "at most 2 short sentences",
		"At most 2 proposals this beat, one per project", "the owner, who rates them"} {
		if !strings.Contains(got, want) {
			t.Errorf("block lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "- shared") {
		t.Error("a doc another agent owns is that agent's to steward")
	}

	// "on" names the project threads; a project with an open proposal is
	// marked so the agent skips it.
	b.SetProjectSteward(StewardInThreads)
	if !b.StewardOn() {
		t.Fatal("on is off")
	}
	// An owner-only (shadow) proposal is named, but does not hold the project.
	shadow, _ := st.CreateSuggestion(store.Suggestion{Source: store.SuggestionSourceSteward, Project: "garden", Title: "t", Change: "c"})
	got = b.buildStewardBlock(time.Now())
	if want := fmt.Sprintf("earlier proposal #%d went only to the owner's DM", shadow); !strings.Contains(got, want) || strings.Contains(got, "skip this project") {
		t.Errorf("on block with a shadow row, want %q and no skip:\n%s", want, got)
	}
	id, _ := st.CreateSuggestion(store.Suggestion{Source: store.SuggestionSourceSteward, Project: "garden", Audience: store.ChatAudience(-100200300), ThreadID: 77, Title: "t", Change: "c"})
	got = b.buildStewardBlock(time.Now())
	for _, want := range []string{"each project's own thread", fmt.Sprintf("open proposal #%d still waiting for an answer — skip this project", id)} {
		if !strings.Contains(got, want) {
			t.Errorf("on block lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "went only to the owner's DM") {
		t.Errorf("an open thread proposal outranks the shadow note:\n%s", got)
	}
}
