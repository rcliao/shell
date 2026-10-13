package daemon

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rcliao/shell/internal/bridge"
	"github.com/rcliao/shell/internal/store"
)

// deliverSteward sends the owner the project steward's new proposals right
// after the deep beat that filed them (docs/DESIGN-AGENT-LOOPS.md, step 1,
// shadow mode), rather than holding them for the weekly review. At most
// bridge.StewardMaxProposals go out; extras stay proposed for the review.
// Delivered means the owner can see it: rows are marked only after a send
// that reported success.
func deliverSteward(st *store.Store, agent string, ownerChatID int64, notify func(int64, string) error) (int, error) {
	if ownerChatID == 0 {
		return 0, nil
	}
	pending, err := st.ProposedFrom(store.SuggestionSourceSteward)
	if err != nil || len(pending) == 0 {
		return 0, err
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
	if len(pending) > bridge.StewardMaxProposals {
		pending = pending[:bridge.StewardMaxProposals]
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "🧭 Project steward (shadow) from %s — next steps it would take. Only you see these.\n", agent)
	var ids []int64
	for _, s := range pending {
		fmt.Fprintf(&sb, "\n#%d %s\n  why: %s\n  next: %s\n", s.ID, s.Title, oneLine(s.Evidence), oneLine(s.Change))
		ids = append(ids, s.ID)
	}
	sb.WriteString("\nReply here, e.g. “accept 12” or “decline 12 because …” — your answers rate the steward.")
	if err := notify(ownerChatID, sb.String()); err != nil {
		return 0, fmt.Errorf("send steward proposals to owner: %w", err)
	}
	if err := st.MarkSuggestionsDelivered(ids); err != nil {
		return 0, err
	}
	return len(ids), nil
}
