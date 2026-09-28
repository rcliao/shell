package project

import (
	"errors"
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

type fakeOpener struct {
	editErr  error
	edits    []string
	posted   []string
	summEdit []int64
}

func (f *fakeOpener) UpdateOpener(chatID, threadID int64, ref, text string) error {
	if f.editErr != nil {
		return f.editErr
	}
	f.edits = append(f.edits, ref+"|"+text[:strings.Index(text, "\n")])
	return nil
}
func (f *fakeOpener) PostSummary(chatID, threadID int64, ref, text string) (int64, error) {
	f.posted = append(f.posted, text)
	return 700000000000000001, nil
}
func (f *fakeOpener) EditSummary(chatID, threadID, msgID int64, ref, text string) error {
	f.summEdit = append(f.summEdit, msgID)
	return nil
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
	f := &fakeOpener{}
	RefreshOpener(st, "", "a", trip, "## 待決定\n\n1. dates\n", f)
	RefreshOpener(st, "", "a", loose, "", f)
	if len(f.edits) != 1 || f.edits[0] != "discord:900000000000000099|**Trip**" {
		t.Fatalf("edits = %v", f.edits)
	}
}

// A post someone else opened: the owner posts its own pinned summary once,
// then edits it; the other agent leaves it alone.
func TestRefreshOpenerOwnSummary(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/shell.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root, wsA, wsB := t.TempDir(), t.TempDir(), t.TempDir()
	const post = 900000000000000021
	st.CreateProject(store.Project{Slug: "travel", Title: "Travel", ChatID: -100200300, Kind: store.ProjectKindArea, PlaceRef: "discord:900000000000000099"})
	trip, _ := st.CreateProject(store.Project{Slug: "trip", Title: "Trip", ChatID: -100200300, MessageThreadID: post, Area: "travel"})
	for _, ws := range []string{wsA, wsB} {
		dir, _ := EnsureDocRepo(ws, "trip")
		ScaffoldDoc(dir, "Trip", "")
	}
	LinkShared(root, wsA, "trip", "a", post)
	LinkShared(root, wsB, "trip", "b", post)

	f := &fakeOpener{editErr: errors.New("403: not the author")}
	RefreshOpener(st, wsA, "a", trip, "", f)
	RefreshOpener(st, wsA, "a", trip, "", f)
	RefreshOpener(st, wsB, "b", trip, "", f)
	if len(f.posted) != 1 || len(f.summEdit) != 1 || f.summEdit[0] != 700000000000000001 {
		t.Fatalf("posted %d, summary edits %v", len(f.posted), f.summEdit)
	}
}
