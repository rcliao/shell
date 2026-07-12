package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	agentmemory "github.com/rcliao/ghost"
)

func newTestMemory(t *testing.T) *Memory {
	t.Helper()
	t.Setenv("GHOST_EMBED_PROVIDER", "none") // FTS-only: fast + deterministic
	db := filepath.Join(t.TempDir(), "mem.db")
	profiles := map[string]ProfileConfig{"p": {AgentNS: "agent:test"}}
	m, err := New(db, 2000, nil, 500, nil, 3000, profiles, map[int64]string{42: "p"})
	if err != nil {
		t.Fatalf("new memory: %v", err)
	}
	return m
}

// TestLogExchangeDistillsSalientFact is the B1 property: a fact the user just
// stated must be searchable on the next query (not buried in the sensory tier).
func TestLogExchangeDistillsSalientFact(t *testing.T) {
	m := newTestMemory(t)
	ctx := context.Background()
	m.LogExchange(ctx, 42, "I always park the car in lot B on level 3.", "Got it, noted.")

	res, err := m.store.Search(ctx, agentmemory.SearchParams{NS: "agent:test", Query: "where did I park the car", Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var found bool
	for _, r := range res {
		if strings.Contains(strings.ToLower(r.Content), "lot b") && r.Tier != "sensory" {
			found = true
		}
	}
	if !found {
		t.Fatalf("distilled same-day fact should be searchable at a non-sensory tier; got %d results %+v", len(res), res)
	}
}

// TestLogExchangeChatterNotDistilled: pure chatter yields no searchable fact.
func TestLogExchangeChatterNotDistilled(t *testing.T) {
	m := newTestMemory(t)
	ctx := context.Background()
	m.LogExchange(ctx, 42, "haha ok thanks", "sure thing")

	same, err := m.store.List(ctx, agentmemory.ListParams{NS: "agent:test", Tags: []string{"same-day"}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(same) != 0 {
		t.Errorf("chatter should not produce a distilled fact, got %d: %+v", len(same), same)
	}
}
