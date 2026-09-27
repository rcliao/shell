package project

import (
	"fmt"
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
