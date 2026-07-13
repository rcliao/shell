# Shell as an Agent Harness — Pillars, Ownership, Exit Conditions

*2026-07-13. Written after a week of incident-driven work (cycles 161-167+)
that clarified what this layer actually is.*

**Identity: shell is the accountable seam.** It owns boundaries, budgets,
triggers, and ledgers — the things that must be **correct** — and deliberately
owns no intelligence, no knowledge, no personality, and no capability content
— the things that must be **good**. Intelligence belongs to the model;
knowledge to the memory store (ghost); personality and household specifics to
the agent layer (`~/.shell`, private); capability content to skills.

**Ownership test.** Shell owns X only if:
1. X lives on a boundary only shell straddles (chat platform ↔ model process
   ↔ memory store ↔ disk), **or**
2. X is a promise that must be *mechanically verifiable* (reconciliation,
   invariant, golden).

Corollary: if a capability is judgment-based, it is not shell's. If the
substrate (the model CLI) will inevitably do it better, shell's version is
scaffolding with an exit condition, not architecture.

---

## Pillar 1 — Delivery & transport integrity *(irreducible core)*

**Owns:** message movement in both directions, once, intact, to the right
destination. Streaming edits, flood-control retry, chunking, outbound dedup,
send-gating (no internal text to users), reactions, inbound media archiving,
relay routing.
**Does not own:** what the message says.
**Instruments:** outbound_sends ledger, media ledger, daemon send logs.
**Eval:** delivery_complaints, dup suppressions, internal_text_leaks,
photo_expired_apologies, reply-truncation signals — reconciliation +
zero-tolerance invariants.
**Exit condition:** none. This is the product's floor.

## Pillar 2 — Concurrency & coordination *(plumbing, not a feature)*

**Owns:** per-chat turn serialization (parallel across topics), preemption of
system turns by human turns, busy-retry for synthetic callers, background
tickers (cleanup, prewarm), graceful restart.
**Does not own:** parallelism *inside* a model turn (substrate's business).
**Instruments:** queue reactions, busy-retry logs, H18 queue_ms (note: the
handler-level lock wait precedes the timer — known gap).
**Eval:** nudges_unanswered, dropped-turn invariants, a2a completion.
**Exit condition:** none; Go makes this cheap and it is boundary work.

## Pillar 3 — System prompt harness *(own the pipeline, never the words)*

**Owns:** composition (identity slot + pinned + skills catalog + lifecycle +
group guidance), token budgeting, prompt fingerprinting → rotation, per-turn
Channel B assembly, metering (`shell context` manifest).
**Does not own:** the content of identity, rules, or skills. When shell
hardcodes a sentence an agent should say, the layering has broken.
**Instruments:** `shell context` (live per-component manifest via RPC),
prompt fingerprint.
**Eval:** base_context_tokens trend, dead-ns pin count, pinned-budget
overflow events.
**Known failure mode:** unmetered growth (the 7/13 rotation-thrash incident)
and content stranded in namespaces the composer never reads.
**Exit condition:** none for composition; individual blocks may migrate as
the substrate's native context features evolve.

## Pillar 4 — Memory integration *(own the seam, never the store)*

**Owns:** when to inject, what to log, provenance tagging (chat/speaker on
every write), audience scoping contracts for public composers, write/recall
verification ledgers, pre-rotation flush (planned).
**Does not own:** retrieval scoring, storage schema, consolidation policy —
that is the memory store's domain (independent ghost roadmap). Policy like
"recall-shaped turn → issue a targeted query" is shell's; *ranking the
results* is not. Hold this line: seam-creep starts exactly there.
**Instruments:** write_verifications, recall_verifications ledgers.
**Eval:** recall_grounded, inject_irrelevant share, write_confabulation,
post-verified-correction rate.
**Exit condition:** none for the seam; the store is already externalized.

## Pillar 5 — Skills lifecycle *(provisional — substrate may absorb)*

**Owns:** loading and precedence, hot/lazy tiering with token budgets,
activation (reload → next-turn), mechanical lint (no permission escalation,
size caps), usage telemetry, auto-retire, the authoring loop machinery.
**Does not own:** skill content — agents and owners write skills.
**Instruments:** tool_uses ledger (usage truth), skills catalog in the
context manifest.
**Eval:** skill usage rates, failed skill-path calls, self-authored-skill
activation success.
**Exit condition — explicit:** the substrate has its own skills system and
is converging. Re-evaluate whenever it gains per-agent directories, tier
budgets, or usage telemetry; shed duplicated machinery rather than compete.
Do not build a registry.

## Pillar 6 — Execution profiles *(a table, not a layer)*

**Owns:** the mapping turn-type → {model, effort, ephemeral vs persistent,
timeout}. Today ~five rows (conversation, light heartbeat, deep heartbeat,
scheduler, one-shot keyword override).
**Deliberately not owned:** per-message complexity routing. Measured verdict
(cycle 160): cache-keyed resume makes per-turn model switching economically
hostile, and the routable-population here is ~zero. This pillar stays a
table; resist re-inflating it into a router.
**Eval:** cost per chat-week, heartbeat cost share, model-attribution
coverage.
**Exit condition:** revisit only if the substrate offers per-message model
selection without cache invalidation.

## Pillar 7 — Session lifecycle & context economy *(substrate compensation)*

**Owns:** rotation (triggers, summaries, carry-forward packs), compaction
policy, cache economics (prewarm after rotation, warm-generation guarantees),
retention of transcripts, generation bookkeeping.
**Honest framing:** most of this exists because the subprocess/resume model
has no native session management. It is the most sophisticated part of shell
and the most likely to dissolve if the substrate ships proper agent-session
infrastructure. Own it fully; document it as scaffolding.
**Instruments:** H18 phase timings (queue/ttft/total), rotation logs with
typed reasons and generation age, `shell context` totals.
**Eval:** generation_age_p50, latency_p95, turns_over_60s, cold-turn share.
**Known failure mode:** thresholds set against yesterday's context size
(7/13 rotation thrash: generations lived minutes). Validation must hard-error
when a threshold is below observed base cost.
**Exit condition:** substrate-provided session management with cache-aware
model/context handling.

## Pillar 8 — Observability & self-measurement *(the moat)*

**Owns:** every ledger (write, recall, tool, tier, outbound, media, timings),
the owner-fitness eval (`shell eval`), the context manifest, daemon logs,
per-signal liveness checks (dead telemetry = failure, not a pass), the
PII gate protecting the public repo, snapshot/trend machinery (planned,
OwnerEval v2).
**Why it is the moat:** no surveyed peer harness measures whether a deployed
agent is getting better or worse for its actual owners over time. This pillar
is also what keeps every other pillar's ownership verdict falsifiable — the
router rejection, the threshold fixes, and the eval's own design were all
data decisions.
**Eval:** it *is* the eval; measured reflexively via ledger_liveness and
verifier-class entropy.
**Exit condition:** none.

## Cross-cutting — Orchestration & proactivity *(own triggers, never content)*

Scheduler/cron, heartbeat cadence and budgets, a2a transport with hop caps
and human-yield, dedup windows, quiet hours, prewarm scheduling. Shell decides
*when an agent may act unprompted and how often*; the agent decides what is
worth saying (and [noop] is always acceptable). The duplicate-reminder class
proved deterministic trigger-layer guards beat content-layer memory rules.

---

## What shell must not grow

- Opinions about answer quality (model's job; the eval only *counts* owner
  reactions to it).
- Retrieval scoring or memory schema (ghost's job).
- Persona/household content in code or repo (agent layer; enforced by the
  PII gate).
- A skill registry/marketplace (supply-chain surface; first-party only).
- A per-message model router (measured dead end under this substrate).

## How this document stays honest

Each pillar names its instruments and eval dimensions. The evolve loop grades
ships by dimension movement (OwnerEval v2, `.evolve/designs/ownereval-v2.md`)
and this file's claims are falsifiable by those numbers. When an exit
condition triggers, shed the scaffolding — the pillar map should shrink over
time, not grow.
