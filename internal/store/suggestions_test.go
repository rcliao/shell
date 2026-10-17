package store

import (
	"strings"
	"testing"
	"time"
)

func TestSuggestionLifecycle(t *testing.T) {
	s, done := newTestStore(t)
	defer done()

	id, err := s.CreateSuggestion(Suggestion{Title: "Log reactions", Evidence: "3 👎 this week", Change: "record them"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSuggestion(Suggestion{Title: "  "}); err == nil {
		t.Error("an empty title must be refused")
	}
	if err := s.DecideSuggestion(id, SuggestionDone, "owner", ""); err == nil {
		t.Error("done before accepted must be refused")
	}
	if err := s.MarkSuggestionsDelivered([]int64{id}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSuggestion(id)
	if got == nil || got.Status != SuggestionDelivered || got.DeliveredAt == nil || got.Audience != "owner" {
		t.Fatalf("after delivery: %+v", got)
	}
	if err := s.DecideSuggestion(id, SuggestionAccepted, "owner", "good idea"); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideSuggestion(id, SuggestionDeclined, "owner", ""); err == nil || !strings.Contains(err.Error(), "accepted") {
		t.Errorf("a decision is final; got %v", err)
	}
	if err := s.DecideSuggestion(id, SuggestionDone, "owner", "shipped"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSuggestion(id)
	if got.Status != SuggestionDone || got.Note != "shipped" || got.DecidedBy != "owner" || got.DecidedAt == nil {
		t.Fatalf("after done: %+v", got)
	}
	if err := s.DecideSuggestion(999, SuggestionAccepted, "owner", ""); err == nil {
		t.Error("an unknown id must be an error")
	}

	decided, err := s.SuggestionsDecidedSince(time.Now().Add(-time.Hour))
	if err != nil || len(decided) != 1 {
		t.Fatalf("decided since = %v, %v", decided, err)
	}
	open, _ := s.ListSuggestions([]string{SuggestionProposed, SuggestionDelivered}, 0)
	if len(open) != 0 {
		t.Errorf("open = %+v, want none", open)
	}
}

func TestFeedbackCounts(t *testing.T) {
	s, done := newTestStore(t)
	defer done()
	for _, e := range []string{"👍", "👍", "👎"} {
		if err := s.LogFeedback(42, 0, 7, "reaction", e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.FeedbackCounts("reaction", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got["👍"] != 2 || got["👎"] != 1 {
		t.Fatalf("counts = %v", got)
	}
}

// A steward proposal posted in a project thread holds that project until it
// is answered or ages out of the thread's turns; a shadow (owner) row never
// holds it.
func TestOpenStewardFor(t *testing.T) {
	st, done := newTestStore(t)
	defer done()
	shadow, _ := st.CreateSuggestion(Suggestion{Source: SuggestionSourceSteward, Project: "garden", Title: "t", Change: "c"})
	if open, _ := st.OpenStewardFor("garden", StewardOpenAge); len(open) != 0 {
		t.Fatalf("shadow row holds the project: %+v", open)
	}
	id, _ := st.CreateSuggestion(Suggestion{Source: SuggestionSourceSteward, Project: "garden", Audience: ChatAudience(-100200300), ThreadID: 77, Title: "t", Change: "c"})
	if open, _ := st.OpenStewardFor("garden", StewardOpenAge); len(open) != 1 || open[0].ID != id {
		t.Fatalf("proposed thread row: %+v", open)
	}
	st.MarkSuggestionsDelivered([]int64{shadow, id})
	if open, _ := st.OpenStewardFor("garden", StewardOpenAge); len(open) != 1 {
		t.Fatalf("delivered thread row: %+v", open)
	}
	st.db.Exec(`UPDATE suggestions SET delivered_at = ? WHERE id = ?`, time.Now().Add(-15*24*time.Hour).UTC(), id)
	if open, _ := st.OpenStewardFor("garden", StewardOpenAge); len(open) != 0 {
		t.Fatalf("a 15-day-old unanswered row still holds the project: %+v", open)
	}
	if open, _ := st.OpenThreadSuggestions(-100200300, 77, StewardOpenAge); len(open) != 0 {
		t.Fatalf("thread still shows it: %+v", open)
	}
}

func TestStewardForProject(t *testing.T) {
	st, done := newTestStore(t)
	defer done()
	a, _ := st.CreateSuggestion(Suggestion{Source: SuggestionSourceSteward, Project: "garden", Title: "t1", Change: "c"})
	b, _ := st.CreateSuggestion(Suggestion{Source: SuggestionSourceSteward, Project: "garden", Audience: ChatAudience(-100200300), Title: "t2", Change: "c"})
	st.CreateSuggestion(Suggestion{Source: SuggestionSourceSteward, Project: "kitchen", Title: "t3", Change: "c"})
	st.CreateSuggestion(Suggestion{Project: "garden", Title: "review row", Change: "c"})
	st.DecideSuggestion(a, SuggestionDeclined, "owner", "")
	now := time.Now()
	got, err := st.StewardForProject("garden", now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != a || got[1].ID != b {
		t.Fatalf("garden history = %+v, want both steward rows, any status, oldest first", got)
	}
	st.db.Exec(`UPDATE suggestions SET created_at = ? WHERE id = ?`, now.Add(10*time.Minute).UTC(), b)
	if got, _ := st.StewardForProject("garden", now.Add(-time.Hour), now.Add(time.Minute)); len(got) != 1 || got[0].ID != a {
		t.Fatalf("a row created after the cutoff leaked: %+v", got)
	}
}
