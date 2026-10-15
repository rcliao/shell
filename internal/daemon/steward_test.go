package daemon

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/store"
)

type sent struct {
	chat, thread int64
	text         string
}

// Steward proposals go out right after the beat, at most two: a
// chat-addressed one into its project's thread, an owner-addressed one in a
// DM digest. Each is marked delivered only after its own send succeeded.
// Review suggestions wait for the weekly review.
func TestDeliverSteward(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	review, _ := st.CreateSuggestion(store.Suggestion{Title: "review item", Change: "c"})
	inThread, _ := st.CreateSuggestion(store.Suggestion{Source: store.SuggestionSourceSteward, Project: "garden",
		Audience: store.ChatAudience(-100200300), ThreadID: 77, Title: "種番茄", Evidence: "DO — the owner-only detail", Change: "ASK: 這週末種番茄嗎？"})
	toOwner, _ := st.CreateSuggestion(store.Suggestion{Source: store.SuggestionSourceSteward, Project: "trip", Title: "book", Evidence: "e", Change: "Book the cabin?"})
	third, _ := st.CreateSuggestion(store.Suggestion{Source: store.SuggestionSourceSteward, Project: "house", Title: "later", Change: "c"})

	// The thread send fails, the owner's works: only the owner's is delivered.
	var out []sent
	n, err := deliverSteward(st, "me", 42, func(c, th int64, text string) error {
		if c != 42 {
			return errors.New("down")
		}
		out = append(out, sent{c, th, text})
		return nil
	})
	if err == nil || n != 1 {
		t.Fatalf("partial failure: n=%d err=%v", n, err)
	}
	if g, _ := st.GetSuggestion(inThread); g.Status != store.SuggestionProposed {
		t.Fatalf("failed thread send was marked %s", g.Status)
	}
	if len(out) != 1 || out[0].thread != 0 || !strings.Contains(out[0].text, "trip · book") || !strings.Contains(out[0].text, "Book the cabin?") {
		t.Fatalf("owner digest = %+v", out)
	}

	// Next beat: the thread post goes out, short, with no label or evidence.
	out = nil
	n, err = deliverSteward(st, "me", 42, func(c, th int64, text string) error { out = append(out, sent{c, th, text}); return nil })
	if err != nil || n != 2 {
		t.Fatalf("deliver: n=%d err=%v", n, err)
	}
	if out[0].chat != -100200300 || out[0].thread != 77 || out[0].text != "🧭 種番茄\n這週末種番茄嗎？" {
		t.Fatalf("thread post = %+v", out[0])
	}
	if out[1].chat != 42 || !strings.Contains(out[1].text, "house · later") {
		t.Fatalf("owner digest = %+v", out[1])
	}
	for _, id := range []int64{inThread, toOwner, third} {
		if g, _ := st.GetSuggestion(id); g.Status != store.SuggestionDelivered {
			t.Errorf("#%d = %s, want delivered", id, g.Status)
		}
	}
	if r, _ := st.GetSuggestion(review); r.Status != store.SuggestionProposed {
		t.Errorf("a review suggestion was delivered by the steward: %s", r.Status)
	}

	// The thread sees its own proposal; another thread of the chat does not.
	if open, _ := st.OpenThreadSuggestions(-100200300, 77, time.Hour); len(open) != 1 || open[0].ID != inThread {
		t.Errorf("thread 77 open = %+v", open)
	}
	if open, _ := st.OpenThreadSuggestions(-100200300, 88, time.Hour); len(open) != 0 {
		t.Errorf("thread 88 sees another project's proposal: %+v", open)
	}
	// And the chat's weekly retro neither counts nor withdraws it.
	if got := retroOwn([]store.Suggestion{{ID: 1, Source: store.SuggestionSourceSteward}, {ID: 2, Source: "review"}}); len(got) != 1 || got[0].ID != 2 {
		t.Errorf("retroOwn = %+v", got)
	}
}
