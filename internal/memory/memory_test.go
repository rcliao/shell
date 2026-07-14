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

// TestRememberMediaPhotoSearchable is the vision-memory Phase 1 property: a
// photographed thing must be findable by asking about it later. The media-note
// becomes a searchable memory tagged to the chat, carrying a file ref to the
// archived photo.
func TestRememberMediaPhotoSearchable(t *testing.T) {
	m := newTestMemory(t)
	ctx := context.Background()
	m.RememberMedia(ctx, 42,
		"mami's new Eevee Evolutions backpack from Hot Topic, brown canvas with all nine evolutions embroidered, shared while shopping online",
		[]string{"/tmp/media/2026-07/testbot-20260714-msg1.jpg"})

	res, err := m.store.Search(ctx, agentmemory.SearchParams{NS: "agent:test", Query: "Eevee backpack photo", Limit: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) == 0 || !strings.Contains(res[0].Content, "Eevee Evolutions backpack") {
		t.Fatalf("photo memory should be searchable, got %+v", res)
	}
	var hasChatTag, hasPhotoTag bool
	for _, tag := range res[0].Tags {
		if tag == "chat:42" {
			hasChatTag = true
		}
		if tag == "photo" {
			hasPhotoTag = true
		}
	}
	if !hasChatTag || !hasPhotoTag {
		t.Fatalf("photo memory should carry chat + photo tags, got %v", res[0].Tags)
	}

	// The file ref must be attached (drill-down to the actual image).
	got, err := m.store.Get(ctx, agentmemory.GetParams{NS: "agent:test", Key: res[0].Key})
	if err != nil || len(got) == 0 {
		t.Fatalf("get: %v", err)
	}
	if len(got[0].Files) == 0 || !strings.HasSuffix(got[0].Files[0].Path, "msg1.jpg") {
		t.Fatalf("photo memory should carry a file ref to the archived image, got %+v", got[0].Files)
	}
}

// TestRememberMediaNoNoteNoWrite: no note or no archived paths → no memory.
func TestRememberMediaNoNoteNoWrite(t *testing.T) {
	m := newTestMemory(t)
	ctx := context.Background()
	m.RememberMedia(ctx, 42, "", []string{"/tmp/x.jpg"})
	m.RememberMedia(ctx, 42, "a note", nil)
	res, err := m.store.List(ctx, agentmemory.ListParams{NS: "agent:test", Tags: []string{"photo"}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("expected no photo memories, got %d", len(res))
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
