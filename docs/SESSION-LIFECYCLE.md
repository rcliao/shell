# Session Lifecycle (Shell ↔ Claude Code ↔ Ghost)

Status: draft
Owners: pikamini team
Last updated: 2026-04-19

## 1. Problem

Shell wraps Claude Code CLI. Each `(chat_id, thread_id)` has a persistent session whose Claude-side `claude_session_id` (UUID) is reused via `--resume`. The system prompt is **only** appended on fresh sessions (`internal/process/manager.go:232`). That means:

- Pinned ghost memories added after a session is born never reach the active session — the cached prefix inside Claude CLI wins.
- Behavioral updates show up in heartbeat/phantom-chat (which rebuilds often) but are invisible in mami's/papi's DMs until the session is killed.
- We currently only have **reactive** token-threshold compaction; sessions live effectively forever.
- There is no concept of a session "generation" — we cannot rotate, version, or checkpoint.

The symptoms: stale day-of-week (fixed separately by per-turn injection), stale behavioral learnings, heartbeats that diverge from DM state, 106 duplicate scheduling memories leaking into ancient sessions.

## 2. Goals & non-goals

**Goals**
- Keep prefix cache warm for the hot path. (Don't mutate system prompt mid-session.)
- Make pinned memory and skill-catalog changes visible to ongoing conversations within one turn.
- Rotate sessions safely — carry forward relevant ghost context automatically.
- Introduce proactive compaction so conversations don't hit the 95% panic threshold.
- Unify heartbeat/phantom chat into the same lifecycle vocabulary.

**Non-goals**
- Rewriting ghost. We keep the current memory store and namespaces.
- Cross-agent session sharing (still one session per `(agent, chat_id, thread_id)`).
- Changing Claude Code CLI itself. We work within `--resume` / `--append-system-prompt` semantics.

## 3. Research findings (summary)

Three patterns repeat across Claude Code, OpenAI Assistants/Responses, LangGraph, Letta/MemGPT, Windsurf, and Hermes:

1. **Freeze the prefix, inject changes as user-role.** System prompt is cache-hot. Deliver updates via user-role or tool output. Hermes makes this explicit; Claude Code's compaction summary is a user message, not a system-prompt edit.
2. **Two channels: frozen prefix + live sidecar.** LangGraph `Store`, OpenAI per-run `additional_instructions`, Letta core blocks. The sidecar is read fresh every turn.
3. **Proactive compaction beats reactive.** Claude Cookbook's soft threshold (~60%) with background summarization. Letta's sleep-time agents. Claude CLI itself auto-compacts at ~95% but doesn't rotate the prefix.

We adopt all three.

## 4. Design

### 4.1 Identity

A session row is keyed by `(chat_id, thread_id)`, same as today. We add two new columns:

- `generation INT NOT NULL DEFAULT 1` — bumps on rotation. Conversation continuity is preserved via a carry-forward summary.
- `prefix_hash TEXT` — SHA-256 of the frozen prefix content (identity + skills catalog + pinned-memory snapshot) at generation start. Used to detect when rotation is needed.

The `claude_session_id` UUID still uniquely identifies a Claude CLI conversation; it resets on rotation.

### 4.2 States

```
        ┌────── new row ──────┐
        ▼                      │
     fresh ──first turn──▶ active ──idle 5min──▶ cold
        ▲                      │ │                │
        │                      │ └────next turn───┤
        │                      │                  ▼
        │                tokens>60%          (warm up)
        │                      │
        │                      ▼
        │                 compacting ──summary ready──▶ active (same generation, same UUID)
        │                      
        │          hard trigger (skill/identity change)
        │          soft trigger (7d, pinned delta > 1k tokens)
        │                      │
        └─────────── rotating ◀┘
                      │
                      ▼
                (bump generation,
                 new UUID, fresh prefix,
                 carry-forward summary)
                      │
                      ▼
                    active
```

No `stale` state — that was a debugging artifact. Archival is a separate cleanup job (30-day idle).

### 4.3 Two-channel context

**Channel A — frozen prefix (cache-hot, built once per generation)**
- Agent identity (CLAUDE.md-equivalent loaded from agent dir)
- Skills catalog (inlined core skills + compact list)
- Bridge rules
- Timezone/time guidance (no specific date — see `prompt.go`)
- **Pinned memories snapshot** — exactly the ghost memories with `pinned=true` at generation start

Written via `--append-system-prompt` when `claude_session_id == ""`. Never touched again within a generation. Hashed into `prefix_hash`.

**Channel B — per-turn prefix (always fresh, small)**
Expands today's `injectCurrentTime` into `injectPerTurnContext`:
```
[Current time: Monday 2026-04-19 10:42 PDT | America/Los_Angeles]
[Memory updates since session start: <diff>]   ← only when Channel A differs from live ghost
[Active tasks: <brief>]                          ← only when non-empty
<user message>
```

Size budget: ~500 tokens max. If the "memory updates" diff exceeds ~1k tokens, soft-trigger rotation instead of injecting (the diff has become cheaper to rebake into the prefix).

### 4.4 Rotation triggers

**Hard (immediate — rotate before next turn runs):**
- Agent identity content changed
- Skills catalog hash changed (skill added / removed / edited)
- Manual `shell session rotate <chat-id>`

**Soft (rotate at next natural boundary — next user turn):**
- `now - generation_started_at > 7 days`
- Pinned-memory delta since generation start exceeds ~1k tokens
- Operator flag set via RPC (`/session/rotate-next`)

Hard triggers mark all sessions dirty; soft triggers set a per-session flag.

### 4.5 Ghost integration at rotation

This is the part the user explicitly called out. When we rotate, we want to **carry relevant memories forward**, not lose context.

At rotation, the bridge produces three things:

1. **Conversation summary** — run the same compaction pass we already do, but written to the `session_summaries` table with `(chat_id, thread_id, generation)` key. This is the "Previously in this chat" note.
2. **Relevant-memory pack** — call `memory.Context(ctx, query=recent_exchanges, budget=~800 tokens)`. Ghost already does this via scoring; we just ask it for the top-N memories semantically relevant to the last ~2k tokens of conversation. These are *not* pinned memories; they're the long-tail that would otherwise be re-retrieved ad-hoc.
3. **Consolidation kick** — the closed generation is a natural boundary to run `memory.RunReflect()` and consolidation for this chat's exchanges. We already do this on heartbeats; rotation gives us a second trigger.

The new generation's first user-role message becomes:
```
[Previously in this chat (generation N-1 summary):
 <summary from step 1>
]
[Relevant memory context:
 <top-N memories from step 2, tagged by kind>
]
<actual new user message>
```

This is the Claude Code pattern (compaction summary as user message) applied to rotation.

Crucially: pinned memories reload into Channel A automatically at rotation — they don't need to be in the carry-forward pack. The pack handles *semantic* memory the old session was relying on.

### 4.6 Proactive compaction

Add a soft threshold at 60% of `maxSessionTokens`. When crossed:
- Transition to `compacting` state
- Spawn a background goroutine that issues `/compact` to the Claude CLI session
- On success, swap in the compacted state; UUID stays the same
- If the user sends a message during compaction, queue it — don't block

Reactive threshold stays at 95% as a safety net.

This is Claude Cookbook's "instant compaction" pattern.

### 4.7 Heartbeat & phantom chat

The phantom system chat (`chat_id=0`) follows the same lifecycle. Practical implication:

- Heartbeat rotation triggers on skill/identity change, same as user chats — keeps heartbeats aligned with current behavioral state.
- Heartbeats no longer auto-rebuild every turn for freshness (they used to because we killed sessions aggressively); now they stay warm and rotate on real triggers, cheaper.
- Behavioral learnings written during a deep reflection turn will land in the ghost store and become pinned, which will be picked up by the next rotation of the user chat sessions — not immediately, but within one Channel B diff turn.

## 5. Schema changes

New columns on `sessions`:
```sql
ALTER TABLE sessions ADD COLUMN generation INTEGER NOT NULL DEFAULT 1;
ALTER TABLE sessions ADD COLUMN prefix_hash TEXT;
ALTER TABLE sessions ADD COLUMN generation_started_at TIMESTAMP;
ALTER TABLE sessions ADD COLUMN rotate_pending INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN compact_state TEXT NOT NULL DEFAULT '';
  -- '' | 'compacting' — small state machine; active state is implicit
```

New table `session_summaries`:
```sql
CREATE TABLE session_summaries (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  chat_id INTEGER NOT NULL,
  thread_id INTEGER NOT NULL DEFAULT 0,
  generation INTEGER NOT NULL,
  closed_at TIMESTAMP NOT NULL,
  summary TEXT NOT NULL,
  memory_pack TEXT,              -- JSON: the relevant-memory pack
  UNIQUE(chat_id, thread_id, generation)
);
```

Follow the migration ordering rule from prior feedback: CREATE INDEX statements referencing new columns run **after** any table rebuilds.

## 6. Implementation plan (five steps)

### Step 1 — Schema + generation plumbing
- Add columns above
- Backfill existing rows: `generation=1`, `generation_started_at=started_at`, `prefix_hash=hash(current system prompt pieces)`
- Update `store.SaveSession` and `GetSession` to round-trip the new fields
- Unit tests on the migration order

### Step 2 — Per-turn context injection (Channel B)
- Rename `injectCurrentTime` → `injectPerTurnContext`
- Compute pinned-memory diff against `prefix_hash` (cheap: compare the snapshot hash stored at generation start vs. current pinned set)
- Emit `[Memory updates since session start: …]` block only when non-empty and under budget
- Emit `[Active tasks: …]` from `tasks` table, only when non-empty
- If diff > 1k tokens, set `rotate_pending=1` and skip injection (rotation will rebake)
- **This alone solves the main stale-behavior symptom.**

### Step 3 — Proactive compaction
- Track running-total input tokens on the session row (already tracked for reactive path)
- At 60%, spawn background compaction; mark `compact_state='compacting'`
- Coordinate with user turn queue so writes don't collide
- On success, clear state; on failure, fall back to reactive 95% path

### Step 4 — Rotation engine
- Add `rotateSession(chatID, threadID)` in bridge
- Steps: summarize → pack relevant memories → reflect/consolidate → bump generation → DELETE claude_session_id → write `session_summaries` row
- On next user turn with `generation_started_at==now() and claude_session_id==""`: system prompt is rebuilt, first user message is wrapped with the carry-forward pack
- Wire hard triggers: identity file mtime, skills catalog hash watcher, manual CLI (`shell session rotate`)
- Wire soft triggers: 7-day age check in heartbeat tick, pinned-delta check in Channel B injector

### Step 5 — Heartbeat integration + observability
- Phantom system chat uses the same rotation rules (no special-casing)
- Heartbeat loop calls `maybeRotate(chatID)` before enriching
- Emit structured logs at rotation boundaries: generation number, summary length, memory-pack size, reason
- New CLI: `shell session inspect <chat-id>` — prints current generation, prefix hash, token fill %, last rotation, rotate-pending flag
- Dashboards later (Grafana?) from the log stream

Each step is independently shippable. Steps 1 + 2 are the minimum viable fix for the behavioral-drift bug.

## 7. Risks & open questions

- **Cache cost of rotation**: every rotation = one cold read. Budget: if rotations happen more than ~once per chat per week, we're over-rotating. Tune soft-trigger thresholds on measurement.
- **Summary quality**: if the summary is bad, carry-forward is worse than a clean restart. We can gate rotation on summary length/quality heuristics and fall back to "fresh with no carry-forward" if the summary looks degenerate.
- **Ghost relevance scoring drift**: the memory pack relies on ghost's relevance scoring. Umbreonmini has a separate memory DB from pikamini (known divergence). No blocker — each agent uses its own store — but worth a smoke test after shipping.
- **Concurrency**: proactive compaction must not race with a user turn. Use the existing process-manager lock; don't reinvent.
- **Rollback**: schema additions are nullable-safe. If the Channel B injector regresses, we can feature-flag it with a config knob (`sessions.per_turn_injection: true`).

## 8. Success criteria

1. A pinned memory added at time T is visible to the agent's next turn on any existing session within ≤ 1 turn (via Channel B) OR within the rotation window (≤ 7 days).
2. Daily log shows proactive compactions succeeding (no user-facing stall at 95%).
3. Behavioral learnings written by deep reflection reach mami's DM session without manual session kill.
4. Rotation carry-forward passes a "can you summarize what we were just doing" probe without hallucinating — tested in both DMs and group.
5. No regression on heartbeat noop rate (~42% baseline).
