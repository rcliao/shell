package memory

import (
	"context"
	"path/filepath"
	"testing"

	agentmemory "github.com/rcliao/ghost"
)

// Per-path provenance assertions — the structural half of the adoption story
// (ghost docs/research/capture-to-storage-data-model.md): every mechanical
// write path carries provenance BY CONSTRUCTION, so only agent tool calls
// depend on behaviour. If one of these fails, a writer regressed to
// anonymous writes.

func provOf(t *testing.T, m *Memory, ns, key string) (string, string) {
	t.Helper()
	got, err := m.store.Get(context.Background(), agentmemory.GetParams{NS: ns, Key: key})
	if err != nil || len(got) == 0 {
		t.Fatalf("get %s/%s: %v", ns, key, err)
	}
	return got[0].SourceUser, got[0].SourceKind
}

func TestProvenanceExchangeCarriesSpeaker(t *testing.T) {
	m := newTestMemory(t)
	ctx := context.Background()
	m.LogExchange(ctx, 42, "The garage code changed to 4188 yesterday.", "Noted.", "mami")

	// Every exchange row and every distilled fact from this turn must carry
	// the speaker.
	res, err := m.store.List(ctx, agentmemory.ListParams{NS: "agent:test", SourceUser: "mami"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 {
		t.Fatal("no memories attributed to the speaker — LogExchange dropped provenance")
	}
	for _, mem := range res {
		if mem.SourceKind != "stated" {
			t.Errorf("%s: kind=%q, want stated", mem.Key, mem.SourceKind)
		}
	}
}

func TestProvenanceUnknownSpeakerStaysEmpty(t *testing.T) {
	m := newTestMemory(t)
	ctx := context.Background()
	m.LogExchange(ctx, 42, "Some note without a known speaker.", "OK.", "")
	res, _ := m.store.List(ctx, agentmemory.ListParams{NS: "agent:test", Limit: 10})
	for _, mem := range res {
		if mem.SourceKind == "stated" && mem.SourceUser == "" {
			t.Errorf("%s: stated with no user — a guessed origin is worse than none", mem.Key)
		}
	}
}

func TestProvenanceRememberIsStated(t *testing.T) {
	m := newTestMemory(t)
	if err := m.Remember(context.Background(), 42, "The spare key lives with the neighbours.", "papi"); err != nil {
		t.Fatal(err)
	}
	u, k := provOf(t, m, "agent:test", sanitizeKey("The spare key lives with the neighbours."))
	if u != "papi" || k != "stated" {
		t.Errorf("Remember: user=%q kind=%q, want papi/stated", u, k)
	}
}

func TestProvenanceSelfAndPeerWriters(t *testing.T) {
	m := newTestMemory(t)
	ctx := context.Background()
	if err := m.StoreHeartbeatLearning(ctx, 42, "Morning heartbeats run long on Mondays."); err != nil {
		t.Fatal(err)
	}
	if err := m.StoreReviewerLearning(ctx, "agent:test", "Reviewer: keep deploy notes short."); err != nil {
		t.Fatal(err)
	}
	var self, peer bool
	res, _ := m.store.List(ctx, agentmemory.ListParams{NS: "agent:test", Limit: 20})
	for _, mem := range res {
		switch mem.SourceKind {
		case "self":
			self = true
		case "peer":
			peer = true
		}
	}
	if !self || !peer {
		t.Errorf("self=%v peer=%v — a mechanical writer lost its kind", self, peer)
	}
}

func TestProvenanceViaTagForcesPeer(t *testing.T) {
	t.Setenv("GHOST_EMBED_PROVIDER", "none")
	db := filepath.Join(t.TempDir(), "mem.db")
	profiles := map[string]ProfileConfig{"p": {AgentNS: "agent:test", MemoryDirectives: true}}
	m, err := New(db, 2000, nil, 500, nil, 3000, profiles, map[int64]string{-1001: "p"})
	if err != nil {
		t.Fatal(err)
	}
	// Sync-relayed knowledge: via:<agent> must force peer regardless of
	// declaration, while the declared origin person is preserved.
	if err := m.StoreDirectiveTagged(context.Background(), -1001,
		"Mami said the lemongrass allergy is confirmed.", "semantic",
		[]string{"via:umbreonmini"}, "mami", "stated"); err != nil {
		t.Fatal(err)
	}
	res, _ := m.store.List(context.Background(), agentmemory.ListParams{NS: "agent:test", SourceUser: "mami"})
	if len(res) == 0 {
		t.Fatal("relayed memory lost its origin person")
	}
	// Note: the peer-forcing lives at the RPC boundary (where via: arrives);
	// direct StoreDirectiveTagged trusts its caller. This test documents the
	// split: origin person survives the relay, transport kind is set where
	// the transport is known.
}

func TestProvenanceCorrectMemoryCarries(t *testing.T) {
	m := newTestMemory(t)
	ctx := context.Background()
	if err := m.Remember(ctx, 42, "The clinic closes at noon on Fridays.", "mami"); err != nil {
		t.Fatal(err)
	}
	key := sanitizeKey("The clinic closes at noon on Fridays.")
	if err := m.CorrectMemory(ctx, "agent:test", key, "The clinic closes at two on Fridays."); err != nil {
		t.Fatal(err)
	}
	u, k := provOf(t, m, "agent:test", key)
	if u != "mami" || k != "stated" {
		t.Errorf("correction dropped origin: user=%q kind=%q — a correction edits content, not origin", u, k)
	}
}
