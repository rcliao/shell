---
description: Autonomous evolve cycle — picks next action, executes safely, schedules next wakeup
allowed-tools: Bash, Read, Edit, Write, Grep, Glob, TaskCreate, TaskUpdate, TaskList, ScheduleWakeup
---

# /loop — autonomous evolve cycle

You are running ONE autonomous improvement cycle for the shell + ghost agent OS.
This command may be invoked manually OR auto-scheduled via ScheduleWakeup. Your
job is to pick the highest-leverage action, execute it safely, capture the
delta, then schedule the next wakeup (or halt if guard rails fire).

## Invocation

`$ARGUMENTS` may be:
- bare — autonomous mode, pick next action
- `dry-run` — print pick-next decision, don't execute
- `status` — print state.json summary + last 3 cycle outcomes
- `halt` — set state.halted=true (manual stop)
- `resume` — set state.halted=false (clear halt + reset failure counters)

## Step-by-step protocol

### 1. Decide

Run `./shell-bench pick-next` and parse the JSON. Honor `guard_rail_triggered`:
if true, STOP — do not execute, do not schedule next wakeup, write a halt note
to `.evolve/cycles/<today>-loop-halt.md` and exit.

### 2. Plan

Read state.json. Look at last 3 cycle reports in `.evolve/cycles/`. Form ONE
hypothesis for this cycle's action. Note:
- **action=drain-proposal** → read proposal content, decide accept/defer/reject,
  update its `status:` field in ghost.
- **action=close-goal-gap** → look at per-case failures in latest bench JSON;
  pick the worst-performing case and ship a fix targeted at IT specifically.
- **action=cv-regression** → diff latest CV report against previous; identify
  which probe regressed; ship a fix.
- **action=idle** → no work; bump `idle_streak`, write a 3-line cycle file
  saying "no actionable signal"; sleep.

### 3. Ship (one change, narrow scope)

Honor the .evolve/README design rules:
- ❌ NO git push, NO daemon restart, NO destructive ops without user approval
- ❌ NO touching live production memory.db without backup first
- ❌ NO new dependencies, NO schema changes
- ✅ Bench-layer fixes, convention memos, new test cases, doc updates,
  pure-function changes — auto-ship OK
- ✅ Always backup with `cp memory.db memory.db.bak-<date>-<cycle>` before mutation

If the ship requires anything in the ❌ list, downgrade to "propose-only":
file the action as a `proposal-loop-<ts>` memory in `loop:proposals` and
emit an idle cycle.

### 4. Measure

Re-run `make bench-pika` (or relevant subset) and compute the delta vs the bench
that pick-next read. Write a results table in the cycle file.

### 5. Record

Append a `.evolve/cycles/<today>-cycle-<N>.md` with:
- action chosen + reason
- hypothesis
- ship description (files touched + 1-line per file)
- bench delta table (before / after / pp delta)
- counter-explanations considered
- kill-switch (what would have proved it wrong)
- next-cycle suggestion

Update `.evolve/state.json`:
- `last_cycle`, `last_cycle_at`
- `bench_latest` for affected agents
- `current_goal.current` if the goal metric moved
- `track_record`: increment validated|wrong_lever|measurement_fix
- if idle: increment `idle_streak`, else reset to 0
- if failed: increment `consecutive_failures`, else reset to 0

### 6. Reflect

Look at the cycle just shipped. If you notice:
- a pattern across 3+ recent cycles → file a meta-finding in
  `.evolve/LEARNINGS.md`
- a behavior the agents could self-author next time → write a convention
  memo to the affected agent's ghost

### 7. Schedule next

If guard rails are clear (track_record looks healthy, idle_streak < 5, failures < 3):

```
ScheduleWakeup(delaySeconds=3600, prompt="/loop", reason="next autonomous cycle, hourly cadence")
```

If `idle_streak >= 3`, slow to 4 hours. If `idle_streak >= 5`, halt (set
state.halted=true, leave a note for the user, do not re-schedule).

If a ship failed mid-cycle, increment `consecutive_failures`. At 3, halt with
clear failure diagnostic in the cycle file.

## Anti-patterns (do not do)

- ❌ Shipping multiple changes before re-benching ("everything got better!" —
   which one moved the number?)
- ❌ Moving the target after the ship ("0.5 is fine actually")
- ❌ Restarting daemons or pushing to main from inside /loop — defer to user
- ❌ Editing skill prompts directly without a convention memo as anchor
- ❌ Mutating the live memory.db without `cp ... .bak-<date>-c<N>` first
- ❌ Calling /loop recursively in the same session — use ScheduleWakeup
- ❌ Skipping the cycle file — even idle cycles get a 3-line note

## Status line shape (always end with this)

```
[cycle <N>] action=<action> result=<validated|wrong_lever|measurement_fix|idle|failed>
bench: WH=<x> RF.flex=<x> CV.pass=<x>  goal=<dim> current=<x> target=<x> gap=<x>
next wakeup: <duration> (or HALTED: <reason>)
```

## Failure modes the loop is designed to survive

- DB drift between runs — note it in the cycle file, don't treat as regression
- Open proposals from agents — drain first, always
- Ghost search behavior change — CV will catch it; CV regression → fix before goal work
- Context window exhaustion in this command — write cycle file first, schedule
  next wakeup last so progress isn't lost

## Files this command touches

| File | Why |
|---|---|
| `.evolve/state.json`          | source of truth; updated each cycle |
| `.evolve/cycles/*.md`         | per-cycle report (write-once per run)|
| `.evolve/LEARNINGS.md`        | append-only meta-findings           |
| `testdata/bench/cases/**`     | new test cases, edits to expectations |
| `internal/bench/**`           | bench logic fixes only              |
| `cmd/shell-bench/**`          | new subcommands, never breaking changes |
| ghost: `loop:proposals` ns    | proposal state mutation             |
| ghost: agent ns `convention-*`| new conventions for agents to follow |

This command does NOT touch: `cmd/shell/`, `internal/bridge/`, `internal/daemon/`,
any live agent shell.db, or any `~/.shell/agents/*/` skills without explicit user
approval.

## Coupling

- `/loop` and `/goal` are complementary: `/loop` is autonomous, scans for any
  high-leverage action; `/goal` is steered, focused on one dimension.
- `/loop` can promote itself into `/goal` mode by setting `state.current_goal`
  when it detects a sustained gap.
- Agent `propose-backlog` skill feeds the queue; `/loop` drains it first.
