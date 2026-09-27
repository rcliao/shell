package bridge

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcliao/shell/internal/store"
)

// An area's own place gets the area guide and an index of its projects; a
// project in an area points at the area's doc.
func TestAreaBlocks(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const chat, areaThread, tripThread = -100200300, 900000000000000010, 900000000000000011
	if _, err := st.CreateProject(store.Project{Slug: "travel", Title: "Travel", ChatID: chat, MessageThreadID: areaThread,
		Kind: store.ProjectKindArea, PlaceRef: "discord:900000000000000099", DocPath: "projects/travel/doc.md"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateProject(store.Project{Slug: "trip", Title: "Spring trip", Emoji: "✈️", ChatID: chat,
		MessageThreadID: tripThread, Area: "travel", Stage: "planning"}); err != nil {
		t.Fatal(err)
	}
	b := &Bridge{store: st}

	area := b.buildProjectsBlock(chat, areaThread)
	for _, want := range []string{"This is an area", "Projects in this area",
		"- ✈️ trip — Spring trip | active | stage: planning | thread: 900000000000000011", "travel — Travel | area"} {
		if !strings.Contains(area, want) {
			t.Errorf("area block lacks %q:\n%s", want, area)
		}
	}

	trip := b.buildProjectsBlock(chat, tripThread)
	for _, want := range []string{"in area: travel | stage: planning", "Part of area travel (Travel); its doc projects/travel/doc.md"} {
		if !strings.Contains(trip, want) {
			t.Errorf("trip block lacks %q:\n%s", want, trip)
		}
	}
	if strings.Contains(trip, "Projects in this area") {
		t.Error("a project's block must not carry the area index")
	}
}

// A Discord thread that is none of the agent's projects, in a chat with an
// area, gets the join hint; its own post and non-Discord threads do not.
func TestUnboundThreadHint(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const chat, own, other = -100200300, 900000000000000011, 900000000000000012
	b := &Bridge{store: st}
	if b.unboundThreadHint(chat, other) != "" {
		t.Fatal("no areas yet: no hint")
	}
	st.CreateProject(store.Project{Slug: "travel", Title: "Travel", ChatID: chat, Kind: store.ProjectKindArea, PlaceRef: "discord:900000000000000099"})
	st.CreateProject(store.Project{Slug: "trip", Title: "Trip", ChatID: chat, MessageThreadID: own, Area: "travel"})
	if h := b.unboundThreadHint(chat, other); !strings.Contains(h, "project join --thread 900000000000000012") {
		t.Fatalf("hint = %q", h)
	}
	if b.unboundThreadHint(chat, own) != "" || b.unboundThreadHint(chat, 1419) != "" {
		t.Fatal("own post or a Telegram topic got the hint")
	}
}
