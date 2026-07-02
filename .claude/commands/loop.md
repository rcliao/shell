---
description: Autonomous evolve cycle (v2) — reads production signals, ships one narrow improvement to the harness or agent-fit layer, schedules next wakeup
allowed-tools: Bash, Read, Edit, Write, Grep, Glob, TaskCreate, TaskUpdate, TaskList, ScheduleWakeup
---

# /loop — autonomous evolve cycle (v2)

You are running ONE improvement cycle for the shell + ghost ecosystem. Charter
lives in `.evolve/README.md` — read it first if this is your first cycle in
this session. Mission: shell/ghost become **generic agent infrastructure**
(stream H); the deployed agents stay **unique owner-fit products** (stream A).
Pick the highest-leverage action, execute safely, capture the delta, schedule
the next wakeup (or halt per guard rails).

## Invocation

`$ARGUMENTS`: bare = autonomous | `dry-run` = print decision only | `status` |
`halt` | `resume`.

## Protocol

### 0. Environment
Owner chat IDs never live in source (public repo, history scrubbed 7/1).
Derive at runtime:
```
export SHELL_BENCH_OWNER_CHATS=$(python3 -c "import json;print(','.join(str(u) for u in json.load(open('$HOME/.shell/agents/pikamini/config.json'))['telegram']['allowed_users']))")
```

### 1. Signals (cheap reads FIRST — ledgers and SQL before any transcript text)
For each agent config (`~/.shell/agents/{pikamini,umbreonmini}/config.json`):
- `./shell write-hygiene --since 72h --config <cfg>` and `recall-hygiene`, `tool-usage`
- topic health: `./shell-bench topic-stats` (or SQL on topic_decisions)
- true cost: `SELECT source, ROUND(SUM(cost_usd),2) FROM usage WHERE created_at >= datetime('now','-3 days') GROUP BY source`
- heartbeat noop rate; `media gate:` daemon-log lines; agent self-reflection
  memories (tags behavioral/learning/convention) since last cycle
- `./shell-bench pick-next` for bench state + open proposals

### 2. Validate before creating
Any BACKLOG item in `validating` past its `measure-by` → measure and grade
FIRST (`shipped` | `regressed` | `shipped-but-no-metric-effect`). A regression
gets an auto-revert proposal, not a shrug.

### 3. Decide
Priority order: (a) grade overdue validations, (b) approved backlog items
(🟢), (c) a NEW signal-driven finding (file it in BACKLOG — proposed unless
low-risk), (d) backlog hygiene (merge/close stale), (e) idle.
Form ONE hypothesis with a stated kill-switch.

### 4. Ship (one change, narrow scope)

**Stream H (shell/ghost Go code)** — gates, all mandatory:
- `go build ./... && go test ./...` green; bench non-regression on affected dims
- `make verify-no-pii` clean on the staged diff — HARD GATE; on failure
  sanitize or keep a local branch and flag for review
- commit to main and push allowed once gates pass
- ❌ NEVER restart daemons — append to `state.json.deploy_pending` instead
- ❌ no new deps, no schema changes without approval, no force-push

**Stream A (agent fit: skills/config/schedules/memories)**:
- every file change commits to the agent-layer repo at `~/.shell` (git)
- ❌ identity/persona edits and NEW outbound behavior (schedules/messages to
  owners) require approval — file as proposed
- ✅ skill instruction edits, additive conventions (ghost put, non-identity),
  reflect rules, config tuning within approved items
- live DB mutation: backup first (`cp <db> <db>.bak-<date>-c<N>`)

If the ship needs anything forbidden → downgrade to propose-only.

### 5. Measure
Re-run the relevant signal/bench and record before/after. If the effect needs
production time, set `measure-by` and leave the item `validating`.

### 6. Record
- Shipping or real-finding cycles: write `.evolve/cycles/<date>-cycle-<N>.md`
  (action, hypothesis, files touched, delta table, counter-explanations,
  kill-switch, next suggestion). Idle cycles: bump state.json only.
- Update `state.json`: last_cycle/at, track_record, idle_streak,
  consecutive_failures, deploy_pending.
- Meta-pattern across 3+ cycles → append `.evolve/LEARNINGS.md`.

### 7. Schedule next
Healthy → `ScheduleWakeup` per the cadence table in `.evolve/README.md`
(1h active / 2h idle-streak / 4h+ quiet hours 10pm-7am PT / 30-60min right
after a ship). `idle_streak >= 3` → halt WITHOUT wakeup: write a short digest
note in state.json.v2_note. The daily cron firing un-halts automatically when
new signals or approved items exist. `consecutive_failures >= 3` → halt with
diagnostic.

## Anti-patterns (v1 LEARNINGS — verbatim discipline)
- >1 unvalidated ship at once; ≥3 in flight → idle even if tempted
- shipping multiple changes before re-measuring ("which one moved the number?")
- moving the target after the ship
- cycle files for pure-idle observations
- restarting daemons or bypassing verify-no-pii from inside the loop
- recursive /loop in-session — use ScheduleWakeup

## Status line (end every cycle with this)
```
[cycle <N>] stream=<H|A|idle> action=<...> result=<validated|wrong_lever|measurement_fix|idle|failed|proposed>
signals: WH=<x>% recall_vol=<n> tool_fail=<x>% noop pika/umb=<x>/<x>% topic_single_turn=<x>%
deploy_pending=<n> next wakeup: <duration|HALTED: reason>
```
