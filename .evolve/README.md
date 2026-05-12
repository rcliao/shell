# Shell + Ghost Evolution Loop

Driver: Claude (Opus). Steerers: EV (papi), Jennifer (mami).
Cadence: ~1 hour, dynamic /loop.

## Goal
Evolve the shell harness, ghost memory, and per-agent behaviors based on actual usage —
transcripts, skill telemetry, ghost reflections — and capture every change as a git commit.

## Design rules (do not violate)
1. **Agent isolation by design.** Each agent owns its transcript + ghost memory + skills dir.
   Shell is shared workspace + tools, never a forced collision point.
2. **Two-tier approval.** Low-risk items auto-ship without approval. High-risk items require
   explicit approval (status flip from `proposed` → `approved`).
   - **Low-risk (auto-ship):** ghost puts (single agent ns, non-identity), per-agent skill drafts
     into staging dir, prompt-only edits on isolated branches with tests, README/docs, removing
     dead code with 0 invocations in 30+ days, additive reflect rules.
   - **High-risk (gated):** DB schema changes, data migrations / clamps / mass updates, deletions
     of ghost identity memories, force pushes, anything touching production secrets, breaking
     changes to public APIs or skill contracts, restart of running daemons.
3. **No destructive operations without approval.** No force pushes, no resets to remote, no
   deletions of ghost identity memories. Backups before any data migration.
4. **Reflect & consolidate.** Each cycle, before adding new findings, scan the backlog for
   duplicates / supersessions / stale items. Merge or close them. Backlog should shrink as
   often as it grows — quantity isn't the goal, quality and shippability is.
5. **Per-cycle observability.** Every cycle writes one `cycles/YYYY-MM-DD-N.md` report unless
   it's a lightweight idle cycle.
6. **End every shipping cycle with: build → restart shell → broadcast via `shell_relay`.**
   For observation-only cycles, broadcast a short status note.

## Per-cycle inputs (read live, never cached across cycles)

**Primary** (consume agent self-reflections — they already do the pattern detection):
- `agent:pikamini` memories tagged `behavioral`, `learning`, `convention`, or kind `procedural` since last cycle
- `agent:umbreonmini` same in `~/.shell/agents/umbreonmini/memory.db`
- Each one is a candidate finding — the loop's job is translating "the rule exists but isn't applied" into a concrete shell/ghost change the agent can't write itself.

**Secondary** (only when primary is dry):
- New transcript rows since high-water mark
- USAGE.jsonl from skill dirs (when populated)

**Repo state** (always):
- `git log --since=<last cycle>` on shell + ghost (note WIP, recent commits)

**Validation** (always check this first):
- Any items with `status: validating` past their measurement deadline → measure, mark `shipped`/`regressed`

## Per-cycle outputs (4 channels)
| Channel | Target | Mechanism |
|---|---|---|
| Code fix/feature | shell or ghost repo | branch `evolve/<cycle>-<slug>` → commit → user approval → merge |
| Per-agent skill | `~/.shell/agents/<n>/skills/<skill>/` | stage in `skill-drafts/<n>/<skill>/` → approve → install |
| Behavior rule | `agent:<n>` ghost memory | `ghost put` with `tag=learning,convention` |
| Personality nudge | `agent:<n>` ghost memory | `tag=identity`, pinned |

## Status flow in BACKLOG.md
**Low-risk path:** `proposed` → (loop ships next cycle) → `validating` → `shipped` | `regressed`
**High-risk path:** `proposed` → (mami/papi flips to `approved`) → `validating` → `shipped` | `regressed`
**Other terminals:** `rejected`, `superseded` (with link to the item that replaced it).

Every shipped item should carry a `predicted-effect:` and `measure-by:` line so the next cycle
can validate. `validating` items past their `measure-by` deadline get measured and graded;
items that miss their predicted delta become `regressed` and get an auto-revert proposal.

## Lightweight idle cycles
When the cycle runs and finds (a) zero approvals and (b) no significant new activity (<5 new transcript msgs since last cycle), do a **lightweight cycle**: scan approvals, update HWMs, broadcast nothing, skip cycles/*.md. This keeps idle costs minimal while preserving the loop heartbeat.

## Auto-stop after extended idle
After **4 consecutive lightweight cycles** with no broadcasts, send a short digest via shell-relay summarizing pending top-3 backlog items, then **omit ScheduleWakeup** to halt. The user resumes by invoking `/loop` again. Saves ~50% of idle tokens once mami/papi are away.

## Two-tier cadence (suggested when active)
- **Hourly** (currently the only mode): approval scan + lightweight delta check
- **Daily deep-investigation** (proposed): one heavyweight cycle per day rotates through angles. Most idle hours don't need investigation. Implement when ScheduleWakeup gains >1h support, or via CronCreate.

## Quiet hours alignment
Shell suppresses heartbeats 22:00–07:00 (local). The evolve loop should mirror — go fully silent during this window unless an approval is pending. Today's loop runs through the night unnecessarily.

## Per-cycle cost tracking (placeholder)
Each cycle should record `{tokens_in, tokens_out, cost_usd}` in its report. Source: session usage from the bridge or Claude API. Loop reads its own trend; auto-throttles when cumulative cost crosses a configurable budget. Not implemented yet.

## Approval ergonomics
- Surface a **top-3 recommendation** each cycle, not the full backlog.
- Every code-change proposal includes the diff (or SQL) inline so approval is single-click.
- Ship results include an **expected-metric-delta** line so the next cycle can validate.

## Files
- `LEARNINGS.md` — append-only chronological log of patterns observed
- `BACKLOG.md` — prioritized proposals with status
- `cycles/` — per-cycle reports
- `skill-drafts/` — staged per-agent skills awaiting approval
- `state.json` — high-water marks (last transcript msg id seen, last commit sha analyzed)
