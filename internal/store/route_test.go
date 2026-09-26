package store

import (
	"testing"
	"time"
)

func TestRouteDecisionsAndBestLabel(t *testing.T) {
	s, done := newTestStore(t)
	defer done()

	if lane, err := s.LastRouteLane("live", "jev", 42, 7); err != nil || lane != "" {
		t.Fatalf("first message: lane=%q err=%v", lane, err)
	}
	h := TextHash("  lunch memo  ")
	if h != TextHash("lunch memo") {
		t.Error("the hash must ignore surrounding space")
	}
	for i, lane := range []string{"general", "health"} {
		if err := s.LogRouteDecision(RouteDecision{Source: "live", ChatID: 42, ThreadID: 7, MsgID: int64(10 + i),
			MsgAt: time.Now(), TextHash: h, Backend: "jev", Choice: lane, Confidence: 0.9, Lane: lane}); err != nil {
			t.Fatal(err)
		}
	}
	if lane, _ := s.LastRouteLane("live", "jev", 42, 7); lane != "health" {
		t.Errorf("last lane = %q, want health", lane)
	}
	if lane, _ := s.LastRouteLane("live", "jev", 42, 8); lane != "" {
		t.Errorf("another thread must not share the previous lane, got %q", lane)
	}
	got, err := s.RouteDecisions("live", time.Now().Add(-time.Hour))
	if err != nil || len(got) != 2 || got[1].Lane != "health" {
		t.Fatalf("decisions = %+v, %v", got, err)
	}

	for _, l := range []RouteLabel{
		{ChatID: 42, ThreadID: 7, TextHash: h, Lane: "general", Source: "judge", Sure: true},
		{ChatID: 42, ThreadID: 7, TextHash: h, Lane: "health", Source: "human", Sure: true, Note: "meal log"},
		{ChatID: 42, ThreadID: 7, TextHash: h, Lane: "japan", Source: "project"},
	} {
		if err := s.UpsertRouteLabel(l); err != nil {
			t.Fatal(err)
		}
	}
	// Re-labelling by the same source replaces, not duplicates.
	s.UpsertRouteLabel(RouteLabel{ChatID: 42, ThreadID: 7, TextHash: h, Lane: "health", Source: "judge", Sure: false})
	best, err := s.BestRouteLabels()
	if err != nil {
		t.Fatal(err)
	}
	if b := best[LabelKey(42, 7, h)]; b.Source != "human" || b.Lane != "health" {
		t.Fatalf("best label = %+v, want the human one", b)
	}
	if LabelKey(-100200300, 0, "ab") != "-100200300/0/ab" {
		t.Errorf("label key = %q", LabelKey(-100200300, 0, "ab"))
	}
	if err := s.ClearRouteDecisions("live"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.RouteDecisions("live", time.Now().Add(-time.Hour)); len(got) != 0 {
		t.Error("clear must remove the source's decisions")
	}
}

func TestLaneSessionThread(t *testing.T) {
	s, done := newTestStore(t)
	defer done()
	if id, _ := s.LaneSessionThread(42, 7, "general"); id != 7 {
		t.Errorf("general must keep the real thread, got %d", id)
	}
	a, err := s.LaneSessionThread(42, 7, "japan")
	if err != nil || a >= 0 {
		t.Fatalf("project lane id = %d, %v (want negative)", a, err)
	}
	b, _ := s.LaneSessionThread(42, 7, "health")
	c, _ := s.LaneSessionThread(42, 0, "japan")
	again, _ := s.LaneSessionThread(42, 7, "japan")
	if again != a || b == a || c == a || c == b {
		t.Errorf("ids must be stable and distinct: a=%d again=%d b=%d c=%d", a, again, b, c)
	}
	if s.RealThread(42, a) != 7 || s.RealThread(42, 7) != 7 || s.RealThread(42, -999) != -999 {
		t.Error("RealThread must map a lane session back to its thread and leave others alone")
	}
	if other, _ := s.LaneSessionThread(-100200300, 7, "japan"); other != -1 {
		t.Errorf("another chat allocates from -1, got %d", other)
	}
}
