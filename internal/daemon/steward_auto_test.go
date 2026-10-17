package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/bridge"
	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/store"
)

// The auto turn runs in the project's thread with the rules repeated, posts
// its report there and closes the proposal done by auto; [noop] withdraws it
// unsent; a failed turn errors (the queue retries) and leaves it delivered.
func TestStewardAutoTurn(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.CreateProject(store.Project{Slug: "garden", Title: "Garden", Lang: "zh-TW", ChatID: -100200300, MessageThreadID: 77, Status: "active"})
	file := func() int64 {
		id, _ := st.CreateSuggestion(store.Suggestion{Source: store.SuggestionSourceSteward, Project: "garden",
			Audience: store.ChatAudience(-100200300), ThreadID: 77, Title: "比價", Evidence: "AUTO. three shops", Change: "我查三家番茄苗的價格。"})
		st.MarkSuggestionsDelivered([]int64{id})
		return id
	}
	var prompt, reply string
	var turnErr error
	var duringTurn func()
	var out []sent
	deps := projectResearchDeps{
		store:         st,
		stewardAutoOn: true,
		runTurn: func(_ context.Context, chatID, threadID int64, p string) (string, error) {
			if chatID != -100200300 || threadID != 77 {
				t.Errorf("turn ran on %d/%d", chatID, threadID)
			}
			prompt = p
			if duringTurn != nil {
				duringTurn()
			}
			return reply, turnErr
		},
		deliver: func(c, th int64, text string, _ []bridge.LinkButton) { out = append(out, sent{c, th, text}) },
	}
	run := func(id int64) (string, error) {
		return deps.runStewardAuto(context.Background(), project.EventPayload{Event: project.EventStewardAuto, Slug: "garden", SuggestionID: id})
	}

	id := file()
	reply = "查好了：A 店 $3、B 店 $4。要我把最便宜的加進購物清單嗎？"
	if _, err := run(id); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[Steward auto #", "我查三家番茄苗的價格。", "buy nothing", "reply [noop]", "zh-TW"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if len(out) != 1 || out[0].thread != 77 || out[0].text != reply {
		t.Fatalf("delivered = %+v", out)
	}
	if g, _ := st.GetSuggestion(id); g.Status != store.SuggestionDone || g.DecidedBy != store.DecidedByAuto {
		t.Errorf("after report = %s by %q, want done by auto", g.Status, g.DecidedBy)
	}
	// A replay of a finished one does nothing.
	out = nil
	if res, _ := run(id); !strings.HasPrefix(res, "skipped") || len(out) != 0 {
		t.Errorf("replay = %q, sent %d", res, len(out))
	}

	id = file()
	reply = "[noop]"
	if _, err := run(id); err != nil || len(out) != 0 {
		t.Fatalf("noop: err=%v sent=%d", err, len(out))
	}
	if g, _ := st.GetSuggestion(id); g.Status != store.SuggestionWithdrawn || g.DecidedBy != store.DecidedByAuto {
		t.Errorf("after noop = %s by %q", g.Status, g.DecidedBy)
	}

	id = file()
	reply, turnErr = "", errors.New("timeout")
	if _, err := run(id); err == nil {
		t.Fatal("failed turn returned no error")
	}
	if g, _ := st.GetSuggestion(id); g.Status != store.SuggestionDelivered {
		t.Errorf("after failed turn = %s, want delivered", g.Status)
	}
	turnErr = nil

	// Leased before delivery marked it: still runs.
	early, _ := st.CreateSuggestion(store.Suggestion{Source: store.SuggestionSourceSteward, Project: "garden",
		Audience: store.ChatAudience(-100200300), ThreadID: 77, Title: "t", Evidence: "AUTO. e", Change: "c"})
	reply, out = "查好了。要我再做下一步嗎？", nil
	if _, err := run(early); err != nil || len(out) != 1 {
		t.Fatalf("proposed row: err=%v sent=%d", err, len(out))
	}
	if g, _ := st.GetSuggestion(early); g.Status != store.SuggestionDone {
		t.Errorf("proposed row after run = %s, want done", g.Status)
	}

	// The turn records accepted on its own row: still closed done by auto.
	id = file()
	duringTurn = func() { st.DecideSuggestion(id, store.SuggestionAccepted, "agent", "") }
	if _, err := run(id); err != nil {
		t.Fatal(err)
	}
	if g, _ := st.GetSuggestion(id); g.Status != store.SuggestionDone || g.DecidedBy != store.DecidedByAuto {
		t.Errorf("pre-accepted row = %s by %q, want done by auto", g.Status, g.DecidedBy)
	}
}

// A step queued while auto was on does not run after auto is turned off.
func TestStewardAutoTurnedOff(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.CreateProject(store.Project{Slug: "garden", Title: "Garden", ChatID: -100200300, MessageThreadID: 77, Status: "active"})
	id, _ := st.CreateSuggestion(store.Suggestion{Source: store.SuggestionSourceSteward, Project: "garden",
		Audience: store.ChatAudience(-100200300), ThreadID: 77, Title: "t", Evidence: "AUTO. e", Change: "c"})
	st.MarkSuggestionsDelivered([]int64{id})
	turns := 0
	deps := projectResearchDeps{store: st, runTurn: func(context.Context, int64, int64, string) (string, error) { turns++; return "x", nil }}
	if _, err := deps.runStewardAuto(context.Background(), project.EventPayload{Event: project.EventStewardAuto, Slug: "garden", SuggestionID: id}); err != nil {
		t.Fatal(err)
	}
	if g, _ := st.GetSuggestion(id); turns != 0 || g.Status != store.SuggestionWithdrawn {
		t.Errorf("auto off: turns=%d status=%s, want 0 and withdrawn", turns, g.Status)
	}
}

// An auto step whose task never ran is withdrawn after staleAutoAge, so it
// stops holding its project; a fresh one is left alone.
func TestWithdrawStaleAuto(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mk := func() int64 {
		id, _ := st.CreateSuggestion(store.Suggestion{Source: store.SuggestionSourceSteward, Project: "garden",
			Audience: store.ChatAudience(-100200300), ThreadID: 77, Title: "t", Evidence: "AUTO. e", Change: "c"})
		st.MarkSuggestionsDelivered([]int64{id})
		return id
	}
	id := mk()
	if err := withdrawStaleAuto(st, time.Now()); err != nil {
		t.Fatal(err)
	}
	if g, _ := st.GetSuggestion(id); g.Status != store.SuggestionDelivered {
		t.Fatalf("fresh auto = %s, want delivered", g.Status)
	}
	if err := withdrawStaleAuto(st, time.Now().Add(staleAutoAge+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if g, _ := st.GetSuggestion(id); g.Status != store.SuggestionWithdrawn || g.Note != "did not run" {
		t.Errorf("stale auto = %s (%q), want withdrawn", g.Status, g.Note)
	}
	if open, _ := st.OpenStewardFor("garden", store.StewardOpenAge); len(open) != 0 {
		t.Errorf("stale auto still holds the project: %+v", open)
	}
}
