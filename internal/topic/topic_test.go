package topic

import (
	"context"
	"os"
	"testing"
	"time"

	memory "github.com/rcliao/ghost"
)

// newTestStore opens a fresh ghost sandbox for one test.
func newTestStore(t *testing.T) (memory.Store, func()) {
	t.Helper()
	tmp, err := os.CreateTemp("", "topic-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	path := tmp.Name()
	tmp.Close()
	store, err := memory.NewSQLiteStore(path)
	if err != nil {
		os.Remove(path)
		t.Fatal(err)
	}
	return store, func() {
		os.Remove(path)
		os.Remove(path + "-shm")
		os.Remove(path + "-wal")
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	reg := NewRegistry(store, 12345)

	// Empty list
	topics, err := reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(topics) != 0 {
		t.Errorf("expected empty registry, got %d topics", len(topics))
	}

	// Upsert two topics
	err = reg.Upsert(ctx, Topic{Name: "plants", Description: "houseplant care, soil moisture, root health"})
	if err != nil {
		t.Fatal(err)
	}
	err = reg.Upsert(ctx, Topic{Name: "meals", Description: "breakfast/lunch/dinner memos"})
	if err != nil {
		t.Fatal(err)
	}

	// List returns both
	topics, err = reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(topics) != 2 {
		t.Errorf("expected 2 topics, got %d", len(topics))
	}

	// Get specific
	p, err := reg.Get(ctx, "plants")
	if err != nil || p == nil {
		t.Fatalf("Get plants: err=%v p=%v", err, p)
	}
	if p.Description == "" || p.FirstSeen.IsZero() || p.LastUsed.IsZero() {
		t.Errorf("topic fields not populated: %+v", p)
	}

	// IncrementTurnCount
	err = reg.IncrementTurnCount(ctx, "plants")
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := reg.Get(ctx, "plants")
	if p2.TurnCount != 1 {
		t.Errorf("expected turn_count=1, got %d", p2.TurnCount)
	}

	// Prune
	count, err := reg.Prune(ctx, 1*time.Nanosecond) // anything older than 1ns gets pruned
	if err != nil {
		t.Fatal(err)
	}
	// All topics should be marked pruned since they're older than 1ns
	if count == 0 {
		t.Errorf("expected pruning to mark topics; pruned %d", count)
	}

	// List excludes pruned
	topics, _ = reg.List(ctx)
	if len(topics) != 0 {
		t.Errorf("expected 0 active topics after prune, got %d", len(topics))
	}
}

func TestHybridClassifierKeywordFast(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	reg := NewRegistry(store, 12345)
	h := NewHybrid(reg, StubHaiku{})

	cases := []struct {
		msg      string
		wantName string
		wantSrc  string
	}{
		{"my brazilian wood's leaves are droopy from overwatering", "plants", "keyword"}, // 3+ signals → fast-path
		{"早餐memo - toast, latte and dairy", "meals", "keyword"},
		{"hey nova", "general", "fallback"}, // no signal anywhere
	}
	for _, c := range cases {
		r, err := h.Classify(context.Background(), c.msg)
		if err != nil {
			t.Fatalf("Classify(%q): %v", c.msg, err)
		}
		if r.Topic.Name != c.wantName {
			t.Errorf("Classify(%q).topic = %q, want %q (src=%s conf=%.1f)",
				c.msg, r.Topic.Name, c.wantName, r.Source, r.Confidence)
		}
	}
}

func TestHybridClassifierNilHaiku(t *testing.T) {
	// topic_keyword_only wires a nil LLM client (cycle 149 / V2-H1): the
	// cascade must classify keyword-signal messages normally and settle
	// ambiguous ones as general, never erroring or blocking on the LLM tier.
	store, cleanup := newTestStore(t)
	defer cleanup()
	reg := NewRegistry(store, 12345)
	h := NewHybrid(reg, nil)

	cases := []struct {
		msg      string
		wantName string
	}{
		{"my brazilian wood's leaves are droopy from overwatering", "plants"},
		{"早餐memo - toast, latte and dairy", "meals"},
		{"hmm what do you think about that thing", "general"},
	}
	for _, c := range cases {
		r, err := h.Classify(context.Background(), c.msg)
		if err != nil {
			t.Fatalf("Classify(%q) with nil haiku: %v", c.msg, err)
		}
		if r.Topic.Name != c.wantName {
			t.Errorf("Classify(%q).topic = %q, want %q (src=%s)",
				c.msg, r.Topic.Name, c.wantName, r.Source)
		}
		if r.Source == "haiku" {
			t.Errorf("Classify(%q) reported haiku source with nil client", c.msg)
		}
	}
}

func TestHybridClassifierHaikuFallback(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	reg := NewRegistry(store, 12345)
	ctx := context.Background()

	// Seed registry so stub Haiku has something to match
	reg.Upsert(ctx, Topic{Name: "fortune", Description: "daily fortune readings"})

	h := NewHybrid(reg, StubHaiku{})

	// "any fortune for tomorrow?" — single fortune keyword (conf=1) ≤ high
	// threshold → falls into Haiku. Stub matches "fortune" substring.
	r, err := h.Classify(ctx, "any fortune for tomorrow?")
	if err != nil {
		t.Fatal(err)
	}
	if r.Topic.Name != "fortune" {
		t.Errorf("expected fortune topic from Haiku stub, got %q (src=%s)", r.Topic.Name, r.Source)
	}
}

func TestClassifierCache(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	reg := NewRegistry(store, 12345)
	h := NewHybrid(reg, StubHaiku{})
	ctx := context.Background()

	msg := "my plant's leaves are wilting after overwatering"
	r1, _ := h.Classify(ctx, msg)
	r2, _ := h.Classify(ctx, msg)
	if r2.Source != "cache" {
		t.Errorf("expected cache hit, got source=%q", r2.Source)
	}
	if r1.Topic.Name != r2.Topic.Name {
		t.Errorf("cached result differs: %q vs %q", r1.Topic.Name, r2.Topic.Name)
	}
}
