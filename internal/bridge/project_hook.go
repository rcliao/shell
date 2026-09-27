package bridge

import (
	"fmt"
	"github.com/rcliao/shell/internal/decide"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rcliao/shell/internal/store"
)

// projectInstructionsMax caps the instructions excerpt in a [Projects] row.
const projectInstructionsMax = 100

// projectSectionMax caps each doc section quoted into a scoped [Project] block.
const projectSectionMax = 1500

// Doc headings the scoped block quotes. They mirror internal/project's
// DecisionsSection and the to-decide heading; that package imports this one,
// so the names are repeated here rather than imported.
const (
	docDecisionsHeading = "決定"
	docToDecideHeading  = "待決定"
)

// buildProjectsBlock renders the project context for a turn.
//
// In a project's OWN thread (a group forum topic bound to exactly one active
// project) the turn gets a scoped [Project] block: that project alone, with
// its decisions and open questions quoted from the doc. This is deterministic
// — the thread id decides, no classifier — and it is the point of giving a
// project its own topic: the agent stops seeing three trips when someone is
// talking about one (P3.6 unit 2).
//
// Everywhere else it is the chat-wide [Projects] registry (P1): one line per
// ACTIVE project bound to this chat, carrying the external doc id
// (export_ref) so the agent never re-derives it. Returns "" when the chat has
// no active projects.
func (b *Bridge) buildProjectsBlock(chatID, threadID int64) string {
	if b.store == nil || chatID == 0 {
		return ""
	}
	projects, err := b.store.ListProjects(chatID)
	if err != nil {
		slog.Warn("projects block: list failed", "chat_id", chatID, "error", err)
		return ""
	}
	if scoped := b.scopedProjectBlock(projects, threadID); scoped != "" {
		return scoped
	}
	var lines []string
	for _, p := range projects {
		if p.Status != "active" {
			continue
		}
		lines = append(lines, projectRow(p))
	}
	if len(lines) == 0 {
		return ""
	}
	return "[Projects]\n" + strings.Join(lines, "\n")
}

// scopedProjectBlock returns the single-project block when threadID is the
// own thread of exactly one active project, else "". Two projects sharing a
// thread is ambiguous, so it falls back to the list rather than guess.
func (b *Bridge) scopedProjectBlock(projects []store.Project, threadID int64) string {
	if threadID == 0 {
		return ""
	}
	var own *store.Project
	var others []string
	for i := range projects {
		p := &projects[i]
		if p.Status != "active" {
			continue
		}
		if p.MessageThreadID == threadID {
			if own != nil {
				return ""
			}
			own = p
		} else {
			others = append(others, p.Slug)
		}
	}
	if own == nil {
		return ""
	}
	return b.renderScopedProject(*own, others, "[Project] — this thread is this project's own topic; treat the conversation as being about it.",
		"other active projects in this chat, not this thread")
}

// laneProjectBlock is the scoped block for a project lane (R1): the same
// shape a thread-bound project gets, framed as a lane of this chat.
func (b *Bridge) laneProjectBlock(own store.Project, all []store.Project) string {
	var others []string
	for _, p := range all {
		if p.Status == "active" && p.Slug != own.Slug {
			others = append(others, p.Slug)
		}
	}
	return b.renderScopedProject(own, others,
		"[Project] — this message was routed to this project's lane; this conversation (session) is about it.",
		"other active projects in this chat")
}

// advanceProjectLine turns a project conversation into project progress
// (router feedback loop 3): a decision said in chat lands in the doc in the
// same turn instead of waiting for a research pass.
const advanceProjectLine = "If this message settles a decision or raises a new open question for this project, update the doc's " +
	docDecisionsHeading + " / " + docToDecideHeading + " sections in this same turn (read the doc, edit that section, project doc-write)."

func (b *Bridge) renderScopedProject(own store.Project, others []string, header, othersLabel string) string {
	var sb strings.Builder
	sb.WriteString(header + "\n")
	sb.WriteString(advanceProjectLine + "\n")
	sb.WriteString(projectRow(own))
	sb.WriteString(b.areaLines(own))
	if doc := b.readProjectDoc(own.DocPath); doc != "" {
		for _, h := range []string{docDecisionsHeading, docToDecideHeading} {
			if body := docSection(doc, h); body != "" {
				sb.WriteString("\n" + h + ":\n" + truncateRunesTo(body, projectSectionMax))
			}
		}
	}
	if len(others) > 0 {
		sb.WriteString("\n(" + othersLabel + ": " + strings.Join(others, ", ") + ")")
	}
	return sb.String()
}

// readProjectDoc reads a managed doc by its registry path, which is relative
// to the workspace ("projects/<slug>/doc.md") or, in older rows, prefixed
// with "workspace/". Best effort: no workspace or no file means no quotes.
func (b *Bridge) readProjectDoc(docPath string) string {
	if b.workspaceDir == "" || docPath == "" {
		return ""
	}
	rel := filepath.Clean(strings.TrimPrefix(docPath, "workspace/"))
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(b.workspaceDir, rel))
	if err != nil {
		return ""
	}
	return string(data)
}

// docSection returns the trimmed body of every "## " section whose heading
// names heading (docs decorate headings, and can repeat one), joined in
// order. "## " lines inside a fenced code block are content, not headings.
// "" when absent or empty. Mirrors internal/project's scanSections, which
// cannot be imported from here.
func docSection(md, heading string) string {
	var body []string
	in, inFence := false, false
	for _, line := range strings.Split(md, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "## ") {
			in = headingNamed(strings.TrimPrefix(line, "## "), heading)
			continue
		}
		if in {
			body = append(body, line)
		}
	}
	return strings.TrimSpace(strings.Join(body, "\n"))
}

// headingNamed reports whether a "## " heading names title: equal, or title
// decorated on either side by non-letters ("📝 更新紀錄", "更新紀錄 (log)").
// A boundary is required because 待決定 contains 決定 — a substring match
// would file open questions under decisions.
func headingNamed(heading, title string) bool {
	heading = strings.TrimSpace(heading)
	for from := 0; ; {
		i := strings.Index(heading[from:], title)
		if i < 0 {
			return false
		}
		i += from
		before, _ := utf8.DecodeLastRuneInString(heading[:i])
		after, _ := utf8.DecodeRuneInString(heading[i+len(title):])
		if (i == 0 || !unicode.IsLetter(before)) && (i+len(title) == len(heading) || !unicode.IsLetter(after)) {
			return true
		}
		from = i + len(title)
	}
}

// truncateRunesTo bounds s to n runes — docs are mostly CJK.
func truncateRunesTo(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// projectRow renders one registry line for a project.
func projectRow(p store.Project) string {
	var sb strings.Builder
	sb.WriteString("- ")
	if p.Emoji != "" {
		sb.WriteString(p.Emoji)
		sb.WriteString(" ")
	}
	sb.WriteString(p.Slug)
	sb.WriteString(" — ")
	sb.WriteString(p.Title)
	if p.Kind == store.ProjectKindArea {
		sb.WriteString(" | area")
	}
	if p.Area != "" {
		sb.WriteString(" | in area: " + p.Area)
	}
	if p.Stage != "" {
		sb.WriteString(" | stage: " + p.Stage)
	}
	if p.ExportRef != "" {
		sb.WriteString(" | ")
		sb.WriteString(p.ExportKind)
		sb.WriteString(":")
		sb.WriteString(p.ExportRef)
	}
	if p.DocPath != "" {
		sb.WriteString(" | doc: ")
		sb.WriteString(p.DocPath)
	}
	if p.Instructions != "" {
		instr := p.Instructions
		// Rune-safe truncation — instructions may be CJK.
		if r := []rune(instr); len(r) > projectInstructionsMax {
			instr = string(r[:projectInstructionsMax]) + "…"
		}
		sb.WriteString(" | ")
		sb.WriteString(instr)
	}
	return sb.String()
}

// observeRouterShadow hands the turn to the shadow router (P3.7). It builds
// the project list from the same registry the [Projects] block uses, and
// notes which project's own thread this is — the label a which_project
// answer is later scored against. msgID is the Telegram message (0 when the
// turn has none, e.g. a peer relay), so a row can be audited against the
// words it judged. Fire and forget; a nil shadow is a no-op.
func (b *Bridge) observeRouterShadow(chatID, threadID, msgID int64, userMsg string) {
	if b.routerShadow == nil || b.store == nil || chatID == 0 {
		return
	}
	t := decide.Turn{ChatID: chatID, ThreadID: threadID, MsgID: msgID, Message: userMsg, ChatKind: "dm"}
	if chatID < 0 {
		t.ChatKind = "group"
	}
	if projects, err := b.store.ListProjects(chatID); err == nil {
		for _, p := range projects {
			if p.Status != "active" {
				continue
			}
			t.Projects = append(t.Projects, decide.Project{Slug: p.Slug, Title: p.Title, Instructions: p.Instructions, ThreadID: p.MessageThreadID})
			if threadID != 0 && p.MessageThreadID == threadID {
				t.BoundProject = p.Slug
			}
		}
	}
	b.routerShadow.Observe(t)
}

// areaGuideLine tells the agent what an area's own place is for
// (docs/DESIGN-PROJECT-AREAS.md): loose talk and new ideas here, each
// project's work in the project's own place.
const areaGuideLine = "This is an area: its doc holds what is true across all its projects (update it when a lasting constraint or lesson comes up). " +
	"When a message is really about one of its projects, answer it and point people to that project's place. " +
	"When a new effort of this kind keeps coming up, offer to start a project for it (project create --area <this area> --place auto)."

// areaProjectsMax bounds the area index: the newest projects are the live ones.
const areaProjectsMax = 12

// areaLines adds area context to a scoped block: an area gets its guide line
// and the index of its projects; a project in an area gets a pointer to the
// area's doc (shared constraints). "" for plain projects.
func (b *Bridge) areaLines(own store.Project) string {
	if b.store == nil {
		return ""
	}
	var sb strings.Builder
	if own.Kind == store.ProjectKindArea {
		sb.WriteString("\n" + areaGuideLine)
		kids, err := b.store.AreaProjects(own.Slug)
		if err != nil || len(kids) == 0 {
			return sb.String()
		}
		sb.WriteString("\nProjects in this area (newest first; on Discord mention a project's place as <#thread>):")
		for i, k := range kids {
			if i == areaProjectsMax {
				sb.WriteString(fmt.Sprintf("\n- … %d older", len(kids)-i))
				break
			}
			line := "\n- " + strings.TrimSpace(k.Emoji+" "+k.Slug) + " — " + k.Title + " | " + k.Status
			if k.Stage != "" {
				line += " | stage: " + k.Stage
			}
			if k.MessageThreadID > 0 {
				line += fmt.Sprintf(" | thread: %d", k.MessageThreadID)
			}
			sb.WriteString(line)
		}
		return sb.String()
	}
	if own.Area != "" {
		if a, err := b.store.GetProjectBySlug(own.Area); err == nil && a != nil {
			line := "\nPart of area " + a.Slug + " (" + a.Title + ")"
			if a.DocPath != "" {
				line += "; its doc " + a.DocPath + " holds constraints shared by all its projects: read it before planning, and add lasting lessons there"
			}
			sb.WriteString(line + ".")
		}
	}
	return sb.String()
}

// discordThreadFloor: Discord thread and channel ids are snowflakes, far
// above any Telegram topic id (mirrors discord.snowflakeFloor, which this
// package cannot import).
const discordThreadFloor = 100_000_000_000_000_000

// unboundThreadHint tells the agent, in a Discord thread that is none of its
// projects in a chat that has areas, how to take it on: the post is the
// shared record between agents (docs/DESIGN-PROJECT-AREAS.md, part 2), so a
// post another agent opened is joined, not duplicated. "" otherwise.
func (b *Bridge) unboundThreadHint(chatID, threadID int64) string {
	if b.store == nil || threadID < discordThreadFloor {
		return ""
	}
	projects, err := b.store.ListProjects(chatID)
	if err != nil {
		return ""
	}
	hasArea := false
	for _, p := range projects {
		if p.Status != "active" {
			continue
		}
		if p.MessageThreadID == threadID {
			return ""
		}
		if p.Kind == store.ProjectKindArea {
			hasArea = true
		}
	}
	if !hasArea {
		return ""
	}
	return fmt.Sprintf("[Thread] This thread (%d) is none of your projects. If it is a post in one of your areas' forums, "+
		"another agent's project lives here: before doing project work in it, run `project join --thread %d` "+
		"(it registers your own row for this post). Otherwise treat it as ordinary conversation.", threadID, threadID)
}
