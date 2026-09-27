package route

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/store"
)

func TestJudgeMissesAndRecurringSubjects(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.CreateProject(store.Project{Slug: "health", Title: "Health log", Status: "active", ChatID: 7})
	st.SaveSession(7, 0, "c")
	s7, _ := st.GetSession(7, 0)
	st.SaveSession(42, 0, "t")
	s42, _ := st.GetSession(42, 0)
	for _, m := range []string{"lunch memo: noodles", "any chiikawa restock?", "chiikawa at miniso?", "still no chiikawa"} {
		st.LogMessage(s7.ID, "user", m)
	}
	st.LogMessage(s42.ID, "user", "test chat message")
	// Acted-on routing: the lunch memo went general.
	st.LogRouteDecision(store.RouteDecision{Source: "lane", ChatID: 7, MsgAt: time.Now(), TextHash: store.TextHash("lunch memo: noodles"), Backend: "jev-v2", Lane: "general"})

	calls := 0
	fake := func(_ context.Context, _ string, prompt string) (string, error) {
		calls++
		if strings.Contains(prompt, "test chat message") {
			t.Error("an excluded chat must never be judged")
		}
		return `[{"n":1,"lane":"health","sure":true,"subject":"lunch log"},
			{"n":2,"lane":"general","sure":true,"subject":"Chiikawa restock hunt"},
			{"n":3,"lane":"general","sure":true,"subject":"chiikawa restock hunt"},
			{"n":4,"lane":"general","sure":true,"subject":"chiikawa restock hunt"}]`, nil
	}
	n, c, err := RunJudge(context.Background(), st, "judge", time.Now().Add(-time.Hour), false, map[int64]bool{42: true}, fake)
	if err != nil || n != 4 || c != 1 {
		t.Fatalf("judge: labels=%d calls=%d err=%v", n, c, err)
	}
	// A second pass finds everything labelled (with subjects): no call.
	if _, c, _ := RunJudge(context.Background(), st, "judge", time.Now().Add(-time.Hour), false, map[int64]bool{42: true}, fake); c != 0 {
		t.Errorf("already labelled: %d calls, want 0", c)
	}

	misses, err := Misses(st, time.Now().Add(-time.Hour), nil)
	if err != nil || len(misses) != 1 || misses[0].Routed != "general" || misses[0].Want != "health" || misses[0].Text != "lunch memo: noodles" {
		t.Fatalf("misses = %+v, %v", misses, err)
	}

	// Same day, so 1 distinct day: the 2-day bar holds it back; 1 day lets it through.
	if subs, _ := RecurringSubjects(st, time.Now().Add(-time.Hour), 3, 2, nil); len(subs) != 0 {
		t.Errorf("one day of messages is not recurring yet: %+v", subs)
	}
	subs, _ := RecurringSubjects(st, time.Now().Add(-time.Hour), 3, 1, nil)
	if len(subs) != 1 || subs[0].Name != "chiikawa restock hunt" || subs[0].Count != 3 || subs[0].ChatID != 7 {
		t.Fatalf("recurring = %+v (subjects are normalized to lower case)", subs)
	}
}
