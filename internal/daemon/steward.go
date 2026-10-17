package daemon

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

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
//
// An auto proposal (docs/DESIGN-STEWARD-AUTO.md) is not posted as a
// question. With autoOn and a project thread, enqueue queues the turn that
// does it and reports in the thread; otherwise (auto "shadow", or the
// steward itself in shadow) the owner's digest says what would have run,
// and the row is withdrawn by "shadow".
func deliverSteward(st *store.Store, agent string, ownerChatID int64, autoOn bool, send func(chatID, threadID int64, text string) error, enqueue func(s store.Suggestion) error) (int, error) {
	staleErr := withdrawStaleAuto(st, time.Now())
	pending, err := st.ProposedFrom(store.SuggestionSourceSteward)
	if err != nil || len(pending) == 0 {
		return 0, errors.Join(staleErr, err)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
	if len(pending) > bridge.StewardMaxProposals {
		pending = pending[:bridge.StewardMaxProposals]
	}
	errs := []error{staleErr}
	n := 0 // auto steps queued (marked delivered as they go)
	var delivered []int64
	var owner, shadow []store.Suggestion
	for _, s := range pending {
		chatID, ok := stewardChat(s.Audience)
		if store.StewardKind(s.Evidence) == "auto" {
			if !autoOn || !ok || enqueue == nil {
				shadow = append(shadow, s)
				continue
			}
			if err := enqueue(s); err != nil {
				errs = append(errs, fmt.Errorf("steward auto #%d: queue its turn: %w", s.ID, err))
				continue
			}
			// Marked now, not after the sends below: the turn checks it.
			if err := st.MarkSuggestionsDelivered([]int64{s.ID}); err != nil {
				errs = append(errs, err)
			}
			n++
			continue
		}
		if !ok {
			owner = append(owner, s)
			continue
		}
		text := "🧭 " + stewardLabel(st, s) + oneLine(s.Title) + "\n" + stewardChange(s.Change)
		if err := send(chatID, s.ThreadID, text); err != nil {
			errs = append(errs, fmt.Errorf("steward #%d to its project thread: %w", s.ID, err))
			continue
		}
		if sess, err := st.GetSession(chatID, s.ThreadID); err == nil && sess != nil {
			st.LogMessage(sess.ID, "assistant", text)
		}
		delivered = append(delivered, s.ID)
	}
	var withdraw []int64
	if ownerChatID == 0 {
		// Nobody to show a shadow step to: it is dropped, not left holding
		// its project and the beat's cap.
		for _, s := range shadow {
			delivered = append(delivered, s.ID)
			withdraw = append(withdraw, s.ID)
		}
	}
	if (len(owner) > 0 || len(shadow) > 0) && ownerChatID != 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "🧭 %s's project steward — only you see these.\n", agent)
		for _, s := range owner {
			fmt.Fprintf(&sb, "\n#%d %s · %s\n%s\n", s.ID, s.Project, oneLine(s.Title), stewardChange(s.Change))
		}
		for _, s := range shadow {
			fmt.Fprintf(&sb, "\n#%d %s · %s — would do without asking (auto, shadow)\n%s\n", s.ID, s.Project, oneLine(s.Title), stewardChange(s.Change))
		}
		if len(owner) > 0 {
			fmt.Fprintf(&sb, "\nReply “accept %d” or “decline %d because …”.", owner[0].ID, owner[0].ID)
		}
		if err := send(ownerChatID, 0, sb.String()); err != nil {
			errs = append(errs, fmt.Errorf("send steward proposals to owner: %w", err))
		} else {
			for _, s := range owner {
				delivered = append(delivered, s.ID)
			}
			for _, s := range shadow {
				delivered = append(delivered, s.ID)
				withdraw = append(withdraw, s.ID)
			}
		}
	}
	if err := st.MarkSuggestionsDelivered(delivered); err != nil {
		errs = append(errs, err)
	}
	for _, id := range withdraw {
		if err := st.DecideSuggestion(id, store.SuggestionWithdrawn, store.DecidedByShadow, "auto in shadow mode: only the owner saw it; not done"); err != nil {
			errs = append(errs, err)
		}
	}
	return n + len(delivered), errors.Join(errs...)
}

// stewardLabel names the proposal's project ("🍅 Garden · ") so a post is
// clear on its own, even in a chat's main thread or a busy topic.
func stewardLabel(st *store.Store, s store.Suggestion) string {
	p, err := st.GetProjectBySlug(s.Project)
	if err != nil || p == nil {
		return ""
	}
	name := strings.TrimSpace(strings.TrimSpace(p.Emoji) + " " + oneLine(p.Title))
	if name == "" {
		return ""
	}
	return name + " · "
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
	for _, label := range []string{"DO:", "ASK:", "GOAL:", "AUTO:"} {
		if rest, ok := strings.CutPrefix(c, label); ok {
			return strings.TrimSpace(rest)
		}
	}
	return c
}
