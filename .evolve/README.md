# Shell + Ghost Evolution Loop — v2

Driver: Claude. Steerers: the owners (approval via BACKLOG status flips).

## Mission (v2, 2026-07-01)

**Shell/ghost become generic agent infrastructure** — a harness layer anyone
could build agents on — while **the deployed agents stay unique products**,
fit to their owners through day-to-day usage. The loop drives both from
production signals, not benchmarks alone (v1's failure mode: it optimized the
bench layer while production drifted, then stalled waiting on a manual
"resume" that never came).

Every cycle classifies its work into one of two streams:

### Stream H — harness (this repo + ghost)
Generic mechanisms: ledgers, guards, session lifecycle, topic threading,
memory retrieval, cost accounting. Test: *would an unrelated agent deployment
also want this?* If yes → stream H, and it must contain zero owner-specific
values (names, chat IDs, food/health domain rules).

Ship gates (ALL required before commit):
1. `go build ./... && go test ./...` green.
2. `make bench-all` no regression on affected dims (or explicit N/A note).
3. `make verify-no-pii` clean on the staged diff — **hard gate; both repos are
   public and history was scrubbed 2026-07-01.** Owner chat IDs come from
   runtime config, never source. If the gate fails: sanitize, or keep the work
   on a local branch and flag for owner review.
4. One narrow ship per cycle; ≤3 unvalidated ships in flight.

Push to origin main allowed once gates pass. **Daemon restart is NOT** — set
`deploy_pending` in state.json and note it in the cycle file; the owner or an
interactive session deploys.

### Stream A — agent fit (pika/umbreon uniqueness)
Skills, config, schedules, prompt fragments, pinned memories. Lives in the
agent-layer git repo at the shell config dir (local-only, allowlist-tracked
since 2026-07-01). This replaces v1's skill-drafts staging channel — edit
installed skills directly and commit to that repo.

Ship gates:
1. Every change committed to the agent-layer repo (tracked + revertible).
2. Identity/persona content: owner approval required.
3. New outbound behavior (schedules or messages to owners): owner approval
   required — propose, don't ship.
4. Prefer hot reload (skills-reload RPC); note when a restart is needed.

## Per-cycle inputs — production signals FIRST

Read cheaply (ledger CLIs + SQL) before touching any transcript content:

| Signal | Source |
|---|---|
| Write hygiene | `shell write-hygiene --since 72h --config <agent config>` |
| Recall grounding | `shell recall-hygiene --since 72h --config <agent config>` |
| Tool/skill usage + failure rates | `shell tool-usage --since 72h --config <agent config>` |
| True cost by source/chat | `SUM(cost_usd)` from each agent's usage table (per-exchange deltas since 7/1) |
| Heartbeat noop rate | recent heartbeat exchanges vs sends |
| Topic health | `shell-bench topic-stats` (single-turn ratio, is_new rate, classifier latency/errors) |
| Owner friction | transcript grep for correction markers (apologies, repeat-complaints, stop requests) |
| Media gate | daemon log `media gate:` lines (false-positive check before enforcement) |
| Agent self-reflections | ghost memories tagged `behavioral`/`learning`/`convention` since last cycle |
| Bench + proposals | `shell-bench pick-next` (export SHELL_BENCH_OWNER_CHATS from agent config allowed_users) |

**Validation first, always:** items in `validating` past their `measure-by`
deadline get measured and graded before any new work.

## Cadence — self-pacing rules (v1 cycle 115, still good)

| Window | Interval | Rationale |
|---|---|---|
| Active hours (7am-10pm PT) with traffic | 1h | validate ships within the hour |
| Active hours, idle streak ≥3 | 2h | reduce poll burn |
| Quiet hours (10pm-7am PT) | 4h+ / silent | heartbeats suppressed; mirror them |
| Just shipped, awaiting first signal | 30-60min | catch validation/rollback fast |

Liveness (v2 fix for the terminal stall): a **daily scheduled session** fires
`/loop` regardless. On firing, the loop un-halts itself if there are new
signals or approved items; otherwise it does one lightweight scan and exits.
Self-halt after 3 consecutive quiet cycles with a digest note.

**Anti-patterns** (v1 LEARNINGS, verbatim intent):
- No cycle files for pure-idle observations — bump state.json only.
- Never >1 unvalidated ship at a time; ≥3 in flight → idle even if tempted
  (cycles 73-74 orphan-daemon storm).
- No shipping multiple changes before re-measuring.
- No moving the target after the ship.

## Design rules (unchanged from v1 where still true)
1. **Agent isolation by design** — each agent owns its transcript, ghost ns,
   skills dir. Never unify.
2. **Two-tier approval.** Low-risk auto-ship: docs, additive tests, bench
   logic, pure-function fixes with tests, non-identity ghost puts, agent-layer
   skill edits within gates above. High-risk (owner flips `proposed` →
   `approved` in BACKLOG): schema changes, data migrations, identity edits,
   daemon restart, new outbound behavior, anything touching secrets.
3. **No destructive ops without approval.** Backup before any data migration
   (`cp <db> <db>.bak-<date>-c<N>`).
4. **Backlog hygiene each cycle**: merge dupes, close stale/superseded items.
   The backlog should shrink as often as it grows.
5. **Per-cycle observability**: shipping cycles get a `cycles/YYYY-MM-DD-N.md`
   report (action, hypothesis, delta table, kill-switch, next suggestion).

## Status flow in BACKLOG.md
`proposed` → (`approved` for high-risk) → `validating` → `shipped` | `regressed`
Terminals: `rejected`, `superseded`. Every ship carries `predicted-effect:`
and `measure-by:`; misses become `regressed` + auto-revert proposal.

## Files
- `BACKLOG.md` — prioritized items with status + stream tag (H/A)
- `state.json` — loop state: cycle counter, deploy_pending, track record, HWMs
- `cycles/` (gitignored — may quote production data) — per-cycle reports
- `LEARNINGS.md` — append-only meta-findings; read before changing the loop itself
- `.claude/commands/loop.md` — cycle protocol; `goal.md` — bench-steered mode

v1 history: 148 cycles (2026-05-07 → 05-24), 43 validated ships, built the
7-dim bench framework, proved per-turn LLM topic classification is a net loss
(cycle 148). skill-drafts/ is retired (agent-layer repo replaces it).
