package daemon

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/rcliao/shell/internal/bridge"
	"github.com/rcliao/shell/internal/store"
)

// deliverSteward sends the project steward's new proposals right after the
// deep beat that filed them (docs/DESIGN-AGENT-LOOPS.md, step 1), rather than
// holding them for the weekly review. A proposal addressed to a chat ("on"
// mode) is posted in its project's thread, one message each, and logged to
// that thread's session so the turn that reads the answer has it; owner-
// addressed ones ("shadow") go to the owner's DM as one short digest. At most
// bridge.StewardMaxProposals go out; extras stay proposed for the next beat.
// A row is marked delivered only after its send reported success.
func deliverSteward(st *store.Store, agent string, ownerChatID int64, send func(chatID, threadID int64, text string) error) (int, error) {
	pending, err := st.ProposedFrom(store.SuggestionSourceSteward)
	if err != nil || len(pending) == 0 {
		return 0, err
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
	if len(pending) > bridge.StewardMaxProposals {
		pending = pending[:bridge.StewardMaxProposals]
	}
	var errs []error
	var delivered []int64
	var owner []store.Suggestion
	for _, s := range pending {
		chatID, ok := stewardChat(s.Audience)
		if !ok {
			owner = append(owner, s)
			continue
		}
		text := "🧭 " + oneLine(s.Title) + "\n" + stewardChange(s.Change)
		if err := send(chatID, s.ThreadID, text); err != nil {
			errs = append(errs, fmt.Errorf("steward #%d to its project thread: %w", s.ID, err))
			continue
		}
		if sess, err := st.GetSession(chatID, s.ThreadID); err == nil && sess != nil {
			st.LogMessage(sess.ID, "assistant", text)
		}
		delivered = append(delivered, s.ID)
	}
	if len(owner) > 0 && ownerChatID != 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "🧭 %s's project steward — only you see these.\n", agent)
		for _, s := range owner {
			fmt.Fprintf(&sb, "\n#%d %s · %s\n%s\n", s.ID, s.Project, oneLine(s.Title), stewardChange(s.Change))
		}
		fmt.Fprintf(&sb, "\nReply “accept %d” or “decline %d because …”.", owner[0].ID, owner[0].ID)
		if err := send(ownerChatID, 0, sb.String()); err != nil {
			errs = append(errs, fmt.Errorf("send steward proposals to owner: %w", err))
		} else {
			for _, s := range owner {
				delivered = append(delivered, s.ID)
			}
		}
	}
	if err := st.MarkSuggestionsDelivered(delivered); err != nil {
		errs = append(errs, err)
	}
	return len(delivered), errors.Join(errs...)
}

// stewardChat returns the chat a chat-addressed suggestion goes to.
func stewardChat(audience string) (int64, bool) {
	rest, ok := strings.CutPrefix(audience, "chat:")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	return id, err == nil && id != 0
}

// stewardChange is the proposal text as the family reads it: trimmed, and
// without a DO:/ASK:/GOAL: label left over from the older format.
func stewardChange(change string) string {
	c := strings.TrimSpace(change)
	for _, label := range []string{"DO:", "ASK:", "GOAL:"} {
		if rest, ok := strings.CutPrefix(c, label); ok {
			return strings.TrimSpace(rest)
		}
	}
	return c
}
