package bridge

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/route"
	"github.com/rcliao/shell/internal/store"
)

type fakeLaneRouter struct {
	lane string
	conf float64
	err  error
}

func (f *fakeLaneRouter) Name() string { return "jev-v2" }
func (f *fakeLaneRouter) Choose(context.Context, route.Input) (route.Choice, error) {
	return route.Choice{Lane: f.lane, Confidence: f.conf, Latency: 90 * time.Millisecond}, f.err
}

func TestLaneForTurn(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Projects live in chat 7000; test chat 42 borrows them as its lanes.
	if _, err := st.CreateProject(store.Project{Slug: "trip", Title: "Spring trip", Status: "active", ChatID: 7000,
		Instructions: "plan the spring trip"}); err != nil {
		t.Fatal(err)
	}
	fr := &fakeLaneRouter{lane: "trip", conf: 0.9}
	b := &Bridge{store: st}

	// Lanes off for the chat: untouched.
	if th, block, on := b.laneForTurn(context.Background(), 42, 0, "hi"); on || th != 0 || block != "" {
		t.Fatalf("lanes off: th=%d on=%v block=%q", th, on, block)
	}
	b.SetLanes(map[int64]int64{42: 7000}, fr)

	th, block, on := b.laneForTurn(context.Background(), 42, 0, "book the flights")
	if !on || th >= 0 || !strings.Contains(block, "[Project]") || !strings.Contains(block, "trip") {
		t.Fatalf("project lane: th=%d on=%v block=%q", th, on, block)
	}
	projectThread := th

	// A sure general message goes back to the chat's own session.
	fr.lane, fr.conf = "general", 0.95
	if th, block, _ := b.laneForTurn(context.Background(), 42, 0, "good morning"); th != 0 || block != "" {
		t.Errorf("general lane: th=%d block=%q", th, block)
	}
	// Back to the trip: the same session as before.
	fr.lane, fr.conf = "trip", 0.9
	if th, _, _ := b.laneForTurn(context.Background(), 42, 0, "and the hotel?"); th != projectThread {
		t.Errorf("the trip lane must reuse its session: %d vs %d", th, projectThread)
	}
	// Router down: stay in the previous lane (trip), never fail the turn.
	fr.err = errors.New("timeout")
	if th, _, on := b.laneForTurn(context.Background(), 42, 0, "what time?"); !on || th != projectThread {
		t.Errorf("router failure: th=%d, want the previous lane's session %d", th, projectThread)
	}
	rows, _ := st.RouteDecisions("lane", time.Now().Add(-time.Hour))
	if len(rows) != 4 || rows[0].Lane != "trip" || rows[1].Lane != "general" || rows[3].Lane != "trip" {
		t.Errorf("lane decisions = %+v", rows)
	}
}
