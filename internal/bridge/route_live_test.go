package bridge

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/decide"
	"github.com/rcliao/shell/internal/store"
)

func TestLogLiveRouteStickyPerBackend(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b := &Bridge{store: st}
	projects := []decide.Project{{Slug: "japan", Title: "日本行程"}, {Slug: "health", Title: "健康紀錄"}}

	// A sure Japan message, then an unsure "general" in the same thread: sticky.
	b.logLiveRoute(decide.Turn{ChatID: 42, ThreadID: 7, MsgID: 101, ChatKind: "dm", Message: "日本行程先訂機票", Projects: projects},
		"which_project", "japan", 0.9, 150*time.Millisecond)
	b.logLiveRoute(decide.Turn{ChatID: 42, ThreadID: 7, MsgID: 102, ChatKind: "dm", Message: "那飯店呢", Projects: projects},
		"which_project", "none", 0.4, 120*time.Millisecond)

	b.logLiveRoute(decide.Turn{ChatID: 42, ThreadID: 7, MsgID: 102, ChatKind: "dm", Message: "那飯店呢", Projects: projects},
		"which_project_v2", "japan", 0.8, 120*time.Millisecond)

	rows, err := st.RouteDecisions("live", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("want 2 turns × (jev, keyword) + 1 jev-v2 = 5 rows, got %d", len(rows))
	}
	byKey := map[string]store.RouteDecision{}
	for _, r := range rows {
		byKey[r.Backend+"/"+string(rune('0'+r.MsgID-100))] = r
	}
	j2 := byKey["jev/2"]
	if j2.Choice != "general" || j2.Lane != "japan" || !j2.Sticky || j2.LanePrev != "japan" || j2.MsgID != 102 {
		t.Errorf("jev second turn = %+v, want sticky to japan", j2)
	}
	if v2 := byKey["jev-v2/2"]; v2.Lane != "japan" || v2.Sticky {
		t.Errorf("jev-v2 = %+v (its own previous lane: none, so no sticky)", v2)
	}
	if k1 := byKey["keyword/1"]; k1.Lane != "japan" {
		t.Errorf("keyword first turn = %+v, want japan from the title words", k1)
	}
	if j1 := byKey["jev/1"]; j1.LatencyMS != 150 || j1.TextHash != store.TextHash("日本行程先訂機票") {
		t.Errorf("jev first turn = %+v", j1)
	}
}
