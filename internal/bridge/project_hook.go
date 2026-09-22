package bridge

import (
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
	var sb strings.Builder
	sb.WriteString("[Project] — this thread is this project's own topic; treat the conversation as being about it.\n")
	sb.WriteString(projectRow(*own))
	if doc := b.readProjectDoc(own.DocPath); doc != "" {
		for _, h := range []string{docDecisionsHeading, docToDecideHeading} {
			if body := docSection(doc, h); body != "" {
				sb.WriteString("\n" + h + ":\n" + truncateRunesTo(body, projectSectionMax))
			}
		}
	}
	if len(others) > 0 {
		sb.WriteString("\n(other active projects in this chat, not this thread: " + strings.Join(others, ", ") + ")")
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
// answer is later scored against. Fire and forget; a nil shadow is a no-op.
func (b *Bridge) observeRouterShadow(chatID, threadID int64, userMsg string) {
	if b.routerShadow == nil || b.store == nil || chatID == 0 {
		return
	}
	t := decide.Turn{ChatID: chatID, ThreadID: threadID, Message: userMsg, ChatKind: "dm"}
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
