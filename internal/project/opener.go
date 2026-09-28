package project

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// A project's post opens with its live summary (docs/DESIGN-PROJECT-AREAS.md):
// the first message of the post is re-rendered from the doc on every doc
// write and stage change, so opening the post shows where the project stands
// without scrolling or opening Notion. The doc stays the source of truth.

// OpenerMax keeps the opener inside Discord's 2000-character message limit.
const OpenerMax = 1900

// openerSections are the doc sections the opener quotes, in order, each
// bounded so one long section cannot crowd out the others.
var openerSections = []struct {
	heading string
	max     int
}{
	{"目標", 300},
	{"決定", 700},
	{ToDecideSection, 700},
}

// OpenerText renders the opener for p from its doc.
func OpenerText(p store.Project, doc string, now time.Time) string {
	var b strings.Builder
	if p.Emoji != "" {
		b.WriteString(p.Emoji + " ")
	}
	b.WriteString("**" + p.Title + "**")
	if p.Stage != "" {
		b.WriteString(" · " + p.Stage)
	}
	for _, s := range openerSections {
		var bodies []string
		for _, sec := range sectionsNamed(doc, s.heading) {
			if body := strings.TrimSpace(strings.Join(sec.Body, "\n")); body != "" {
				bodies = append(bodies, body)
			}
		}
		if len(bodies) == 0 {
			continue
		}
		body := strings.Join(bodies, "\n")
		if r := []rune(body); len(r) > s.max {
			body = string(r[:s.max]) + "…"
		}
		fmt.Fprintf(&b, "\n\n**%s**\n%s", s.heading, body)
	}
	footer := "\n\n-# " + now.Format("2006-01-02 15:04")
	if p.DocPath != "" {
		footer += " · " + p.DocPath
	}
	text := b.String()
	if r := []rune(text); len(r)+len([]rune(footer)) > OpenerMax {
		text = string(r[:OpenerMax-len([]rune(footer))-1]) + "…"
	}
	return text + footer
}

// OpenerPlaces is what keeping a post's summary needs from the platform:
// edit the post's first message, or post and edit a summary message of the
// agent's own when it cannot (a person opened the post).
type OpenerPlaces interface {
	UpdateOpener(chatID, threadID int64, placeRef, text string) error
	PostSummary(chatID, threadID int64, placeRef, text string) (msgID int64, err error)
	EditSummary(chatID, threadID, msgID int64, placeRef, text string) error
}

// RefreshOpener re-renders p's post summary from its doc (doc "" reads the
// managed doc). It is called after EVERY doc write — the agent's doc-write
// RPC, a stage change, and a human edit reconciled from Notion — so the post
// stays the live summary. For a shared doc only its owner keeps the summary.
// It edits the post's first message when it can; otherwise it keeps a pinned
// summary message of its own (summary.json in the doc dir). No-op for a
// project without a managed place.
func RefreshOpener(st *store.Store, workspaceDir, agent string, p *store.Project, doc string, pl OpenerPlaces) {
	if st == nil || pl == nil || p == nil || p.Area == "" || p.MessageThreadID == 0 {
		return
	}
	a, err := st.GetProjectBySlug(p.Area)
	if err != nil || a == nil || a.PlaceRef == "" || a.ChatID != p.ChatID {
		return
	}
	dir, managed := ManagedDocDir(workspaceDir, p.Slug)
	if managed && !MayRunAs(dir, agent) {
		return // the other agent owns this post's summary
	}
	if doc == "" && managed {
		doc, _ = ReadDoc(dir)
	}
	text := OpenerText(*p, doc, time.Now())
	err = pl.UpdateOpener(p.ChatID, p.MessageThreadID, a.PlaceRef, text)
	if err == nil || !managed {
		if err != nil {
			slog.Info("project: post opener not updated", "slug", p.Slug, "thread", p.MessageThreadID, "error", err)
		}
		return
	}
	// Not our message to edit: keep a summary message of our own.
	state := ReadSummary(dir)
	if state.MessageID != 0 && state.Agent == agent {
		if err := pl.EditSummary(p.ChatID, p.MessageThreadID, state.MessageID, a.PlaceRef, text); err == nil {
			return
		}
	}
	id, err := pl.PostSummary(p.ChatID, p.MessageThreadID, a.PlaceRef, text)
	if err != nil {
		slog.Info("project: post summary not posted", "slug", p.Slug, "thread", p.MessageThreadID, "error", err)
		return
	}
	if err := WriteSummary(dir, SummaryState{Agent: agent, MessageID: id}); err != nil {
		slog.Warn("project: post summary id not saved", "slug", p.Slug, "error", err)
	}
}
