package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/rcliao/shell/internal/memory"
)

// pinAuditMinDropped suppresses the retrieval half of the block unless enough
// pins fall below that cut to be worth a deep beat's attention. One or two is
// packing slack, not drift. The system-prompt half has NO such floor: one pin
// outside the system prompt is one rule the agent does not have.
const pinAuditMinDropped = 3

// buildPinAuditBlock returns the deep-reflect "pinned memory audit" section,
// or empty when every pin fits both budgets.
//
// Two cuts are reported, in order of consequence:
//
//  1. The system-prompt cut — operating pins that did NOT make it into the
//     composed prompt this generation. Measured 2026-09-02 on pikamini: 8 of
//     15 operating pins were in, 7 were out, and the seven included a health
//     hypothesis, the medication-logging rule and the family's explicit
//     no-unprompted-media order. The daemon had warned about this on every
//     turn for weeks while this block told the agent "all N are still in
//     your system prompt" — which was false, and is why the agent skipped
//     the audit each beat.
//  2. The retrieval cut — what ghost_context/ghost_search can surface from
//     the pinned set under its own importance-ranked sub-budget.
//
// It reports ground truth and stops: it does not decide which pins deserve
// the room. The agent knows what it holds; it just cannot see the cut.
func (b *Bridge) buildPinAuditBlock(ctx context.Context, chatID int64) string {
	if b.memory == nil {
		return ""
	}
	sysKept, sysDropped, sysBudget, err := b.memory.SystemPromptPinCut(ctx, chatID)
	if err != nil {
		sysKept, sysDropped, sysBudget = nil, nil, 0
	}
	entries, pinBudget, err := b.memory.PinAudit(ctx, chatID)
	if err != nil {
		entries, pinBudget = nil, 0
	}
	return renderPinAudit(
		systemCut{kept: toRows(sysKept), dropped: toRows(sysDropped), budget: sysBudget},
		toRows(entries), pinBudget)
}

func toRows(es []memory.PinnedEntry) []pinRow {
	rows := make([]pinRow, 0, len(es))
	for _, e := range es {
		rows = append(rows, pinRow{key: e.Key, imp: e.Importance, tok: e.Tokens, locked: e.Locked})
	}
	return rows
}

// pinRow is the rendering view of a pinned memory, kept separate from the
// store type so the block can be tested without a live database.
type pinRow struct {
	key    string
	imp    float64
	tok    int
	locked bool
}

// systemCut is the composed-prompt packing result for the operating pins.
type systemCut struct {
	kept, dropped []pinRow
	budget        int // tokens
}

func sumTok(rows []pinRow) int {
	t := 0
	for _, r := range rows {
		t += r.tok
	}
	return t
}

func renderPinAudit(sys systemCut, retrieval []pinRow, pinBudget int) string {
	var sb strings.Builder

	// --- 1. System-prompt cut: the one that decides what the agent knows.
	if len(sys.dropped) > 0 && sys.budget > 0 {
		keptTok, dropTok := sumTok(sys.kept), sumTok(sys.dropped)
		fmt.Fprintf(&sb, "\n\n---\n**[Pinned memory audit — system prompt]**\n")
		fmt.Fprintf(&sb, "Your operating pins do NOT all fit the system prompt. Budget %d tokens; %d pins are in (%d tok), %d pins are OUT (%d tok) — the rules below are absent from every conversation turn right now, not just from retrieval:\n",
			sys.budget, len(sys.kept), keptTok, len(sys.dropped), dropTok)
		for _, e := range sys.dropped {
			lock := ""
			if e.locked {
				lock = " [locked — read-only]"
			}
			fmt.Fprintf(&sb, "  - %s (%d tok)%s\n", e.key, e.tok, lock)
		}
		sb.WriteString("How the cut works: pins are packed newest-CREATED first until the budget is full. Patching an old pin does not move it up. Every new pin you create pushes the oldest toward the cut. Importance plays no part here.\n")
		fmt.Fprintf(&sb, "Target: total operating pins ≤ %d tokens so nothing is dropped. This is the first priority of a deep beat whenever it appears. Fixes, in order of preference:\n", sys.budget)
		sb.WriteString("  1. Merge overlapping rules into one (ghost_get each → write one concise rule → ghost_consolidate). Two 900-token rules about verification are one 400-token rule.\n")
		sb.WriteString("  2. Trim a bloated pin: rewrite it shorter with ghost_put (same key) — keep the rule, drop the evidence trail into an unpinned memory.\n")
		sb.WriteString("  3. Unpin what is no longer a standing rule (ghost_curate op=unpin). It stays searchable.\n")
		sb.WriteString("Do not create a new pin to fix this. Say what you merged/trimmed/unpinned and the new total.\n")
	}

	// --- 2. Retrieval cut: what the agent's own memory queries can surface.
	if len(retrieval) > 0 && pinBudget > 0 {
		used, cut := 0, len(retrieval)
		for i, e := range retrieval {
			if used+e.tok > pinBudget {
				cut = i
				break
			}
			used += e.tok
		}
		dropped := len(retrieval) - cut
		if dropped >= pinAuditMinDropped {
			total := sumTok(retrieval)
			fmt.Fprintf(&sb, "\n\n---\n**[Pinned memory audit — retrieval]**\n")
			fmt.Fprintf(&sb, "You have %d pinned memories (%d tokens). When you query your OWN memory (ghost_context / ghost_search), pinned competes for ~%d tokens and is admitted by importance, highest first — only the top %d are visible to those calls. This is separate from the system-prompt cut above.\n",
				len(retrieval), total, pinBudget, cut)
			fmt.Fprintf(&sb, "Below the retrieval cut (%d):\n", dropped)
			for i, e := range retrieval[cut:] {
				if i == 8 {
					fmt.Fprintf(&sb, "  … and %d more\n", dropped-8)
					break
				}
				lock := ""
				if e.locked {
					lock = " [locked — read-only, leave it]"
				}
				fmt.Fprintf(&sb, "  - %s (importance %.2f, %d tok)%s\n", e.key, e.imp, e.tok, lock)
			}
			sb.WriteString("Shrinking the pin set (above) fixes this too. Re-ranking by importance is secondary; adjust at most a few per beat and say why.\n")
		}
	}
	return sb.String()
}
