package project

import (
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/store"
)

func TestOpenerText(t *testing.T) {
	doc := "# Trip\n\n## 目標\n\nSee blossoms.\n\n## 現況\n\nlong research…\n\n## 待決定\n\n1. Stay into April?\n2. Budget\n"
	p := store.Project{Title: "Japan 2027", Emoji: "🎉", Stage: "planning", DocPath: "projects/trip/doc.md"}
	got := OpenerText(p, doc, time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC))
	want := "🎉 **Japan 2027** · planning\n\n**目標**\nSee blossoms.\n\n**待決定**\n1. Stay into April?\n2. Budget\n\n-# 2026-09-27 10:00 · projects/trip/doc.md"
	if got != want {
		t.Fatalf("opener:\n%s\nwant:\n%s", got, want)
	}
	long := "## 決定\n\n" + strings.Repeat("決定事項。", 2000)
	if n := len([]rune(OpenerText(p, long, time.Now()))); n > OpenerMax {
		t.Fatalf("opener %d runes, over %d", n, OpenerMax)
	}
}

// Every write path calls RefreshOpener; it edits only a project that has a
// managed place in its area's chat.
func TestRefreshOpener(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/shell.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.CreateProject(store.Project{Slug: "travel", Title: "Travel", ChatID: -100200300, Kind: store.ProjectKindArea, PlaceRef: "discord:900000000000000099"})
	trip, _ := st.CreateProject(store.Project{Slug: "trip", Title: "Trip", ChatID: -100200300, MessageThreadID: 900000000000000011, Area: "travel"})
	loose, _ := st.CreateProject(store.Project{Slug: "loose", Title: "Loose", ChatID: -100200300, MessageThreadID: 900000000000000012})
	var calls []string
	up := func(chatID, threadID int64, ref, text string) error {
		calls = append(calls, ref+"|"+text[:strings.Index(text, "\n")])
		return nil
	}
	RefreshOpener(st, "", trip, "## 待決定\n\n1. dates\n", up)
	RefreshOpener(st, "", loose, "", up)
	if len(calls) != 1 || calls[0] != "discord:900000000000000099|**Trip**" {
		t.Fatalf("calls = %v", calls)
	}
}
