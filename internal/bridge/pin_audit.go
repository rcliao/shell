package bridge

import (
	"context"
	"fmt"
	"strings"
)

// pinAuditMinDropped suppresses the block unless enough pins fall below the
// cut to be worth a deep beat's attention. One or two dropped pins is normal
// packing slack, not drift.
const pinAuditMinDropped = 3

// buildPinAuditBlock returns the deep-reflect "pinned memory audit" section,
// or empty when the pin set comfortably fits its retrieval budget.
//
// It reports ground truth and stops: the ranked pin set, where the budget cut
// falls, and what sits below it. It deliberately does NOT classify which
// memories deserve to be above the line — shell has no notion of a "safety"
// memory and inventing one here would put a keyword heuristic in charge of
// which allergy the agent remembers. The agent knows what it holds; it just
// cannot see the cut. Showing the cut is the whole contribution.
func (b *Bridge) buildPinAuditBlock(ctx context.Context, chatID int64) string {
	if b.memory == nil {
		return ""
	}
	entries, pinBudget, err := b.memory.PinAudit(ctx, chatID)
	if err != nil || len(entries) == 0 || pinBudget <= 0 {
		return ""
	}
	rows := make([]pinRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, pinRow{key: e.Key, imp: e.Importance, tok: e.Tokens, locked: e.Locked})
	}
	return renderPinAudit(rows, pinBudget)
}

// pinRow is the rendering view of a pinned memory, kept separate from the
// store type so the block can be tested without a live database.
type pinRow struct {
	key    string
	imp    float64
	tok    int
	locked bool
}

func renderPinAudit(entries []pinRow, pinBudget int) string {
	if len(entries) == 0 || pinBudget <= 0 {
		return ""
	}
	used, cut := 0, len(entries)
	for i, e := range entries {
		if used+e.tok > pinBudget {
			cut = i
			break
		}
		used += e.tok
	}
	dropped := len(entries) - cut
	if dropped < pinAuditMinDropped {
		return ""
	}

	total := 0
	for _, e := range entries {
		total += e.tok
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "\n\n---\n**[Pinned memory audit]**\n")
	fmt.Fprintf(&sb, "You have %d pinned memories (%d tokens). When you query your OWN memory (ghost_context / ghost_search), pinned competes for ~%d tokens and is admitted by importance, highest first — only the top %d are visible to those calls. All %d are still in your system prompt; this is about what you can RETRIEVE.\n",
		len(entries), total, pinBudget, cut, len(entries))
	fmt.Fprintf(&sb, "Below the cut, never retrieved (%d):\n", dropped)
	for i, e := range entries[cut:] {
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
	sb.WriteString("Read that list and ask one question: does anything down there have a real consequence if you forget it — a health fact, an allergy, a standing instruction from the family? Importance was never set as a priority; it drifts, and persona tends to float to the top while facts sink. If something below the cut matters more than something above it, fix the order:\n")
	sb.WriteString("  ghost curate -n <your ns> -k <key> --op boost      # +0.2, pull it above the cut\n")
	sb.WriteString("  ghost curate -n <your ns> -k <key> --op diminish   # -0.2, let it sink\n")
	sb.WriteString("  ghost curate -n <your ns> -k <key> --op unpin      # still searchable, just not always-on\n")
	sb.WriteString("Unpinning is usually the honest fix: a pin set that outgrows its budget is a pin set that stopped choosing. Adjust at most a few per beat, and say what you changed and why.\n")
	return sb.String()
}
