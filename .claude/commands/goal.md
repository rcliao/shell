---
description: Run a bench-driven evolve cycle targeting one capability dimension
allowed-tools: Bash, Read, Edit, Write, TaskCreate, TaskUpdate, TaskList
---

# /goal — bench-driven evolve cycle

You are running a focused improvement cycle against a measured capability dimension.
Unlike `/loop`, which scans for opportunities, `/goal` is **steered by a number**.

## Args

`$ARGUMENTS` should be one of:
- bare: read `.evolve/state.json` → `current_goal`; resume the active goal
- `<dim>=<target>[@<agent>]` (e.g. `RF.flexible_contains=0.85@pikamini`)
- `status` — show active goals and current bench numbers, no ship
- `done` — close the current goal, write LEARNINGS entry, archive cycle file

Dimensions (from `internal/bench/types.go`):
- `WH` — Write Hygiene (memo claim → ghost row landed correctly)
- **`RF.flexible_contains` — primary RF metric** (every meaningful gold token present in retrieved corpus)
- `RF.token_recall` — RF supporting metric (continuous fraction of gold tokens hit)
- `RF.contains` — RF strict secondary signal (only meaningful for single-token gold answers like "Chonky")
- `CV.pass_rate` / `CV.token_recall` / `CV.flexible_contains` — Conversation eval (synthetic sandbox)
- *(future: `SI`, `PD`, `CA`)*

Why `flex_contains` is primary: gold strings for list-shape facts ("eggs bread coffee")
can't pass strict substring `Contains` against any realistic memory layout. Pikamini
filed `proposal-pikamini-2026-05-13T15-41-38Z` to formalize this; accepted 2026-05-15.

When `CV` and `RF` diverge sharply, you have a data-quality vs infrastructure split:
- CV good + RF bad → real agent DB is polluted/noisy; cleanup or reranker tuning.
- CV bad + RF anything → memory schema / retrieval pipeline is broken at root.
- Both bad → infrastructure issue; fix CV first, then verify RF moves.

## Per-cycle protocol

1. **Read latest bench.** `.evolve/cycles/<latest-date>-bench-<agent>.json`. If absent or
   older than 24h, run `make bench-<agent>` first.
2. **Compute gap.** `gap = target - current`. If `gap <= 0`, the goal is met — write a
   LEARNINGS entry, mark `done`, stop.
3. **Hypothesize.** Look at per-case results. Which cases score 0? What's their tag?
   What single content/code/convention change would lift the worst-performing case to
   ≥0.5? State this as a hypothesis BEFORE coding.
4. **Counter-check.** Could the gap be a measurement artifact (tokenizer, window,
   schema)? If yes, fix the bench first — moving the number by changing the ruler is
   not progress.
5. **Ship.** One change at a time. Backup before any data mutation. Use
   `shell-bench migrate-*` subcommands for ghost-DB rewrites.
6. **Re-bench.** Same agent, same window. Capture before/after delta per metric.
7. **Document.** Append to `.evolve/cycles/<today>-goal-<dim>.md` with:
   - hypothesis (verbatim from step 3)
   - ship description + file paths
   - bench delta table
   - alternative explanations considered
   - kill-switch (what would have proven the hypothesis wrong)
8. **Update `.evolve/state.json`** — `current_goal`, `last_cycle_at`, `track_record`.
9. **No restart, no broadcast, no autopilot.** This is a foreground cycle. Surface
   delta to user; they decide whether to continue.

## Anti-patterns

- ❌ Shipping multiple changes before re-benching ("everything got better!" — which one?)
- ❌ Moving target after the ship ("0.75 is fine actually")
- ❌ Trusting score movement from a bench change without separately validating the
   data change still holds (the alias backfill + bigram tokenizer cycle on 2026-05-13
   conflated two ships — I lucked out, but next time isolate)
- ❌ Goals without `target` numbers — "make it better" is not a goal

## Example

```
/goal RF.flexible_contains=0.85@pikamini
```

Expected output: gap analysis → 1 hypothesis → 1 ship → before/after table → cycle
file written → state.json updated → ask user whether to continue toward the target
or pause to review.

## Coupling with `/loop`

`/loop` produces backlog items.  `/goal` consumes them, but only the ones whose
`target-dimension:` matches the active goal.  Items without that field stay in the
generic loop's queue.  Eventually expect 70% of cycles to be `/goal`-driven (focused)
and 30% `/loop`-driven (opportunity-scan).
