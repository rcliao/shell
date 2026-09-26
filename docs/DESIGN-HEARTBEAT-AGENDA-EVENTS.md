# Design: heartbeat agenda and external events (infrastructure)

2026-09-26. The owner asked for two things: "ideally we should make heartbeat
useful somehow", and "develop external event as infra so that when ready we
can do, for now not needed to get active". Evidence is at the end.

## Intent

1. **A heartbeat that has something to do.** Today a heartbeat asks the agent
   to "review recent activity". About 80% of beats end in `[noop]`, at roughly
   $28 per agent per week. The harness should hand each beat a concrete
   **agenda** built from real signals: the things that need the agent's
   judgment right now. An empty agenda means no turn at all (a skip, with the
   idle backoff applied). The agent still decides what to do with each item.
   The harness only stops calling it when there is nothing to decide.
2. **External events as infrastructure, not yet active.** Something that
   happens outside the chats (an email arrives, a calendar event moves, a
   watched page changes) becomes a normalized **event** that the next
   heartbeat's agenda carries. Producers stay outside shell: a script,
   typically run by cron, drops a JSON file into the agent's event spool. No
   producer is switched on now.

Out of scope:
- Real producers (Gmail, Calendar).
- Routing events to lanes (later: an event is a message to route).
- Unattended event workers (the owner's open question in BACKLOG idea 1).

## Components

| Component | Change |
|---|---|
| `internal/store/events.go` | `events` table: normalized, deduplicated by (source, dedup id), with a status lifecycle |
| `internal/event` | Spool ingestion: `<agent dir>/events/inbox/*.json` → events. Invalid files go to `rejected/`, ingested ones to `done/` |
| `internal/bridge/agenda.go` | `HeartbeatAgenda`: signals → items; render |
| daemon heartbeat callback | Builds the agenda. Empty and not deep: skip. Otherwise the agenda is appended to the beat |
| RPC `/event` + MCP `shell_event` | The agent lists events and marks them done or ignored |
| CLI `shell events` | `list`, and `inject` (test an event without a producer) |
| config `events.spool` | Ingest from the spool (default off) |

## Data flow

**A heartbeat fires.** The daemon builds the agenda:
1. **New conversation.** Real user messages in real chats since the last
   beat, counted per chat, for the agent to review for follow-ups or
   promises it made.
2. **Its schedules need repair.** Its own schedules that are auto-paused, or
   whose last fire failed, in the last 3 days.
3. **Tools keep failing.** A tool or skill that failed 3 or more times in 24
   hours.
4. **A project decision is waiting.** An active project whose doc has a
   non-empty 待決定 (to-decide) section and no human activity for 3 days.
5. **Pending tasks** assigned to the agent.
6. **Events.** Open events (`new`, or `seen` and not yet marked done or
   ignored) are listed, for up to 3 days.

The agenda is built read-only. The conversation watermark (a message id,
kept in `kv` so restarts do not shrink it) and the `seen` marks are
committed only after the beat's turn succeeds, or when the beat is
skipped. A failed turn loses nothing.

An empty agenda on a non-deep beat means no turn: the callback returns an
empty (no-op) reply, so the scheduler's idle interval applies. Deep beats
(reflection cadence) and check-in beats (every 4th beat carries the agent's
proactive "friendly check-in" hint) always run, with the agenda included.

**An event arrives (once a producer exists).** A producer writes (to
`<name>.tmp`, then renames it to `<name>.json`; files younger than 2 s are
left for the next pass)
`{"source": "gmail", "kind": "email.received", "dedup_id": "<message id>",
"summary": "one line", "ref": "<id or url>", "occurred_at": "RFC3339",
"chat_id": 0}` into the spool. The next agenda ingests it (dedup by source
and dedup id) and shows it. The agent acts, or marks it `done` / `ignored`
with `shell_event`. The file moves to `done/`.

## Data model

`events(id, source, kind, dedup_id, chat_id, summary, ref, occurred_at,
created_at, status new|seen|done|ignored, note, UNIQUE(source, dedup_id))`.
There is no content beyond a one-line summary and a reference: the agent
fetches detail itself.

## Interfaces

- `store.AddEvent`, `NewEvents`, `MarkEvents`, `ListEvents`.
- `event.IngestSpool(dir, store)`.
- `bridge.HeartbeatAgenda(ctx) Agenda`, with `Agenda.Empty()` and
  `Render()`.
- RPC `POST /event` (`list|done|ignore`), and MCP `shell_event`.
- CLI `shell events list|inject`.
- Config `events.spool` (bool).

## Plan and verification

1. Store and spool ingestion, with tests (dedup, rejects, moves).
2. The agenda and its gate, with tests (each signal; skip when empty; deep
   always runs).
3. RPC, MCP and CLI, with tests.
4. Review, deploy, verify:
   - the log shows `heartbeat: agenda empty, skipped` on quiet beats;
   - a beat with an agenda runs with the items;
   - `shell events inject` shows up in the next agenda.

## Evidence

- 7 days, 2026-09-19 → 26: heartbeats ended `[noop]` 55 of 66 times for one
  agent and 50 of 65 for the other. They cost $27.57 and $27.86.
- `enrichHeartbeatPrompt` sets `hasContent` true whenever a task store
  exists, so every beat runs a full turn.
- The scheduler already backs off to `scheduler.heartbeat_idle_interval`
  after a no-op beat (`isHeartbeatIdle`).
