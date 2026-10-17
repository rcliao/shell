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
	for _, want := range []string{"each project's own thread", "the family never saw them, so they do not count as asked", fmt.Sprintf("open proposal #%d still waiting for an answer — skip this project", id)} {
		if !strings.Contains(got, want) {
			t.Errorf("on block lacks %q:\n%s", want, got)
		}
	}
	gid, _, _ := st.UpsertScheduleByKey(&store.Schedule{ChatID: -100200300, Label: "goal", Message: "{}", Schedule: "0 9 * * *", Type: "cron", Mode: "event", NextRunAt: time.Now(), Enabled: true, DedupKey: "goal:garden:x"})
	st.SetScheduleGoal(gid, "find a drip kit", "kit in doc", "garden", 4)
	if got := b.buildStewardBlock(time.Now()); !strings.Contains(got, fmt.Sprintf("goal schedule #%d running (run 0 of 4): find a drip kit", gid)) {
		t.Errorf("block lacks the running goal:\n%s", got)
	}
	if strings.Contains(got, "went only to the owner's DM") {
		t.Errorf("an open thread proposal outranks the shadow note:\n%s", got)
	}
}

func TestStewardOptions(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := store.Project{Slug: "garden", Title: "Garden", ChatID: -100200300, MessageThreadID: 77}
	st.CreateProject(p)
	st.CreateSuggestion(store.Suggestion{Source: store.SuggestionSourceSteward, Project: "garden", Title: "t", Change: "dm only"})
	asked, _ := st.CreateSuggestion(store.Suggestion{Source: store.SuggestionSourceSteward, Project: "garden", Audience: store.ChatAudience(-100200300), ThreadID: 77, Title: "t", Change: "Which bed?\n我建議 north."})
	st.MarkSuggestionsDelivered([]int64{asked})
	now := time.Now().Add(time.Minute)

	plain := StewardOptions{Mode: StewardInThreads}
	if got := StewardEntry(st, p, "", now, plain); strings.Contains(got, "asked ") || !strings.Contains(got, "skip this project") {
		t.Errorf("plain entry shows history or lost the hold:\n%s", got)
	}
	if got := StewardInstructions(plain, 2); strings.Contains(got, `"asked" lines`) || strings.Contains(got, "on your own") || strings.Contains(got, "Before filing") || strings.Contains(got, "characters;") {
		t.Errorf("plain instructions carry an optional part:\n%s", got)
	}
	// Production keeps the hold and renders every part.
	def := DefaultStewardOptions(StewardInThreads)
	if got := StewardEntry(st, p, "", now, def); !strings.Contains(got, "asked today (no answer)") || !strings.Contains(got, "skip this project") {
		t.Errorf("default entry lacks history or the hold:\n%s", got)
	}
	if got := StewardInstructions(def, 2); !strings.Contains(got, `"asked" lines`) || !strings.Contains(got, "Prefer do and goal") || !strings.Contains(got, "Before filing") || !strings.Contains(got, "under 200 characters") {
		t.Errorf("default instructions lack a part:\n%s", got)
	}

	all := StewardOptions{Mode: StewardInThreads, History: true, Capabilities: true, SelfCheck: true, IgnoreOpen: true}
	got := StewardEntry(st, p, "", now, all)
	if !strings.Contains(got, "asked today (no answer): Which bed? 我建議 north.") {
		t.Errorf("history line missing or not one line:\n%s", got)
	}
	if strings.Contains(got, "dm only") {
		t.Errorf("an owner-only proposal is not something the family was asked:\n%s", got)
	}
	if strings.Contains(got, "skip this project") {
		t.Errorf("IgnoreOpen kept the hold:\n%s", got)
	}
	ins := StewardInstructions(all, 1)
	for _, want := range []string{`"asked" lines`, "Never say you will go ahead if nobody answers", "on your own", "Prefer do and goal", "Before filing", "At most 1 proposals"} {
		if !strings.Contains(ins, want) {
			t.Errorf("instructions lack %q", want)
		}
	}
}

func TestStewardMaxChars(t *testing.T) {
	if ins := StewardInstructions(StewardOptions{MaxChars: 200}, 1); !strings.Contains(ins, "under 200 characters") {
		t.Error("MaxChars line missing")
	}
	if ins := StewardInstructions(StewardOptions{}, 1); strings.Contains(ins, "characters;") {
		t.Error("MaxChars line without the option")
	}
}
