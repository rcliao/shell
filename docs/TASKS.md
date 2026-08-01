# Durable task system

Status: proposal. Nothing here is built yet.

## Why

Shell already has three partial queues. They do not know about each other, and
that is the actual reliability problem — not the absence of a queue.

| Mechanism | Where | Partition key | Durable? | Covers |
|---|---|---|---|---|
| `chatLocks` + `coalesceQueues` + `activeTurns` | `internal/telegram/handler.go` | `(chat_id, thread_id)` | no | Telegram messages |
| `dispatcher` mailboxes | `internal/scheduler/dispatch.go` | `chat_id` | no | scheduler fires |
| `pending_turns` | `internal/store/pending_turns.go` | `(chat_id, telegram_msg_id)` | yes | Telegram messages |

Three consequences follow directly from that table.

**Different partition keys.** The handler serializes per `(chat, thread)`; the
scheduler serializes per `chat`. A heartbeat and a user message targeting the
same chat are ordered by two different mechanisms that cannot see each other.
They do not collide today only because heartbeats run on the system chat.

**Only one trigger is durable.** Scheduler fires, a2a hand-offs and task events
have no replay path. A restart mid-scheduler-turn leaves an `interrupted` row in
`job_runs` and nothing re-runs the work. Telegram messages survive; everything
else does not.

**Completion means different things in different places.** This is what caused
the 2026-08-01 08:41 incident. `turnWG` (the drain barrier) is released inside
`bridge.HandleMessageStreaming`, but `CompletePendingTurn` runs afterwards in
the Telegram handler. Drain declared idle in the same millisecond the session
exited, exec'd, and left an already-computed turn marked `done=0` — replayed 39
seconds later, and the family saw a disconnect.

A queue does not fix that race by existing. If the queue marks an item complete
when the bridge returns, the same gap reappears one layer up. The fix is
defining completion as *the human has the answer* and making the drain barrier
cover that whole span.

## Non-goals

- **An external broker.** Single node, one family, tens of messages a day.
  SQLite is the right substrate; Kafka/NATS/Redis would add an operational
  surface with no matching benefit.
- **Cross-agent queues.** Agent isolation is deliberate. Each agent keeps its
  own queue in its own DB.
- **Replacing coalescing or absorb.** V2-H44 coalescing and V2-H46
  absorb-into-active-turn are measured wins. They become queue operations, not
  casualties.
- **At-most-once delivery.** For a family assistant, a duplicated reminder is a
  nuisance and a dropped one is a failure. At-least-once with idempotent
  delivery is the correct trade.
- **Priorities.** Nothing here has contended enough to need them. Adding
  priorities before evidence invents a starvation problem we do not have.

## The name, and the system that had it

`tasks` is not a free name. A cross-agent delegation system already owns it:
`internal/transcript/taskstore.go`, the shared DB at `~/.shell/shared/tasks.db`,
the `shell-task` skill, and the scheduler's task-poll path.

It is not dead code that never worked. It ran 36 tasks between 2026-07-09 and
2026-07-15 — 31 completed, 4 failed, 1 canceled — with 135 recorded events. It
stopped because an agent diagnosed a design defect and migrated away from it.
That agent wrote the reason into its own final task:

> Superseded — round 18 TTL-expired on this same self-task mechanism (confirmed
> root cause: CreateTask hardcodes ttl_minutes=60, daemon sweeps every 1min via
> ExpireOverdueTasks, incompatible with 24h not-before gates). Migrated the
> watcher to shell-schedule instead.

A 60-minute hardcoded TTL swept by a 1-minute expiry loop cannot express "not
before tomorrow 09:00." The work was not lost, it was expired out from under
itself, twice.

So the old system is **absorbed, not deleted**. Its purpose — an agent assigns
work, the work durably gets done, a result comes back — is precisely what this
queue provides, and its fatal defect is fixed incidentally: leases replace TTL,
so an item is reclaimed when its *owner dies*, not when a fixed clock runs out.
A not-before time becomes a queued item with a visibility delay rather than a
race against a sweeper.

Concretely, `from_agent` / `to_agent` become fields on a task, and delegation is
one `source` among telegram / scheduler / a2a. The separate shared DB goes away
with Step 4, not before — deleting it earlier would remove delegation while its
replacement is still in shadow mode.

The per-agent `tasks` table in `shell.db` (the `/task add` heartbeat backlog) is
a different thing entirely and has 0 rows lifetime on both agents. That one is
genuinely unused and can be dropped outright.

## Model

One durable task queue per agent. A task is **generic work with a payload**, not
a chat message — chat is one `kind` among several, and the queue must not be
shaped around it. Getting this wrong produces a Telegram queue with extra steps,
which is the thing already built and not worth rebuilding.

```
telegram message ─┐                         ┌─ handler: chat turn ──→ reply sent
scheduler fire   ─┼─→ tasks ──→ worker ─────┼─ handler: agent prompt
a2a hand-off     ─┤       ▲                 ├─ handler: maintenance
agent-authored   ─┘       │                 └─ handler: ...
                          └── lease expiry / restart ──────────────────┘
```

Three fields carry the design:

- **`kind`** — what work this is. Selects the handler. `chat_turn`,
  `agent_prompt`, `a2a_delivery`, and whatever comes later. Adding a kind is
  registering a handler, not changing the schema.
- **`payload`** — JSON, handler-defined. A `chat_turn` payload carries chat id,
  thread id, sender and text; a maintenance task carries none of those. The
  queue never interprets it.
- **`partition_key`** — an opaque serialization domain. Tasks sharing a key run
  one at a time and in order; tasks with different keys run concurrently. An
  empty key means no constraint — run it whenever a worker is free.

Partitioning is the only concurrency concept, and it is deliberately not tied to
chat. Chat work sets `partition_key = "chat:<id>:<thread>"` because a chat has
one Claude subprocess and two turns would contend for it. Work with no shared
resource sets nothing and parallelizes freely. The scheduler's current
`chat_id`-keyed mailboxes and the handler's `(chat, thread)` mutex both become
the same mechanism expressed as a key.

### States

```
queued ──lease──→ leased ──deliver──→ done
   ▲                 │
   └── lease expiry ─┘        (attempts++, back to queued)
                     │
                     └──→ dead   (attempts exhausted, or permanently unprocessable)
```

`leased` is the state `pending_turns` lacks. Today an item is either recorded or
finished; there is no way to distinguish "being worked on right now by a live
process" from "abandoned by a process that died." That distinction is what makes
crash recovery principled instead of a 30-minute age heuristic.

### Schema

```sql
CREATE TABLE tasks (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  kind            TEXT NOT NULL,            -- selects the handler
  source          TEXT NOT NULL,            -- who enqueued: telegram|scheduler|a2a|agent
  idempotency_key TEXT NOT NULL,            -- see "Processing once"
  partition_key   TEXT NOT NULL DEFAULT '', -- serialization domain; '' = unconstrained
  payload         TEXT NOT NULL DEFAULT '{}',
  state           TEXT NOT NULL DEFAULT 'queued',
  attempts        INTEGER NOT NULL DEFAULT 0,
  max_attempts    INTEGER NOT NULL DEFAULT 3,
  not_before      DATETIME,                 -- visibility delay; NULL = immediately
  lease_owner     TEXT NOT NULL DEFAULT '', -- daemon boot id
  leased_until    DATETIME,
  enqueued_at     DATETIME NOT NULL,
  started_at      DATETIME,
  done_at         DATETIME,
  result          TEXT NOT NULL DEFAULT '', -- handler-defined; what the assigner reads
  last_error      TEXT NOT NULL DEFAULT ''
);

-- Enqueue idempotency. See "Processing once" below.
CREATE UNIQUE INDEX idx_tasks_idem ON tasks(idempotency_key);

-- The dequeue path: ready work, oldest first, one partition at a time.
CREATE INDEX idx_tasks_ready ON tasks(state, partition_key, not_before, id);
```

`not_before` is what the retired task system could not express. A visibility
delay is a property of the task, evaluated at dequeue, so "check this tomorrow
at 09:00" is a queued task nobody leases until then — rather than a live row
racing a 1-minute expiry sweeper it is guaranteed to lose.

`result` closes the assign/report loop: an agent that assigns work reads it back
when the task reaches `done`. That, plus `not_before`, is the whole of what the
old delegation system did, minus its defect.

Timestamps are written UTC in SQLite-parseable form — the DSN already sets
`_time_format=sqlite`, and the reason is documented in `internal/store/timeformat.go`.

### Processing once

"Exactly once" is not achievable and claiming it would be the most dangerous
sentence in this document. A process can die between doing the work and
recording that it did. What is achievable is **enqueue-once plus
effectively-once delivery**, and those are two different mechanisms that fail in
different ways.

**Enqueue idempotency — `idempotency_key`, required, unique.**

Every task carries one. The unique index means a second enqueue with the same
key is a no-op returning the existing task, not a duplicate. Callers supply it;
where there is a natural key, it is derived rather than invented:

| Source | Key |
|---|---|
| telegram | `telegram:<chat_id>:<msg_id>` |
| scheduler | `sched:<schedule_id>:<occurrence_utc>` |
| a2a | `a2a:<from_agent>:<peer_event_id>` |
| agent-authored | caller-supplied; hash of (kind, normalized payload, `not_before`) if omitted |

The key is opaque to the queue — it is a string, not a parsed structure. The
prefixes above are a readability convention for humans reading the table, and
nothing depends on their shape.

The occurrence timestamp — not the fire time — is what makes the scheduler key
correct: a job retried three times for the same 09:00 occurrence is one task,
while tomorrow's 09:00 is a different one. `ScheduleDedupKey` in
`internal/store/jobruns.go` already establishes this pattern for schedule
registration (sha256 over chat, type, expression, normalized message); this is
the same idea moved to work.

The agent-authored row is the case the old `(source, external_id)` natural key
could not serve. When an agent assigns itself "check whether X landed tomorrow
at 09:00" and its turn is retried, there is no external id to dedup on — which
is precisely how the prior task system produced duplicate watcher rounds. The
derived key includes `not_before`, so re-assigning the same work for a *later*
time is correctly a new task rather than a swallowed duplicate.

**Execution is at-least-once, on purpose.** A lease expires and the task is
reclaimed; if the original worker was merely slow rather than dead, the work
runs twice. Making that impossible requires distributed consensus we have no
reason to build for one process on one Mac mini. So the guarantee is placed
where it can actually be enforced: at the side effect.

**Delivery idempotency — reuse the send ledger, do not invent a second one.**

`RecordOutboundSend` / `SeenOutboundSince` (`internal/store/outbound.go`, wired
at `daemon.go:599`) already hash normalized outbound text per (chat, thread) and
suppress repeats within a window. That is the existing chokepoint where a
duplicated execution collapses into a single visible message, and it shipped as
V2-H3. A re-executed task that produces the same reply is therefore invisible to
the family even though it genuinely ran twice.

The gap to close is that the ledger currently keys on text alone. Passing the
task's `idempotency_key` through to the send path makes suppression exact rather
than heuristic — two genuinely different messages that happen to share text stay
distinct, and a true retry is caught even if the model phrased the reply
slightly differently. That is a small change to an already-proven mechanism.

**Non-idempotent side effects are the residue.** A task whose work is not a
message — creating a schedule, writing to Notion, sending mail — cannot be made
safe by the send ledger. Two mitigations, in order: prefer tools that are
themselves idempotent (`shell_schedule` create already is, via
`UpsertScheduleByKey`), and for the rest, record the side effect's own
identifier on the task before performing it so a reclaiming worker can detect
that it already happened. Anything that satisfies neither should not be run from
a reclaimable task at all, and that constraint belongs in review, not in code.

### Leases, not heartbeat-of-liveness

A worker claims an item by setting `state='leased'`, `lease_owner=<boot id>`,
`leased_until=now+timeout`, in one transaction that starts with the UPDATE.
(Write-first is deliberate: a read-then-write transaction returns SQLITE_BUSY
immediately rather than honoring `busy_timeout` — see `FinishJobRun` in
`internal/store/jobruns.go`, which lost a heartbeat's terminal row to exactly
that on 7/30.)

On startup the daemon reclaims any item whose `lease_owner` is not the current
boot id: that process is gone, so its leases cannot still be live. This replaces
`ListUnfinishedTurns(30 * time.Minute)`, whose age window is a guess that is
simultaneously too short for a slow turn and too long for a fast crash loop.

### Completion is the handler's terminal effect

`done` is set when the handler's effect is durable and externally visible — not
when the handler function returns. For `chat_turn` that means after the reply is
sent, which is exactly what the 08-01 incident got wrong: the drain barrier
released when the bridge returned, leaving a computed-but-undelivered turn to be
replayed in front of the family. For other kinds the handler defines its own
terminal point, but the rule is the same shape: everything before completion is
recoverable, everything after is a duplicate.

This is the single most important line in this document. Getting it wrong
reproduces the incident with more machinery.

### Drain

```
1. stop leasing new items          (the queue keeps accepting enqueues)
2. wait for leased items to reach done or lease expiry
3. exec
```

Enqueue stays open throughout, so a message arriving mid-drain is durably
recorded and picked up by the new process rather than depending on Telegram
redelivery. That is a real improvement over today: the current drain stops the
poller, which works only because Telegram happens to redeliver.

## What moves, and what does not

**Coalescing and absorb become queue operations.** Today `absorbQueued` mutates
in-memory `coalesceQueues`. As a queue operation it is: mark the absorbed items
`done` with a reference to the absorbing item, atomically, in the same
transaction that claims the absorber. Same behavior, durable, and survivable —
today an absorbed message is lost if the process dies before the absorbing turn
delivers.

**Overlap policy moves up from the scheduler.** `internal/scheduler/policy.go`
already defines skip / buffer_one / allow, and the reasoning generalizes: a
heartbeat that comes due while the previous one runs should skip; a reminder
should queue. Overlap becomes a property of the work item, evaluated at lease
time, and the scheduler stops owning a concept that is really about work.

**Per-chat serialization is preserved but expressed as a key.** Chat work sets
`partition_key = "chat:<id>:<thread>"`; the scheduler's `chat_id`-only mailboxes
become `"chat:<id>:0"`, which is what they already mean. Nothing else about the
queue knows what a chat is.

**The Claude subprocess lifecycle does not change here.** Graceful shutdown of
the CLI subprocess is a separate concern from queueing work, and conflating them
is how this design would grow past what one commit can validate.

## Sequence

Each step is independently valuable and independently revertable. Nothing later
is required for something earlier to be worth having.

**Step 0 — widen the drain barrier.** Move the barrier so it covers delivery and
completion, not just the bridge turn. No new tables. This alone closes the
observed incident, and it is worth doing whether or not the rest is ever built.
Measured by: zero replays following a clean restart.

**Step 1 — new `tasks` alongside `pending_turns`, Telegram only, shadow mode.**
Idempotency keys are populated and their uniqueness exercised from day one;
shadow mode is the cheapest place to discover a key that collides or is not
stable across a retry.
Enqueue and complete in the new table while the existing path stays
authoritative. Compare the two ledgers for a week. Any divergence is a bug in
the new path found for free.

**Step 2 — cut Telegram over.** The handler dequeues instead of executing on
arrival. `pending_turns` becomes a view or is dropped. Coalescing and absorb move
to queue operations here, with their existing tests as the regression gate.

**Step 3 — enqueue scheduler fires.** Heartbeats and cron jobs become work items.
The scheduler keeps deciding *when*; the queue owns *running it*. This is where
scheduled work finally gets replay, and where the dispatcher's mailboxes are
replaced by the shared partition workers.

**Step 4 — a2a and delegation.** The last two trigger types. This is where the
old shared task store is retired: its rows migrate in as tasks with
`source='a2a'`, `~/.shell/shared/tasks.db` is archived, and
`internal/transcript/taskstore.go` plus the `shell-task` skill are removed. The
60-minute TTL defect dies with it.

Steps 3 and 4 are where the "one infra for scheduling and messaging" property
actually arrives. Steps 0 through 2 are what earn the right to attempt them.

## Risks

**This is the path every family message takes.** A subtle bug here does not
degrade quality, it loses a reminder. That is why Step 1 is shadow mode and why
Step 0 ships alone first.

**Two ordering mechanisms during Steps 1–2.** The in-memory chat locks and the
queue coexist. The mitigation is that the queue is not authoritative until Step
2, so a disagreement is a logged discrepancy rather than a dropped message.

**Lease timeout tuning.** Too short and a slow turn gets duplicated; too long and
a crash stalls a chat. Observed turn durations run to ~8.5 minutes, so the
initial lease should be well above that (the scheduler's 20-minute job timeout is
the natural anchor) and every expiry should be logged loudly rather than silently
retried.

**Scope.** The honest failure mode of this document is building all of it at
once. Step 0 is a few lines. If only Step 0 ever ships, the incident that
prompted this is still fixed.
