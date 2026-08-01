# Work queue design

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

## Model

One durable queue per agent, partitioned by `(chat_id, thread_id)`, with every
trigger enqueuing into it.

```
telegram message ─┐
scheduler fire   ─┼─→ work_items ──→ per-partition worker ──→ bridge turn ──→ deliver ──→ done
a2a hand-off     ─┤        ▲                                                      │
task event       ─┘        └──────────────── lease expiry / restart ──────────────┘
```

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
CREATE TABLE work_items (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  source        TEXT    NOT NULL,           -- telegram | scheduler | a2a | task
  external_id   TEXT    NOT NULL DEFAULT '',-- telegram msg id, job_run id, ...
  chat_id       INTEGER NOT NULL,
  thread_id     INTEGER NOT NULL DEFAULT 0,
  sender_name   TEXT    NOT NULL DEFAULT '',
  payload       TEXT    NOT NULL,           -- the message/prompt text
  state         TEXT    NOT NULL DEFAULT 'queued',
  attempts      INTEGER NOT NULL DEFAULT 0,
  lease_owner   TEXT    NOT NULL DEFAULT '',-- daemon boot id
  leased_until  DATETIME,
  enqueued_at   DATETIME NOT NULL,
  started_at    DATETIME,
  done_at       DATETIME,
  last_error    TEXT    NOT NULL DEFAULT ''
);

-- Dedup: the same external event enqueued twice is one item. Replaces the
-- (chat_id, telegram_msg_id) primary key that does this for Telegram today.
CREATE UNIQUE INDEX idx_work_items_dedup
  ON work_items(source, external_id) WHERE external_id != '';

-- The dequeue path.
CREATE INDEX idx_work_items_ready ON work_items(state, chat_id, thread_id, id);
```

Timestamps are written UTC in SQLite-parseable form — the DSN already sets
`_time_format=sqlite`, and the reason is documented in `internal/store/timeformat.go`.

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

### Completion is delivery

`done` is set when the human has the answer — after the reply is sent, not when
the bridge returns. Everything upstream of that is recoverable; everything after
it is a duplicate. This is the single most important line in this document,
because getting it wrong reproduces the 08-01 incident with more machinery.

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

**Per-chat serialization is preserved but unified.** One partition key
`(chat_id, thread_id)` for everything. The scheduler's `chat_id`-only key
becomes `(chat_id, 0)` — the system chat's main thread — which is what it
already means in practice.

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

**Step 1 — `work_items` alongside `pending_turns`, Telegram only, shadow mode.**
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

**Step 4 — a2a and task events.** The last two trigger types, both currently
fire-and-forget goroutines.

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
