# Model & Session Configuration — Architecture Assessment

> Companion to [ARCHITECTURE.md](ARCHITECTURE.md) and [SESSION-LIFECYCLE.md](SESSION-LIFECYCLE.md).
> `SESSION-LIFECYCLE.md` designed the two-channel context + rotation engine **before** per-task
> model routing existed. This doc covers what that addition broke and how to reconcile the three
> concerns — **model/effort selection**, **session lifecycle**, and **context assembly** — into a
> coherent architecture.

## TL;DR — the one root cause

Shell runs one long-lived `claude -p` subprocess per `(chat_id, thread_id)` and reuses it across
turns to keep the prompt cache warm. Three properties are **bound at the moment that subprocess is
spawned** and cannot change while it lives:

1. **model** (`--model`)
2. **reasoning effort** (`--effort`)
3. **system prompt** (`--append-system-prompt`, only sent on a fresh, non-`--resume` spawn)

But shell *decides* all three **per turn**, at the logical-session layer, on every message. The
logical `Session` (a SQLite row: generation, prefix_hash, model intent) and the physical
`persistentProc` (an OS subprocess with a frozen model + system prompt) are **two state machines
that are supposed to mirror each other and don't.** Per-turn intent is silently ignored by the
live subprocess until it dies (10-min idle) or a model mismatch accidentally forces a respawn.

Every defect below is a symptom of that single gap: **spawn-time-bound properties requested at
per-turn granularity, reconciled only by accident.**

## Symptom → cause map

| # | Symptom | Root mechanism | Evidence |
|---|---------|----------------|----------|
| S1 | **Deep heartbeat `effort=max` is not reliably applied.** For pikamini (`heartbeat_deep`==`heartbeat`==conversation model) it is *never* applied; for umbreonmini only when its proc happens to be alive on a different model. | `--effort` is emitted **only** on the ephemeral / per-message spawn path (`manager.go:245`), never in `spawnPersistent`. A deep heartbeat sets `Ephemeral:false` (`bridge.go:925`), so effort reaches the CLI only when a model mismatch incidentally forces `runClaudeBidirectional`. The comment at `bridge.go:900-903` explicitly relied on fable being a *distinct* model to force that fresh spawn — the fable revert silently broke it. | `bridge.go:904-927`, `manager.go:238-247`, `persistent.go:85-124` |
| S2 | **A running session can't change model; switching = cold prompt cache.** | Model frozen at `proc.model` (`persistent.go:197`); `getOrSpawn` errors on mismatch (`persistent.go:49-58`) → fallback spawns a *new* subprocess `--resume`-ing the same session UUID under a new model → provider cache is model-keyed, so the whole prefix re-processes as cache-creation. | `persistent.go:49-58`, `manager.go:205-244` |
| S3 | **Rotation doesn't take effect on an actively-chatting session.** A "new generation / fresh system prompt" clears logical fields but the *live* subprocess keeps its spawn-time system prompt and old CLI history until it idle-dies (10 min) or mismatches. | `rotateSession` wipes `HasHistory`/`ProviderSessionID` (`rotate.go:143-149`) but never kills the proc; system prompt only enters at spawn (`persistent.go:108`); next `Send` reuses the same live proc with the retained `p.sessionID`. | `rotate.go:113-162`, `persistent.go:49-62,220` |
| S4 | **Three rotation triggers collapse into one boolean; the cause is lost, and cost-rotation starves compaction.** `rotate_max_tokens` (excl. cache-read) vs `rotate_max_context_tokens` (incl. cache-read) vs pinned-delta overflow all set the same `rotate_pending`. With `rotate_max_tokens` 60k/90k always < `max_session_tokens` 200k, compaction is effectively dead. | `bridge.go:589-622`, `prompt.go:161-169`, `rotate.go:73-80` | 
| S5 | **Empty-model foot-gun.** An unset `model_routing` entry + empty top-level `model` → no `--model` flag → CLI silently picks its own default instead of erroring. | `config.go:171-200`, `persistent.go:99-105` |
| S6 | **Heartbeat vs conversation context diverge silently.** Same entrypoint, but shared-transcript + task-store blocks are gated out of heartbeats and re-implemented differently inside `enrichHeartbeatPrompt`; `SystemPromptWithBudget` (the intended smaller heartbeat budget) is dead code, so heartbeats pay the full 3000-token pinned system prompt; ghost `InjectContext` runs for heartbeats on `chatID=0` (near-inert). | `bridge.go:783-804`, `heartbeat.go:85-129`, `memory.go:110` (0 callers) |
| S7 | **Lifecycle knobs hardcoded despite per-agent budget differences.** `idleTimeout=10m`, `rotationMaxAge=7d`, `compactionSoftRatio=0.6`, `pinnedDeltaTokenBudget=1000` are global constants; the two agents have 60k vs 90k token budgets. | `persistent.go:38`, `rotate.go:19-35`, `bridge.go:573`, `prompt.go:91` |

## Target architecture — three decoupled layers + one reconciliation rule

```
┌─────────────────────────────────────────────────────────────┐
│ Layer 1  EXECUTION PROFILE   (pure fn of task_type)          │
│   task_type → { model, effort, persistence, ctxProfile }     │
│   e.g. conversation → {opus,  none, PERSISTENT, full}        │
│        heartbeat    → {sonnet,none, EPHEMERAL,  hb-light}    │
│        heartbeat_deep→{opus,  max,  EPHEMERAL,  hb-deep}     │
│        compaction   → {haiku, none, EPHEMERAL,  none}        │
│        fable(oneshot)→{fable, none, EPHEMERAL,  full}        │
└───────────────┬─────────────────────────────────────────────┘
                │ desired spec
┌───────────────▼─────────────────────────────────────────────┐
│ Layer 2  SESSION RUNTIME   (subprocess = source of truth)    │
│   reconcile(key, spec):                                      │
│     if spec.persistence == EPHEMERAL → fresh one-shot spawn  │
│         (honors model+effort, never touches persistent proc) │
│     else PERSISTENT:                                         │
│       if live proc's (model, genHash) == spec  → reuse (warm)│
│       else  kill + respawn  (deliberate cold-cache point)    │
│   rotation = the ONE sanctioned respawn: bump gen, then      │
│              reconcile() so the fresh system prompt is real. │
└───────────────┬─────────────────────────────────────────────┘
                │ builds prompt via
┌───────────────▼─────────────────────────────────────────────┐
│ Layer 3  CONTEXT COMPOSER   (one builder, profile-driven)    │
│   compose(ctxProfile) → { systemPrompt, perTurnBlocks }      │
│   heartbeat is a PROFILE (which blocks + which budgets),     │
│   not a divergent code path. Budgets are data, not if/else.  │
└─────────────────────────────────────────────────────────────┘
```

### The reconciliation rule (the crux)

> **The live subprocess is a projection of the desired spec. Any spawn-time-bound property that
> differs from the running proc forces a deliberate respawn — never a silent no-op.**

This single invariant fixes S1, S2, S3 at once:
- **S1** — deep heartbeat becomes an `EPHEMERAL` profile → always a fresh spawn → `--effort max`
  always emitted, on the chosen model, regardless of what else is running.
- **S3** — rotation calls `reconcile()`, which respawns the persistent proc → the new-generation
  system prompt actually loads instead of waiting for idle death.
- **S2** — model switching stays *possible* but becomes *explicit and rare*: only PERSISTENT
  conversation turns keep a stable model; the cold-cache cost is paid only at the sanctioned
  rotation boundary, not accidentally mid-conversation.

### Why "ephemeral for everything except conversation"

The warm persistent cache only pays off for a stable, back-to-back conversation. Heartbeats
(hourly, > idle timeout apart), compaction, topic classification, planner, write-verify, and fable
are all **stateless one-shots that carry their own context in the message** — they gain nothing
from `--resume` and today only muddy the persistent proc's model/effort. Make persistence a
declared profile property, default `EPHEMERAL`, opt into `PERSISTENT` only for `conversation`.

## Concrete decisions this settles

1. **Deep heartbeat → ephemeral profile.** Immediate: `Ephemeral: isDeepHeartbeat || fableTurn`
   at `bridge.go:925`. Validates the whole analysis (S1) with one line and near-zero risk — deep
   heartbeats already don't want the warm persistent cache.
2. **Rotation respawns.** `rotateSession` (or `maybeRotate`) calls `agent.Kill(key)` after
   bumping the generation, so the next `Send` spawns fresh with the rebuilt system prompt (S3).
3. **Typed rotation reason.** Replace the `rotate_pending` boolean with an enum
   (`cost | latency | pinned-overflow | age | day-boundary | manual`) recorded on the session, so
   logs say *why* and precedence is explicit (S4). Separate the **latency guard**
   (`rotate_max_context_tokens`, about turn speed) from **cost rotation** (`rotate_max_tokens`) as
   first-class, differently-owned triggers rather than two writes to one flag.
4. **Config validation at load.** Reject a config where a resolved model would be empty; warn when
   `rotate_max_tokens < max_session_tokens` (compaction unreachable) (S5, S4).
5. **One context composer.** Fold `enrichHeartbeatPrompt` into the same builder as the
   conversation path, parameterized by `ctxProfile`; wire the intended `SystemPromptWithBudget`
   so heartbeats stop paying the full 3000-token system prompt (S6).
6. **Promote hardcoded knobs** that differ by agent budget: `idle_timeout`, `pinned_delta_budget`,
   `compaction_soft_ratio` become per-agent config with the current constants as defaults (S7).

## Phased plan (lowest-risk first, each independently shippable)

| Phase | Change | Risk | Validates |
|-------|--------|------|-----------|
| **P0** | Deep heartbeat → `Ephemeral:true`. | ~1 line | S1; proves the spawn-binding thesis end-to-end (check `--effort max` on the fresh spawn) |
| **P1** | Rotation calls `Kill(key)` after `BumpGeneration`. | low | S3; new system prompt actually loads |
| **P2** | Introduce the `ExecutionProfile` struct as the single source for {model, effort, persistence}; route all `ResolveModel` callers through it. | med | S1/S2 unified, removes the effort/model entanglement |
| **P3** | Typed rotation reason + trigger precedence + config validation. | med | S4/S5, observability |
| **P4** | Context composer unification + `SystemPromptWithBudget` wiring + promote hardcoded knobs. | med-high | S6/S7 |

P0 and P1 are safe to ship now and directly restore the max-effort deep heartbeat the operator
asked for — **without** re-introducing fable (see the fable-heartbeat identity-pollution incident:
max effort was never the problem, the model's self-ranking disposition was).

## Invariants to preserve (don't regress)

- Persistent conversation turns keep a **stable model within a generation** → warm cache.
- Rotation is the **only** place a persistent conversation pays cold-cache cost.
- Ephemeral one-shots **never** mutate the persistent session's UUID/model (today only fable
  respects this — generalize it).
- Public-repo hygiene: this doc and any config carry no chat IDs or personal identifiers.
