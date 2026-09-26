package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

func openReviewStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestReviewRunsTurnAndDeliversCappedSuggestions(t *testing.T) {
	st := openReviewStore(t)
	// Last week's decision, with the owner's words, must reach this week's pack.
	old, _ := st.CreateSuggestion(store.Suggestion{Title: "old idea", Change: "x"})
	st.MarkSuggestionsDelivered([]int64{old})
	st.DecideSuggestion(old, store.SuggestionDeclined, "owner", "too noisy")
	st.LogFeedback(42, 0, 9, "reaction", "👎")

	var prompt, sent string
	var sentTo int64
	d := reviewDeps{
		store: st, agentName: "testagent", ownerChatID: 42,
		ownerEval: func(since, until time.Time) string { return "factual_corrections: 2" },
		proposals: func(context.Context) []string { return []string{"history block duplicated"} },
		runTurn: func(ctx context.Context, p string) (string, error) {
			prompt = p
			// The agent files four; the cap delivers three.
			for i := 0; i < 4; i++ {
				st.CreateSuggestion(store.Suggestion{Title: "idea", Evidence: "seen\ntwice", Change: "do it"})
			}
			return "Trimmed my meal-memo rules.", nil
		},
		notify: func(chatID int64, text string) error { sentTo, sent = chatID, text; return nil },
	}
	res, err := d.handle(context.Background(), scheduler.LeasedTask{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "3 suggestion") {
		t.Errorf("result = %q", res)
	}
	for _, want := range []string{"factual_corrections: 2", "👎 ×1", "declined by owner", "too noisy", "history block duplicated", "at most 3",
		"NOT in front of the owner", "sent to your\nowner verbatim", "No preamble, no headings"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if sentTo != 42 || !strings.Contains(sent, "Trimmed my meal-memo rules.") || !strings.Contains(sent, "accept") {
		t.Errorf("delivery to %d = %q", sentTo, sent)
	}
	if strings.Contains(sent, "seen\ntwice") || !strings.Contains(sent, "seen twice") {
		t.Errorf("evidence must be one line in the owner message: %q", sent)
	}
	delivered, _ := st.ListSuggestions([]string{store.SuggestionDelivered}, 0)
	proposed, _ := st.ListSuggestions([]string{store.SuggestionProposed}, 0)
	if len(delivered) != 3 || len(proposed) != 1 {
		t.Errorf("delivered=%d proposed=%d, want 3 and 1", len(delivered), len(proposed))
	}
}

// A failed send must leave suggestions proposed (the next review delivers
// them), and a failed or partially-run turn must not be retried by the queue.
func TestReviewFailuresNeverReplayTheTurn(t *testing.T) {
	st := openReviewStore(t)
	d := reviewDeps{store: st, agentName: "a", ownerChatID: 42,
		runTurn: func(context.Context, string) (string, error) {
			st.CreateSuggestion(store.Suggestion{Title: "t", Change: "c"})
			return "summary", nil
		},
		notify: func(int64, string) error { return errors.New("bot blocked") },
	}
	res, err := d.handle(context.Background(), scheduler.LeasedTask{})
	if err != nil || !strings.Contains(res, "delivery failed") {
		t.Fatalf("send failure: res=%q err=%v (must not return an error: the turn ran)", res, err)
	}
	if p, _ := st.ListSuggestions([]string{store.SuggestionProposed}, 0); len(p) != 1 {
		t.Errorf("after a failed send the suggestion must stay proposed, got %d proposed", len(p))
	}

	d.runTurn = func(context.Context, string) (string, error) { return "", errors.New("process killed") }
	if res, err := d.handle(context.Background(), scheduler.LeasedTask{}); err != nil || !strings.Contains(res, "failed") {
		t.Errorf("a turn that ran and failed: res=%q err=%v, want recorded, not retried", res, err)
	}

	d.runTurn = func(context.Context, string) (string, error) {
		return "", fmt.Errorf("wrapped: %w", process.ErrSessionBusy)
	}
	if _, err := d.handle(context.Background(), scheduler.LeasedTask{}); err == nil {
		t.Error("session busy means the turn never started: it must be retried")
	}
}

func TestReviewSkipsWithoutOwner(t *testing.T) {
	st := openReviewStore(t)
	ran := false
	d := reviewDeps{store: st, runTurn: func(context.Context, string) (string, error) { ran = true; return "", nil },
		notify: func(int64, string) error { return nil }}
	if res, err := d.handle(context.Background(), scheduler.LeasedTask{}); err != nil || ran || !strings.Contains(res, "no owner") {
		t.Fatalf("res=%q err=%v ran=%v", res, err, ran)
	}
}

func TestRegisterReviewScheduleReconciles(t *testing.T) {
	st := openReviewStore(t)
	count := func() (live, all int) {
		scheds, _ := st.ListAllSchedules(false)
		for _, s := range scheds {
			if s.DedupKey == ReviewDedupKey {
				all++
				if s.Enabled {
					live++
				}
			}
		}
		return
	}
	registerReviewSchedule(st, "30 10 * * 3", "UTC", true)
	registerReviewSchedule(st, "30 10 * * 3", "UTC", true)
	if live, all := count(); live != 1 || all != 1 {
		t.Fatalf("after two starts: live=%d all=%d, want 1/1", live, all)
	}
	registerReviewSchedule(st, "0 9 * * 1", "UTC", true) // cadence changed
	if live, all := count(); live != 1 || all != 2 {
		t.Fatalf("after cron change: live=%d all=%d, want 1/2", live, all)
	}
	if s, _ := st.FindScheduleByDedupKey(ReviewDedupKey); s == nil || s.Schedule != "0 9 * * 1" {
		t.Fatalf("live schedule = %+v", s)
	}
	registerReviewSchedule(st, "0 9 * * 1", "UTC", false) // owner chat removed
	registerReviewSchedule(st, "0 9 * * 1", "UTC", false)
	if live, all := count(); live != 0 || all != 2 {
		t.Fatalf("after disable: live=%d all=%d, want 0/2 (no disabled pile-up)", live, all)
	}
}
