package bridge

import (
	"log/slog"
	"strings"
)

// projectInstructionsMax caps the instructions excerpt in a [Projects] row.
const projectInstructionsMax = 100

// buildProjectsBlock renders the chat-scoped [Projects] registry block (P1,
// docs/PLAN-PROJECT-WORKSPACE.md): one line per ACTIVE project bound to this
// chat, carrying the external doc id (export_ref) so the agent never
// re-derives it — the doc-ID-amnesia killer. Returns "" (zero bytes injected)
// when the chat has no active projects. Per-project attributed blocks via
// topic binding are P4; this block is unconditional for the whole chat.
func (b *Bridge) buildProjectsBlock(chatID int64) string {
	if b.store == nil || chatID == 0 {
		return ""
	}
	projects, err := b.store.ListProjects(chatID)
	if err != nil {
		slog.Warn("projects block: list failed", "chat_id", chatID, "error", err)
		return ""
	}
	var lines []string
	for _, p := range projects {
		if p.Status != "active" {
			continue
		}
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
		lines = append(lines, sb.String())
	}
	if len(lines) == 0 {
		return ""
	}
	return "[Projects]\n" + strings.Join(lines, "\n")
}
