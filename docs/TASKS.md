# Durable task system

Status: steps 0-2 shipped and validated in production; step 3 is the design below.

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

- **`kind`** — what work this is. Selects the handler, which is either ordinary
  Go code or a delegation to an agent turn (see "Handlers" below). `chat_turn`,
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
  expires_at      DATETIME,                 -- drop rather than run after this; NULL = never
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

`expires_at` is what keeps replay from becoming a liability. A reclaimed chat
turn is still worth running because the human is still waiting; a heartbeat
reclaimed hours after it was due would report on a world that has moved on. Past
its expiry a task is dropped with a recorded outcome rather than run late —
recovery that knows when to stop.

`result` closes the assign/report loop: an agent that assigns work reads it back
when the task reaches `done`. That, plus `not_before`, is the whole of what the
old delegation system did, minus its defect.

Timestamps are written UTC in SQLite-parseable form — the DSN already sets
`_time_format=sqlite`, and the reason is documented in `internal/store/timeformat.go`.

### Handlers: deterministic and agent-delegated

A handler is a Go function:

```go
type Handler func(ctx context.Context, t Task) (result string, err error)
```

Kinds map to handlers in a registry. Most are ordinary code — deliver a message,
run a maintenance sweep, call an API. But **one handler delegates to an agent**,
and that is the point of building this inside an agent harness rather than a
job runner:

```go
// agentHandler runs the task's payload as an agent turn. Non-deterministic by
// construction: the agent decides HOW, the queue guarantees THAT and ONCE.
func agentHandler(ctx context.Context, t Task) (string, error)
```

This split already exists and works. The scheduler's `mode` field is exactly it:
`notify` sends fixed text (deterministic), `prompt` runs a Claude turn
(`scheduler.go:536`). The queue generalizes that from schedules to all work.

The division of labor is worth stating explicitly, because it is what makes the
non-determinism affordable: **the queue owns delivery guarantees, the agent owns
judgment.** Leases, retries, idempotency, ordering and completion are the
queue's; deciding what to actually do is the agent's. An agent handler that
crashes, hangs or wanders is still bounded by lease expiry and `max_attempts`.

Agent handlers need three constraints that deterministic ones do not.

**Completion must be evidence, not assertion.** A Go handler returning `nil`
means it worked. An agent saying "done!" means nothing — this codebase already
learned that the expensive way, which is why `internal/bridge/write_verify.go`
exists: agents report saving things that were never saved, "pure confabulation,
because the agent often lacks, or fails to call, the write tool." So an agent
handler is complete only when the agent calls `task_complete(id, result)` — a
real tool call, observable in `tool_uses`. A turn that ends without one is
`failed`, not `done`, regardless of how confidently it narrates success. The
write-verify enforcement path is the working precedent: it already issues a
bounded correction turn when a claim lacks a corresponding write.

**Retries are not replays.** Re-running a deterministic handler reproduces the
same work. Re-running an agent handler produces *similar* work — possibly a
differently-worded message, possibly a second Notion row. The send ledger's text
hash will not catch a paraphrase, which is precisely why the idempotency key has
to reach the side effect rather than stopping at the queue. Kinds whose agent
turn performs non-idempotent effects should set `max_attempts = 1` and surface
the failure to a human instead of gambling.

**Authority is per-kind, not per-agent.** A task that can invoke any tool is a
task that can do anything wrong at 3am with nobody watching. Each kind declares
the tools its handler may use, and the agent turn is spawned with that set —
using the `disallowed_tools` plumbing already in `internal/process/args.go`. A
maintenance task has no business sending Telegram messages, and the enforcement
should be structural rather than a line in a prompt.

The honest trade: agent handlers cost a full turn (observed up to ~8.5 minutes
and real money) where a Go handler costs microseconds. Use one when the work
genuinely requires judgment. "Send this text at 9am" does not.

### Worker extensibility: three tiers, one of them agent-authored

The constraint that matters most for an agent harness: **an agent should be able
to add a worker without a Go change and a redeploy.** A queue whose capabilities
are fixed at compile time makes the agent a caller of infrastructure rather than
an extender of it.

**What the ecosystem does.** Surveyed while designing this:

- **Restate** assigns each *virtual object* a key and serializes all handler
  calls on that key, journalling each step so a crashed handler is re-invoked
  with completed steps skipped. That is the same idea as `partition_key`,
  arrived at independently — useful corroboration that keyed serialization is
  the right primitive rather than a workaround.
- **Temporal** separates workflow (deterministic, replayed) from activity
  (effectful, retried). Our deterministic-vs-agent handler split is the same
  seam, minus the replay machinery we have no reason to build.
- **Inngest / Trigger.dev / Restate** converge on event-in, step-function-out
  with flow control and observability in the platform rather than the handler.
- **AnythingLLM** is the most directly stealable: a `plugin.json` declares the
  entrypoint and its parameters, and `handler.js` exports a `runtime.handler`
  taking exactly those parameters. Metadata file plus executable, discovered
  from disk.
- **Formal Skill / FairyClaw** and **SkillOps** (both 2026 papers) generalize
  that: a skill is a capability plugin with JSON metadata and an action schema,
  resolved by the runtime before each model decision; SkillOps adds a typed
  *Skill Contract* so skills can be maintained as a library rather than
  accumulating as scripts.

**What shell already has.** The substrate for the agent-authored tier exists and
is in daily use: `SKILL.md` frontmatter (name, description, `allowed-tools`) plus
a `scripts/` directory, loaded from `~/.shell/skills/` and the per-agent skills
dir, hot-reloadable through the existing `SkillsReload` RPC — no restart. Agents
already have a scoped authoring perimeter (`daemon.go:191`: Write access limited
to the playground and their own skills dir, invocation through the run-skill
wrapper with usage logging).

So the registry resolves a `kind` through three tiers:

| Tier | Handler | Author | Determinism | Use when |
|---|---|---|---|---|
| 1 | Go function | us | deterministic | hot paths, privileged effects, delivery |
| 2 | skill subprocess | **agent or human** | deterministic-ish | new capability, no Go change |
| 3 | agent turn | — | non-deterministic | the work needs judgment |

**Tier 2 is the answer to the question.** A skill declares which task kinds it
handles in its frontmatter:

```yaml
name: plant-care
description: Check plant watering schedule and report what needs attention
task-kinds: [plant_check]
timeout: 60s
```

The worker invokes `scripts/<skill>` as a subprocess with the task JSON on
stdin, reads a JSON result from stdout, and treats exit status as the outcome.
That contract — typed in, typed out, exit code decides — is what buys back
determinism from a handler whose author was non-deterministic. It is
AnythingLLM's `plugin.json`/`handler.js` shape expressed in the file format this
repo already uses.

Three constraints on tier 2, learned from what is already known here:

- **Promotion is a gate, not friction.** Skill drafts under
  `.evolve/skill-drafts/` are inert until installed — a fact previously logged
  as a papercut. For agent-authored *workers* it is the safety property: a
  handler that will run unattended at 3am with nobody watching should be read by
  a human once before it can be leased. Authoring is free; promotion is
  reviewed.
- **Tool scope comes from the skill, enforced by the runtime.** `allowed-tools`
  already exists in frontmatter; a skill-backed handler spawns with that set and
  nothing more, via the `disallowed_tools` plumbing in `internal/process/args.go`.
- **Timeouts are declared, not assumed.** A subprocess handler that hangs holds
  its partition. `timeout` in frontmatter, bounded by the lease.

**What we are not taking.** Journalled step-level replay (Restate/Temporal's
core) is the expensive part of durable execution and buys little here: our tasks
are one agent turn or one subprocess, not twelve-step pipelines where losing step
nine is costly. Task-level retry with idempotency at the side effect is the right
granularity for this scale. If tasks ever grow internal steps worth resuming,
that is the moment to revisit — not before.

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

**Step 1 — storage layer, no wiring.** DONE. `tasks` table, enqueue/lease/
complete/fail/reclaim, tested including a contention soak against a copy of a
live DB (which found and fixed two SQLITE_BUSY losses before anything depended
on them).

**Step 2 — enqueue SCHEDULER fires.** DONE. Heartbeats, cron jobs and one-shots
became tasks. The scheduler keeps deciding *when*; the queue owns running it.

Proven in production on 2026-08-05 rather than only in tests. A fire was
triggered, leased, and the daemon was killed with SIGKILL mid-turn so the drain
barrier could not save it. The replacement process reclaimed the orphaned lease
within milliseconds of booting and re-ran the occurrence:

    job_run 90  interrupted  0ms       <- the killed fire
    job_run 91  fired_ok     6377ms    <- the replay
    task 3      done, attempts=2, "lease reclaimed: owner gone or lease expired"

Before this step only row 90 would exist. That is the whole change.

Two details that earned their keep. The lease owner is pid PLUS start timestamp:
the SIGHUP restart path execs in place and keeps the pid, so pid alone would
make a restart look like the same process and reclaim nothing. And workers drain
before their first poll tick rather than after, so the reclaim-to-replay gap is
milliseconds instead of a poll interval added to work that already lost time.

Overlap moved from the dispatcher's in-memory maps to a query against the table,
so it now survives a restart too. `durable_queue_disabled` falls back to the
dispatcher without a code change.

This step was originally sequenced last, behind Telegram, on the reasoning that
the earlier steps "earn the right" to attempt it. That reasoning optimized for
validating the queue rather than for fixing the largest hole, and the ledger
says the hole is here:

- Telegram has replayed **13 turns** across the two agents. That path is
  protected by `pending_turns` and works.
- Scheduled work has 3 recorded deaths (`interrupted`, `spawn_failed`,
  `turn_failed`) and **zero replays**, because no replay path exists. One of
  them is a deep reflection beat killed by a deploy on 2026-08-01 after 340s of
  work — the most expensive single unit of work in the system, discarded with no
  recovery.

Cutting Telegram over first would replace working machinery with new machinery.
Doing the scheduler first fills a gap that is actually open. Blast radius points
the same way: a replayed heartbeat is noop-suppressed and dedup'd by the send
ledger, where a replayed chat turn is a duplicate message to the family.

**Staleness is the design question this step must answer.** A reclaimed chat
turn is still worth running — the human is still waiting. A heartbeat reclaimed
three hours after it was due is not; it would report on a world that has moved
on. So scheduled tasks need an expiry distinct from their lease: past it, the
task is dropped with a recorded outcome rather than run late. Concretely, that
is an `expires_at` alongside `not_before`, defaulting to the schedule's own
interval — a beat is worth replaying within its own period and not after.

Deep beats are the exception worth tuning separately: 340s of work is worth
re-running even somewhat late, where a routine 8pm check-in is not.

**Step 3 — transport-agnostic intake.** Telegram stops being the message path
and becomes one PRODUCER of a generic `message.turn` kind. A TUI, Discord, or a
web client is then a second producer with no queue changes at all.

This is a bigger goal than "give Telegram replay protection", and it is the
right one: the thing that makes a message a unit of work is (who said it, in
which conversation, what it says). None of that is Telegram. What IS Telegram
is how the message arrived and how the reply gets rendered.

```
payload: {
  transport:   "telegram" | "tui" | ...,   -- names the delivery adapter
  external_id: "12366",                    -- the transport's own message id
  chat_id, thread_id,                      -- conversation address
  sender_id, sender_name,
  text,
  media: [...]                             -- transport-neutral references
}
```

Three decisions carry the design.

**The idempotency key is (transport, external_id)** — a NATURAL key, not a
content hash. Telegram redelivers updates on reconnect, and two identical
"ok" messages minutes apart are different work while the same message id is
always the same work. A content hash gets both of those backwards.

**The partition key must be (chat, thread), not chat.** Sessions are keyed by
both — the family group alone runs five subprocesses, one per topic — so
partitioning by chat serializes conversations that have no reason to wait for
each other. `FirePartitionKey` currently gets this wrong for scheduled fires
too; it is a latency bug there and would be a correctness-shaped bug here.

**Intake is transport-agnostic; DELIVERY is not, and pretending otherwise is
the trap.** Telegram streams a reply by editing a message in place; a TUI
writes to a terminal; a webhook posts once at the end. So the queue carries the
transport name and the worker resolves a delivery sink from it. The sink
interface is the narrow part worth getting right — roughly "start a reply",
"update it", "finish it", "attach media" — and Telegram's live-editing streamer
becomes its first implementation rather than the assumed one.

**Step 4 — cut over behind a kill switch.** Not shadow mode. The original plan
was to dual-write for a week and compare ledgers, on the theory that the queue
needed proving. It has since been proven where it counts: a fire was killed
mid-turn with SIGKILL and replayed by the next process. Dual-writing the
family's actual messages would mean two sources of truth on the one path where
a duplicate is a duplicate message to a human, so the safer shape is the one
already used twice — a config kill switch, with `pending_turns` kept
write-only for a few days as a cross-check before removal.

Blast radius is genuinely higher here than anywhere else in this document. A
replayed heartbeat is noop-suppressed and dedup'd. A replayed chat turn is a
message the family reads twice. The natural idempotency key is what makes that
impossible rather than unlikely.

**What shipped is narrower than the paragraph above originally promised, on
purpose.** This step said coalescing and absorb would "move to queue
operations". They did not. Execution stays exactly where it was: the handler
runs the turn inline, holding the per-topic lock, with the coalescing waiters
and the absorb path untouched. What moved is the LEDGER and the REPLAY.

The reason is that those are the weak parts and the rest is not. `pending_turns`
was text-only, had no attempt limit, no expiry, and a replay that ran once at
boot. The 600 lines around it are tuned against real traffic and carry a fix for
a race found on live messages the day before this shipped. Rewriting the tuned
half to reach the broken half would have been the expensive way round.

So a Telegram message is now a `message.turn` task created ALREADY LEASED by the
handler that will run it — durable, invisible to workers while its owner lives,
and reclaimed by the ordinary path when that owner dies. Boot owners carry a
start timestamp, so a SIGHUP exec that keeps the PID still counts as a new
owner. Moving coalescing into the queue stays available as a later step, and the
queue's partition keys already express it; it is no longer a prerequisite.

One hazard this introduces that the ledger did not have: a leased task holds its
partition, so a turn abandoned by a LIVE process would block that conversation's
scheduled work until the lease aged out. Handler paths that give up without
answering — a placeholder send that fails past its flood retries — therefore
requeue explicitly rather than returning silently.

**Agent-assigned work — RETIRED 2026-08-07, and worth recording why.**

An `agent.task` kind and a `shell_task` tool let an agent register durable work
for itself. Both are gone. Over seven days the two live agents called the tool
once between them, and that once was a smoke test — against a hundred
`ghost_put` calls in the same window.

Three attempts moved that number nowhere. Better tool descriptions took
`shell_schedule` from three calls to zero. Better instructions gave `shell_task`
zero from the start. Injecting queue state into the heartbeat, on the theory
that the gap was perception, changed nothing — and could not have: the digest
rendered only when work already existed, so with no tasks it was permanently
silent and unable to bootstrap itself.

What settled it was checking the OTHER task system. The legacy shared store had
no activity since 2026-07-15. Two independent systems, three weeks, near-zero
use. The agents were not failing to find the tool; they had nothing they wanted
to defer. A queue is worth having where work arrives whether or not anyone
chooses it — inbound messages, scheduled fires — and those are exactly the kinds
that stayed.

The reasoning is preserved because it is the kind of thing that gets rebuilt: a
self-assigned task queue is an obvious idea, and the obvious next move on
finding it unused is to promote it harder. That was tried three times.

**Step 5 — a2a and delegation.** The last trigger type. The old shared task
store is retired: its rows migrate in as tasks with `source='a2a'`,
`~/.shell/shared/tasks.db` is archived, and `internal/transcript/taskstore.go`
plus the `shell-task` skill are removed. The 60-minute TTL defect dies with it.
Note that this store is already dormant — nothing has written to it since
2026-07-15 — so this is now a cleanup, not a migration.

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

## Sources

Surveyed 2026-08-01 for the worker-interface design:

- [Temporal — durable execution](https://temporal.io/) — workflow/activity seam, event-history replay
- [Inngest — durable execution for AI agents](https://www.inngest.com/blog/durable-execution-key-to-harnessing-ai-agents) — event-in/step-out, flow control in the platform
- [Inngest vs Trigger.dev v3 vs Restate (2026)](https://www.pkgpulse.com/guides/inngest-vs-trigger-dev-v3-vs-restate-2026) — Restate's virtual-object keyed serialization and journalled handlers
- [Temporal vs Inngest (2026)](https://wetheflywheel.com/en/comparisons/temporal-vs-inngest/) — selection trade-offs
- [AnythingLLM — handler.js reference](https://docs.anythingllm.com/agent/custom/handler-js) and [plugin.json reference](https://docs.anythingllm.com/agent/custom/plugin-json) — the metadata-plus-entrypoint contract tier 2 copies
- [Formal Skill: Programmable Runtime Skills for LLM Agents](https://arxiv.org/html/2605.19604v1) — skills as capability plugins resolved by the runtime
- [SkillOps: Managing LLM Agent Skill Libraries](https://arxiv.org/html/2605.13716v1) — typed skill contracts, skill libraries as maintained ecosystems
- [SoK: Agentic Skills — Beyond Tool Use](https://arxiv.org/html/2602.20867v1) — survey framing
