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

// SetStewardAuto sets the auto mode from config (scheduler.steward_auto):
// "shadow" offers the auto kind and reports what would run to the owner,
// "on" runs it; anything else leaves it out of the prompt.
func (b *Bridge) SetStewardAuto(mode string) { b.stewardAuto = strings.TrimSpace(mode) }

// StewardOptions picks the optional parts of the steward prompt. The eval
// (docs/DESIGN-STEWARD-EVAL.md) compares them; production renders
// DefaultStewardOptions.
type StewardOptions struct {
	// Mode is the steward mode: "on" addresses each project's thread.
	Mode string
	// History lists each project's earlier steward proposals (any status,
	// last StewardHistoryAge) and asks for nothing already asked.
	History bool
	// Capabilities lists what the agent can do alone and prefers do/goal
	// over ask.
	Capabilities bool
	// SelfCheck asks the agent to check each proposal before filing it.
	SelfCheck bool
	// MaxChars, when set, caps the length of change.
	MaxChars int
	// Auto offers the auto kind: a low-risk do the agent runs right after
	// the beat without asking, and reports (docs/DESIGN-STEWARD-AUTO.md).
	Auto bool
	// IgnoreOpen drops the "open proposal, skip" hold (eval only: it runs a
	// beat as if the hold had expired).
	IgnoreOpen bool
}

// DefaultStewardOptions is what deep beats render: the variant that won the
// 2026-10-16 eval (docs/DESIGN-STEWARD-EVAL.md, Results), plus the auto kind
// when auto (scheduler.steward_auto) is "shadow" or "on".
func DefaultStewardOptions(mode, auto string) StewardOptions {
	return StewardOptions{Mode: mode, History: true, Capabilities: true, SelfCheck: true, MaxChars: StewardMaxChars,
		Auto: auto == StewardShadow || auto == StewardInThreads}
}

// StewardMaxChars caps a proposal's change, the text the family reads.
const StewardMaxChars = 200

// StewardHistoryAge is how far back a project's earlier proposals are listed.
const StewardHistoryAge = 30 * 24 * time.Hour

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
	opts := DefaultStewardOptions(b.stewardMode, b.stewardAuto)
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
		sb.WriteString(StewardEntry(b.store, p, b.readProjectDoc(p.DocPath), now, opts))
	}
	if n == 0 {
		return ""
	}
	return StewardHeader + sb.String() + StewardInstructions(opts, StewardMaxProposals)
}

// StewardHeader opens the steward block; the project entries follow it.
const StewardHeader = "\n\n---\n**[Project steward]**\n" +
	"Before your reflection move: you own these projects, and nobody else is moving them forward between research passes. Your active projects:\n"

// StewardEntry renders one project's lines of the steward listing.
func StewardEntry(st *store.Store, p store.Project, doc string, now time.Time, o StewardOptions) string {
	var sb strings.Builder
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
	if open, err := st.OpenStewardFor(p.Slug, store.StewardOpenAge); !o.IgnoreOpen && err == nil && len(open) > 0 {
		fmt.Fprintf(&sb, "  open proposal #%d still waiting for an answer — skip this project\n", open[0].ID)
	} else if o.Mode == StewardInThreads {
		if old, err := st.OwnerOnlyStewardFor(p.Slug); err == nil && len(old) > 0 {
			fmt.Fprintf(&sb, "  earlier proposal #%d went only to the owner's DM; the family never saw it, so it does not hold this project. If it is still the next step, file it again here (short, for the family)\n", old[len(old)-1].ID)
		}
	}
	if o.History {
		if past, err := st.StewardForProject(p.Slug, now.Add(-StewardHistoryAge), now); err == nil {
			for _, s := range past {
				if s.Audience == "owner" && o.Mode == StewardInThreads {
					continue // the family never saw it
				}
				fmt.Fprintf(&sb, "  asked %s (%s): %s\n", ago(now, s.CreatedAt), stewardOutcome(s), clipRunes(oneLine(s.Change), 140))
			}
		}
	}
	if goals, err := st.OpenGoals(p.Slug); err == nil {
		for _, g := range goals {
			fmt.Fprintf(&sb, "  goal schedule #%d running (run %d of %d): %s\n", g.ScheduleID, g.Runs, g.MaxRuns, clipRunes(g.Goal, 100))
		}
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
	if at, ok, err := st.LastHumanMessageAt(p.ChatID, p.MessageThreadID); err == nil && ok {
		human = ago(now, at)
	}
	research := "never"
	if p.LastResearchAt != nil {
		research = ago(now, *p.LastResearchAt)
	}
	fmt.Fprintf(&sb, "  last human message in its thread: %s; last research pass: %s\n", human, research)
	return sb.String()
}

// stewardOutcome says how an earlier proposal ended, for the history lines.
func stewardOutcome(s store.Suggestion) string {
	if store.StewardKind(s.Evidence) == "auto" {
		switch s.DecidedBy {
		case store.DecidedByAuto:
			return "you did it without asking and reported"
		case store.DecidedByShadow:
			return "auto, shadow: only the owner saw it; not done"
		}
	}
	switch s.Status {
	case store.SuggestionProposed, store.SuggestionDelivered:
		return "no answer"
	case store.SuggestionAccepted, store.SuggestionDone:
		return "they said yes"
	case store.SuggestionDeclined:
		return "they said no"
	case store.SuggestionWithdrawn:
		return "withdrawn"
	}
	return s.Status
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// StewardInstructions renders the steward pass's instructions, after the
// project entries.
func StewardInstructions(o StewardOptions, max int) string {
	audience := "the owner, who rates them"
	if o.Mode == StewardInThreads {
		audience = "each project's own thread, where the people of that project read and answer it"
	}
	autoKind, kindWords := "", "DO, ASK or GOAL"
	if o.Auto {
		autoKind = "  auto — a do so safe you need no yes (rules below): it runs in the project's thread right after this beat and the family gets the result, not a question. change is the task: what you will do and what they get, in the project's language, no question.\n"
		kindWords = "DO, ASK, GOAL or AUTO"
	}
	var out strings.Builder
	fmt.Fprintf(&out, `Pick the one or two projects where a next step would matter most to the family (a goal not progressing, a decision stuck, a deadline, a thread gone quiet). Read a doc with the project skill's doc-read if the lines above are not enough. For each, file ONE proposal with shell_suggestion(action="create", source="steward", project="<slug>", title=…, evidence=…, change=…):
- change: what the family reads. Write it in the project's language (lang above), to the people of that project, in at most 2 short sentences: the next step and why now, ending with a yes/no question they can answer in one word (好 / 不用, or yes / no). Kinds:
  do — "I'll <do X> so you get <Y>. 要我做嗎？"
  ask — the one question, with your recommendation: "<question>? 我建議 <answer>, 因為 <one reason>."
  goal — "I'll <keep doing X> every <cadence> until <done check>. 要開始嗎？"
%s  No labels, ids, phone numbers, emails or links in change.
- title: a few words in the same language, no slug.
- evidence: for the owner's ledger, not the family: start with %s, then what you saw in the doc or thread that makes this the next step. Details and links go here.
`, autoKind, kindWords)
	if o.History {
		out.WriteString(stewardHistoryRule)
	}
	if o.Capabilities {
		out.WriteString(stewardCapabilities)
	}
	if o.SelfCheck {
		out.WriteString(stewardSelfCheck)
	}
	if o.Auto {
		out.WriteString(stewardAutoRule)
	}
	if o.MaxChars > 0 {
		fmt.Fprintf(&out, "Keep change under %d characters; the reason it matters can be one clause.\n", o.MaxChars)
	}
	fmt.Fprintf(&out, "At most %d proposals this beat, one per project. They go to %s after this beat. Do not act on them now, and do not message anyone about them this beat. If no project needs a next step, file nothing and say so in one line of your journal.\n", max, audience)
	if o.Mode == StewardInThreads {
		out.WriteString("Steward proposals addressed to the owner (audience owner in shell_suggestion list) only reached the owner's DM; the family never saw them, so they do not count as asked. If one is still the next step, file it again for its project's thread.\n")
	}
	return out.String()
}

const stewardHistoryRule = `The "asked" lines are what you already put to the family in the last 30 days and how it ended. Do not put the same decision to them again unless something new happened since; then say what is new. A question that went unanswered twice is not stuck for lack of asking: do the work that makes it easy (compare the options, check prices or dates, draft the message) and propose that, or recommend one answer as the default and ask only for a yes to go ahead with it. Never say you will go ahead if nobody answers.
`

const stewardCapabilities = `What you can do on your own once they say yes: research on the web, check prices, dates and availability, use a browser session (up to the payment or submit step), draft Google Docs and Sheets, draft emails and calendar events (sending still needs their yes), and run a goal schedule that keeps working on something every few days until a done check is met. Prefer do and goal: the family's one-word yes should be all you need. Use ask only for what only they can decide (money, taste, a date they commit to, consent), and then recommend an answer. A project with no open decision whose 目標 is not met usually wants a do or a goal.
`

const stewardAutoRule = `Use auto instead of do when the step is all of: free (buys nothing, starts no paid service); private (sends, emails, posts or submits nothing to anyone, and contacts no business or person; your report in the project's thread is the only message); undoable (books, cancels, deletes or edits nothing the family already has; a new draft doc or sheet is fine); one sitting (not a recurring schedule); and nothing the family turned down before. Research, price and date checks, comparisons and drafts usually qualify. If you are unsure, it is a do. Auto is for steps the family would obviously want; at most one auto per project a week runs.
`

const stewardSelfCheck = `Before filing, check each proposal and rewrite or drop it if one fails: it is not something already asked; after a one-word yes you can take the next step yourself; it says what the family gets; every fact in change is in the doc or thread; change is at most 2 sentences and ends in a yes/no question.
`

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
