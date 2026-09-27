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
