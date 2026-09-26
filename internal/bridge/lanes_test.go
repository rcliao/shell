package bridge

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/process"
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

type threadRecorder struct {
	*fakeTransport
	threads []int64
}

func (r *threadRecorder) Notify(chatID, threadID int64, msg string) {
	r.threads = append(r.threads, threadID)
	r.fakeTransport.Notify(chatID, threadID, msg)
}

// A lane session's late follow-up (background work finishing after the
// reply) must reach the REAL thread, never the lane's negative id.
func TestLaneFollowUpGoesToRealThread(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lane, err := st.LaneSessionThread(42, 7, "trip")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSession(42, lane, "claude-lane"); err != nil {
		t.Fatal(err)
	}
	rec := &threadRecorder{fakeTransport: newFakeTransport()}
	b := &Bridge{store: st, transport: rec}
	b.HandleUnsolicitedTurn(process.SessionKey{ChatID: 42, ThreadID: lane}, process.SendResult{Text: "The hotel list is ready."})
	if len(rec.threads) != 1 || rec.threads[0] != 7 {
		t.Fatalf("follow-up sent to threads %v, want [7]", rec.threads)
	}
}

// With lanes off the thread-bound project block is unchanged (byte-identical
// prompt); the lane block has its own wording.
func TestScopedProjectWordingUnchanged(t *testing.T) {
	b := &Bridge{}
	p := store.Project{Slug: "trip", Title: "Spring trip", Status: "active", MessageThreadID: 9}
	other := store.Project{Slug: "health", Title: "Health", Status: "active"}
	scoped := b.scopedProjectBlock([]store.Project{p, other}, 9)
	if !strings.Contains(scoped, "(other active projects in this chat, not this thread: health)") {
		t.Errorf("thread-bound block changed: %q", scoped)
	}
	if lane := b.laneProjectBlock(p, []store.Project{p, other}); !strings.Contains(lane, "routed to this project's lane") ||
		!strings.Contains(lane, "(other active projects in this chat: health)") {
		t.Errorf("lane block = %q", lane)
	}
}
