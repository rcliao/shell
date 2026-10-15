package bridge

import (
	"fmt"
	"github.com/rcliao/shell/internal/store"
	"strings"
	"time"
)

// Project steward (docs/DESIGN-AGENT-LOOPS.md, step 1): on a deep heartbeat
// the agent reviews its active projects and proposes what would move each
// forward — something it can do itself, a question for the family, or a
// goal-bound schedule. The proposals are filed as steward suggestions and
// the daemon sends them after the beat: in "shadow" mode only to the owner's
// DM, in "on" mode each into its own project's thread, where the people of
// that project answer it.

// Steward modes; "" and "off" disable it.
const (
	StewardShadow    = "shadow"
	StewardInThreads = "on"
)

// stewardMaxProjects caps the listing so the deep beat's prompt stays small;
// the agent reads a full doc itself (doc-read) for the one or two it picks.
const stewardMaxProjects = 15

// StewardMaxProposals is how many proposals one beat may file and the daemon
// sends; extra ones stay proposed and reach the owner with the weekly review.
const StewardMaxProposals = 2

// SetProjectSteward sets the steward mode from config (scheduler.project_steward).
func (b *Bridge) SetProjectSteward(mode string) {
	b.stewardMode = strings.TrimSpace(mode)
}

// StewardOn reports whether deep beats run the steward pass.
func (b *Bridge) StewardOn() bool {
	return b.stewardMode == StewardShadow || b.stewardMode == StewardInThreads
}

// StewardMode returns the configured steward mode.
func (b *Bridge) StewardMode() string { return b.stewardMode }

// buildStewardBlock renders the steward pass for a deep beat: one compact
// entry per active project whose doc this agent owns, then the instructions.
// Empty when the steward is off or there is nothing to steward.
func (b *Bridge) buildStewardBlock(now time.Time) string {
	if !b.StewardOn() || b.store == nil {
		return ""
	}
	projects, err := b.store.ListProjects(0)
	if err != nil {
		return ""
	}
	var sb strings.Builder
	n := 0
	for _, p := range projects {
		if p.Status != "active" || !b.ownsDoc(p.Slug) {
			continue
		}
		if n == stewardMaxProjects {
			break
		}
		n++
		doc := b.readProjectDoc(p.DocPath)
		kind := p.Kind
		if kind == "" {
			kind = "project"
		}
		meta := kind
		if p.Area != "" {
			meta += ", area " + p.Area
		}
		if p.Stage != "" {
			meta += ", stage " + p.Stage
		}
		if p.Lang != "" {
			meta += ", lang " + p.Lang
		}
		fmt.Fprintf(&sb, "- %s (%s) — %s\n", p.Slug, meta, clipRunes(p.Title, 80))
		if open, err := b.store.OpenStewardFor(p.Slug, store.StewardOpenAge); err == nil && len(open) > 0 {
			fmt.Fprintf(&sb, "  open proposal #%d still waiting for an answer — skip this project\n", open[0].ID)
		}
		if goal := firstLine(docSection(doc, "目標")); goal != "" {
			fmt.Fprintf(&sb, "  目標: %s\n", clipRunes(goal, 160))
		}
		open := openItems(docSection(doc, docToDecideHeading))
		switch {
		case len(open) == 0:
			sb.WriteString("  待決定: none open\n")
		case len(open) == 1:
			fmt.Fprintf(&sb, "  待決定 (1 open): %s\n", clipRunes(open[0], 120))
		default:
			fmt.Fprintf(&sb, "  待決定 (%d open): %s; %s\n", len(open), clipRunes(open[0], 100), clipRunes(open[1], 100))
		}
		human := "never"
		if at, ok, err := b.store.LastHumanMessageAt(p.ChatID, p.MessageThreadID); err == nil && ok {
			human = ago(now, at)
		}
		research := "never"
		if p.LastResearchAt != nil {
			research = ago(now, *p.LastResearchAt)
		}
		fmt.Fprintf(&sb, "  last human message in its thread: %s; last research pass: %s\n", human, research)
	}
	if n == 0 {
		return ""
	}
	audience := "the owner, who rates them"
	if b.stewardMode == StewardInThreads {
		audience = "each project's own thread, where the people of that project read and answer it"
	}
	var out strings.Builder
	out.WriteString("\n\n---\n**[Project steward]**\n")
	out.WriteString("Before your reflection move: you own these projects, and nobody else is moving them forward between research passes. Your active projects:\n")
	out.WriteString(sb.String())
	fmt.Fprintf(&out, `Pick the one or two projects where a next step would matter most to the family (a goal not progressing, a decision stuck, a deadline, a thread gone quiet). Read a doc with the project skill's doc-read if the lines above are not enough. For each, file ONE proposal with shell_suggestion(action="create", source="steward", project="<slug>", title=…, evidence=…, change=…):
- change: what the family reads. Write it in the project's language (lang above), to the people of that project, in at most 2 short sentences: the next step and why now, ending with a yes/no question they can answer in one word (好 / 不用, or yes / no). Kinds:
  do — "I'll <do X> so you get <Y>. 要我做嗎？"
  ask — the one question, with your recommendation: "<question>? 我建議 <answer>, 因為 <one reason>."
  goal — "I'll <keep doing X> every <cadence> until <done check>. 要開始嗎？"
  No labels, ids, phone numbers, emails or links in change.
- title: a few words in the same language, no slug.
- evidence: for the owner's ledger, not the family: start with DO, ASK or GOAL, then what you saw in the doc or thread that makes this the next step. Details and links go here.
At most %d proposals this beat, one per project. They go to %s after this beat. Do not act on them now, and do not message anyone about them this beat. If no project needs a next step, file nothing and say so in one line of your journal.
`, StewardMaxProposals, audience)
	return out.String()
}

// openItems returns the open top-level list items of a 待決定 section:
// numbered or bulleted lines, minus ones struck through or checked off.
func openItems(section string) []string {
	var out []string
	for _, line := range strings.Split(section, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue // blank or nested
		}
		t := strings.TrimSpace(line)
		body := strings.TrimLeft(t, "0123456789.)-*+ ")
		if body == t || body == "" {
			continue // not a list item
		}
		if strings.HasPrefix(body, "~~") || strings.HasPrefix(body, "[x]") || strings.HasPrefix(body, "[X]") {
			continue
		}
		body = strings.TrimPrefix(body, "[ ] ")
		out = append(out, body)
	}
	return out
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// ago renders a coarse age: "today", "1 day ago", "12 days ago".
func ago(now, t time.Time) string {
	d := int(now.Sub(t).Hours() / 24)
	switch {
	case d <= 0:
		return "today"
	case d == 1:
		return "1 day ago"
	default:
		return fmt.Sprintf("%d days ago", d)
	}
}
