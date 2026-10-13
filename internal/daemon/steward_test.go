package daemon

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcliao/shell/internal/store"
)

// Steward proposals go to the owner right after the beat, at most two, and
// are marked delivered only after a successful send. Review suggestions wait
// for the weekly review.
func TestDeliverSteward(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	review, _ := st.CreateSuggestion(store.Suggestion{Title: "review item", Change: "c"})
	for _, title := range []string{"garden: a", "garden: b", "trip: c"} {
		st.CreateSuggestion(store.Suggestion{Source: store.SuggestionSourceSteward, Title: title, Evidence: "seen", Change: "DO: it"})
	}

	if n, err := deliverSteward(st, "me", 0, func(int64, string) error { t.Fatal("no owner, no send"); return nil }); n != 0 || err != nil {
		t.Fatalf("no owner: %d %v", n, err)
	}

	// A failed send leaves everything proposed.
	if _, err := deliverSteward(st, "me", 42, func(int64, string) error { return errors.New("down") }); err == nil {
		t.Fatal("send error not returned")
	}
	if p, _ := st.ProposedFrom(store.SuggestionSourceSteward); len(p) != 3 {
		t.Fatalf("after a failed send, %d proposed, want 3", len(p))
	}

	var sent string
	var to int64
	n, err := deliverSteward(st, "me", 42, func(c int64, text string) error { to, sent = c, text; return nil })
	if err != nil || n != 2 || to != 42 {
		t.Fatalf("deliver: n=%d to=%d err=%v", n, to, err)
	}
	if !strings.Contains(sent, "garden: a") || !strings.Contains(sent, "garden: b") || strings.Contains(sent, "trip: c") || strings.Contains(sent, "review item") {
		t.Fatalf("sent = %q", sent)
	}
	if p, _ := st.ProposedFrom(store.SuggestionSourceSteward); len(p) != 1 {
		t.Errorf("the third stays proposed, got %d", len(p))
	}
	if r, _ := st.GetSuggestion(review); r.Status != store.SuggestionProposed {
		t.Errorf("a review suggestion was delivered by the steward: %s", r.Status)
	}
}
