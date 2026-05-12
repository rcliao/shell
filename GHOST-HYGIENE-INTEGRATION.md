# Ghost Hygiene Integration — Shell Memory Self-Improvement

## Problem

Shell's heartbeat hygiene runs reflect/consolidate/summarize but has no feedback loop. It acts blindly — no measurement of whether context quality improved, no noise detection, no parameter tuning, no trend tracking.

The `ghost-improve` skill demonstrates a better pattern: measure → diagnose → act → re-measure → report.

## Current State

### What shell does today (bridge.go:702-716)

```
Every heartbeat:
  1. RunReflect()              → promote/decay/prune/dedup/link
  2. SummarizeExchanges()      → consolidate old episodic exchanges  
  3. ConsolidationCandidates() → stash clusters for Claude to consolidate next tick
```

Plus reactive: `CompactionSuggested` triggers background reflect during normal messages.

### What's missing

| Gap | Impact |
|-----|--------|
| No retrieval quality measurement | Don't know if hygiene helps or hurts |
| No precision/noise detection | Irrelevant memories pollute context silently |
| No parameter tuning | Edge expansion uses static defaults, never adapts |
| No trend tracking | No history of memory quality over time |
| No agent-specific eval data | `ghost eval` uses synthetic `eval:ghost` data, not real agent queries |

## Proposed Changes

### Phase 1: Measure + Track (memory.go + bridge.go)

**Add `HealthCheck()` to memory module:**

```go
// HealthCheck runs a lightweight quality check on context assembly.
// Returns a score and optional diagnosis for logging/stashing.
func (m *Memory) HealthCheck(ctx context.Context, agentNS string) HealthResult {
    // 1. Sample 3-5 recent user queries from exchange logs
    // 2. For each, run store.Context() and check:
    //    - Are pinned memories present? (sanity check)
    //    - How many results have importance < 0.3? (noise indicator)
    //    - Are any consolidation summaries returned alongside their children? (suppression working?)
    // 3. Return scores + diagnosis
}

type HealthResult struct {
    QueriesTested  int
    AvgResultCount float64
    NoiseRatio     float64  // fraction of low-importance results
    PinnedPresent  bool
    Diagnosis      string   // human-readable summary
}
```

**Call from heartbeat post-processing (bridge.go):**

After `RunReflect()` and `SummarizeExchanges()`, add:

```go
health := b.memory.HealthCheck(ctx, agentNS)
slog.Info("memory health", "noise_ratio", health.NoiseRatio, "pinned_ok", health.PinnedPresent)
```

**Store outcome as sensory memory for trend tracking:**

```go
// Store hygiene outcome for trend tracking (auto-decays via sensory tier)
m.store.Put(ctx, PutParams{
    NS:      agentNS,
    Key:     fmt.Sprintf("hygiene-%d", time.Now().Unix()),
    Content: fmt.Sprintf("Reflect: %d eval, %d merged, %d decayed. Health: noise=%.0f%%", ...),
    Kind:    "episodic",
    Tags:    []string{"hygiene"},
    Tier:    "sensory",
    TTL:     "30d",
})
```

### Phase 2: Diagnose + Surface (memory.go + heartbeat.go)

**Add noise detection to ConsolidationCandidates():**

When building consolidation candidates for the next heartbeat, also include:
- Top 3 memories with highest access count but lowest importance (popular noise)
- Memories that appear in context results but have tier=sensory and age > 24h (should have been promoted or decayed)

Surface these as a "[Memory noise detected]" section in heartbeat enrichment, asking Claude to diminish or archive them.

### Phase 3: Adaptive Parameters (future)

**Add per-agent parameter storage:**

Store tuned edge expansion parameters as pinned memories:
```
key: ghost-params
content: GHOST_EDGE_DAMPING=0.2 GHOST_EDGE_MIN_WEIGHT=0.3
```

The memory module reads these on startup and passes them to context assembly. Agents can tune their own retrieval characteristics via the ghost-improve skill.

## Files to Modify

| Phase | File | Change |
|-------|------|--------|
| 1 | `internal/memory/memory.go` | Add `HealthCheck()`, add hygiene outcome logging |
| 1 | `internal/bridge/bridge.go` | Call `HealthCheck()` after reflect in heartbeat post-processing |
| 2 | `internal/memory/memory.go` | Extend `ConsolidationCandidates()` with noise detection |
| 2 | `internal/bridge/heartbeat.go` | Add noise section to heartbeat enrichment |
| 3 | `internal/memory/memory.go` | Read per-agent params from pinned memory |

## Success Criteria

- Hygiene outcomes are logged every heartbeat (visible in daemon logs)
- Noise ratio trends are trackable via `ghost search -n agent:pikamini -t hygiene`
- Claude actively diminishes noisy memories when surfaced during heartbeat
- Context quality measurably improves over time (lower noise ratio trend)

## Non-Goals

- Not replacing the `ghost-improve` skill (that's for algorithm tuning on eval data)
- Not adding a full eval framework to shell (ghost already has that)
- Not changing ghost's core retrieval — only using its existing tools better
