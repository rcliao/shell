# Plan: first-class projects in shell

2026-08-13. Consolidates `RESEARCH-PROJECT-WORKSPACE.md` (2026-08-11, revised
08-12, project-home round 08-13) plus owner design decisions from review
(per-project git, single shared poller, lifecycle nudges, topic
consolidation, attribution transparency). Supersedes the phase sketch in the
research doc.

## Intent

Give shell a first-class **project**: a named unit of multi-week research
work (trip planning, housing search) that binds together a living document,
the conversation, scheduled autonomous research, and memory — so that

- the family user can **read and comment on a real doc** (Notion) instead of
  steering everything through chat,
- the agent never loses document identity across sessions (the doc-ID
  amnesia failure mode, observed live 2026-08-12: a 213s turn spending ~2
  minutes re-deriving a Notion database ID),
- both users can always **see what projects exist and which need them**,
- the implicit topic system keeps doing its job, now with a visible,
  correctable link to projects.

**Non-goals (v1):** multi-agent project ownership (one agent owns a
project; in the family group the non-owner agent simply has no binding and
behaves as today), Telegram-as-canvas (rich messages / Threaded Mode /
callback buttons — rejected in research), two-way Notion content sync
(comments + reconciled human edits only), Mini App dashboard (gated v2),
scheduler fire re-partitioning (`schedules.message_thread_id` in the
PartitionKey — group-topic delivery is solved on the send path instead, see
below).

**Group projects (in scope, per owner review).** A project may bind to the
family group: `chat_id` = the group, `message_thread_id` = a group forum
topic (human-created, e.g. a shopping-research-style topic) or 0 for the
general thread. Design constraints that keep this cheap:

- **Ownership stays single-agent**: the creating agent owns the project;
  only its DB has the row, so only it injects [Project] blocks, reacts with
  the project emoji, and writes the doc. The peer agent answers group
  messages as it does today (always-double is unchanged) but cannot
  double-write project state — agent isolation does the work for free.
- **Topic delivery without scheduler surgery**: inbound group-topic messages
  already route to per-(chat, thread) sessions, and `Transport` already
  carries a threadID on every send. Re-review F4 found the scheduler's
  delivery callbacks are chatID-only; rather than plumbing threadID through
  them, the owner's decision (2026-08-14) is to **extend the scheduler with
  non-chat schedules** (see "Event schedules" below): a project's research
  fire emits an event, the project consumer runs the turn and delivers the
  delta itself through `Transport` with the project's stored
  `message_thread_id`. Chat-bound notify/prompt schedules are untouched;
  fires keep their (chat, 0) serialization key.
- The pinned 📋 Projects message is per chat, so the group gets its own
  (pinned in the project's topic when one is set).

## Components

| Component | New/Changed | Role |
|---|---|---|
| `internal/store` `projects` (+ side tables) | NEW | registry: binding + lifecycle state |
| `workspace/projects/<slug>/` per-project git repo | NEW | canonical doc, commit = receipt |
| `internal/project` (renderer + poller + repair) | NEW | Notion render w/ block map; comment/edit ingestion; revert-reapply |
| `skills/project` | NEW | agent-facing verbs (bash over RPC) |
| `internal/bridge` `project_hook` | NEW | [Project] block injection via sticky-pointer binding |
| `internal/topic` | CHANGED | binding lookup, pinned override, correction source |
| Reaction sender | CHANGED | project emoji replaces 👀 when turn is bound |
| `internal/scheduler` | CHANGED | per-project research schedule; shared poll job |
| Heartbeat enrichment | CHANGED | lifecycle asks + needs-you digest line |
| Transport (`internal/bridge/transport.go`, telegram) | CHANGED | SendDocument, Pin/Unpin, edit-by-id; `type="document"` artifacts |
| Pinned 📋 Projects message | NEW | project home v1 (per chat) |
| CLI `shell project` | NEW | owner ops: list/show/archive/bind |

## Data flow (domain stories)

**Create.** The family user asks for help planning a trip → agent runs
`project create` → registry row + per-project `git init` + doc scaffolded
from template (goals/constraints/options/to-decide/log) and committed
(receipt) → Notion page created (icon = project emoji), block map persisted,
`export_ref` saved → research schedule registered (prompt mode,
`dedup_key=project:<slug>`) → current sticky topic bound
(`topic_thread_ref`) → pinned 📋 Projects message updated → 1-2 line reply
with the Notion link.

**Attributed turn.** Two routing modes (re-review F1 — the topic layer is
chat-scoped, so sticky attribution cannot safely span group forum topics):

- **Thread-bound (groups):** a group project bound to a forum topic routes
  deterministically — any turn in that (chat, thread) gets the project's
  block. No classifier involved, no override needed.
- **Topic-bound (DMs):** the sticky pointer resolves the topic; a bound
  topic selects the project. Pinned overrides and corrections apply to DM
  chats only in v1 (re-keying `conversations` by thread is future work if
  ever needed).

On attribution: **reaction = project emoji** (instead of 👀; the 15s ⏳
long-turn switch is unchanged) → `project_hook` injects the [Project] block
(size logged) → reply follows the disclosure tiers: silent on continuation;
one small tag on transition (`🏠 housing — …`); stated assumption + escape
hatch when ambiguous. Project emoji are constrained at `project create` to
Telegram's reaction-allowlist (~70 emoji; anything else silently no-ops as a
reaction — re-review F3), so the emoji works identically as reaction, list
row, and Notion icon. Doc writes commit with receipts; the pinned list's
row updates.

**Correction.** User says the attribution is wrong → agent re-binds, **pins
the sticky pointer** to the corrected project (override TTL until natural
drift), logs `source=human_correction` to `topic_decisions`, and if the
mis-attributed turn wrote to the wrong doc: `git revert` the receipt commit
there, re-apply to the right project, confirm in one line. Corrections feed
the attribution-hygiene metric.

**Event schedules (owner decision 2026-08-14, replaces re-review F4's
callback plumbing).** The scheduler gains a third mode beside
notify/prompt: **`event`** — a schedule not bound to chat delivery. On
fire it enqueues its payload as a task-queue row (kind from the payload,
idempotency via `DeriveIdempotencyKey` as elsewhere) and is done; no
NotifyFunc/PromptFunc involved. Consumers own everything downstream,
including any chat delivery. This makes the scheduler a cron front-end to
the queue and generalizes beyond projects: the Notion poll tick, future
cron monitors ("watch this database / date column / feed → event → agent
acts") are all event schedules. Chat-bound schedules keep working
unchanged.

**Autonomous research.** The project's weekly schedule is an event schedule
emitting `project.event {event: research.due, project}` → the project
consumer leases it (PartitionKey = the project's (chat, thread), so it
serializes against live turns) → ONE bounded research pass → doc edit +
commit → surgical block re-render in Notion → consumer delivers the ≤3-line
delta via `Transport` with the project's stored thread → row in pinned list
updated (`last_research_at`). No classification involved anywhere.

**Feedback via Notion — evented, per owner review.** The poller is a
*producer*, not a doer. Shared poll job (one job, iterates `status=active`,
~30 min): `last_edited_time` short-circuit → detect changes → emit
**normalized events** into the existing durable task queue
(`kind=project.event`, payload
`{event: notion.comment.created | notion.page.edited, project, discussion_id,
block_ids, ...}`). Idempotency key = kind-prefixed hash of
(event, source id, **comment id / `last_edited_time`**) via
`DeriveIdempotencyKey` — the queue dedupes forever including completed
tasks, so a bare source id would permanently drop the second comment in the
same thread (re-review F2). `ExpiresAt` generous (hours; revision work stays
worth doing late), PartitionKey = the project's (chat, thread) so revision
turns serialize against live chat turns instead of racing them.
A consumer leases events and runs the bounded revision turn (one pass per
comment thread) → edit canonical → commit → re-render → **reply inside the
discussion thread** → mark handled. Human resolving the comment in the UI is
the acknowledgment. Direct page edits become `notion.page.edited` events
whose consumer folds the change into canonical as an attributed human-edit
commit.

Why evented: (a) Notion webhooks later become a second *producer* of the
identical events — zero consumer changes; (b) the event shape generalizes:
future cron monitors (watch a database, a date column, an external feed) can
emit `project.event` rows that trigger agent action through the same
consumer path, which is the owner's intended trigger architecture; (c) the
queue's lease/idempotency semantics give restart-safe, exactly-effective
processing for free. This uses the generic task queue as designed — it does
not resurrect the retired `agent.task` kind (the PLAN-BUZZ fence).

**Promotion (implicit → explicit).** A recurring unbound topic with
accumulating `open_commitments` → heartbeat asks once whether to make it a
project → human yes → create + bind + seed doc from the topic's rolling
summary and commitments. Never automatic.

**Lifecycle.** `review_after` passed, or N days without
`last_human_activity_at` movement, or consecutive empty-diff research fires
→ heartbeat asks once (archive / pause). Archive: schedule disabled via
dedup_key, project leaves poll set and pinned list, Notion page + git
history remain as the record. Pause: same minus finality; any chat mention
revives.

## Data model

All additive. Per-agent shell.db. Indexes created after table rebuilds
(store.go:611 migration-order trap).

```
projects
  id, slug UNIQUE, title, emoji, status(active|paused|archived)
  chat_id, message_thread_id (0 for DMs)
  doc_path                      -- workspace/projects/<slug>/doc.md
  doc_rev                       -- last rendered commit
  export_kind, export_ref       -- notion page id (doc-ID amnesia fix)
  block_map JSON                -- section -> notion block id
  handled_discussions JSON      -- comment threads already processed
  instructions, notify_policy(quiet|announce), lang
  ghost_tag, schedule_dedup_key
  topic_thread_ref              -- nullable FK -> topic_threads (binding)
  review_after, last_research_at, last_human_activity_at
  created_at, updated_at

chat_pins            -- project home v1 (chat-level, resolves review c4)
  chat_id PK, projects_msg_id, updated_at

topic_decisions      += source value 'human_correction'
conversations        += pinned_override (slug), pinned_override_until
write_verifications  += project_id (nullable)
```

- **Git:** one repo per project (`git init` at create). Pathspec-only
  commits. Pre-write dirty check: uncommitted foreign changes are committed
  separately (attributed human edit) before any agent write — receipts are
  never forged over human edits.
- **Ghost:** no schema change; `project:<slug>` tag on inject/log for bound
  turns.
- **Deliberately unchanged:** sessions, shared tasks.db, topic classifier
  fast path (binding is one indexed read on the already-fetched thread row;
  no LLM anywhere on the turn path).

## Interfaces

**RPC** (Unix socket, beside `/schedule`): `POST /project`,
`GET /projects`, `GET /project/:slug`, `POST /project/:slug/doc` (write +
commit + render + delta), `POST /project/:slug/status`,
`POST /project/:slug/bind` (topic re-bind / correction),
`GET /project/:slug/export`.

**Skill `skills/project`** (hot tier): `create | list | get | doc-read |
doc-write | render | status | bind | export`. SKILL.md hard rules: no
"saved" claim without the printed commit hash; deltas ≤3 lines, never
re-dumps; match the user's language; structural rewrites propose-then-
confirm; one revision pass per feedback message/thread; disclosure tiers
(silent / transition tag / stated assumption).

**Telegram surface:** SendDocument, PinChatMessage/Unpin,
edit-message-by-persisted-id (debounced via `withFloodRetry`);
`parseArtifacts` accepts `type="document"`; `/projects` command renders the
list; reaction sender takes an emoji parameter. NOT added: rich messages,
forum-topic creation, callback queries. `schedules.message_thread_id` stays
out of scope (PartitionKey re-partitioning risk; Telegram threads are not a
project surface).

**CLI:** `shell project list | show <slug> | archive <slug> | bind <slug>`.
The family user gets zero commands — natural language only.

**Heartbeat:** active-projects block (slug, emoji, needs-you, staleness
signals) + at most one lifecycle/promotion ask per beat; digest line only
when something needs a human.

## Delivery phases

Each phase ships independently via `make build` + SIGHUP, PII gate before
every commit.

**P1 — Registry (weekend).** `projects` table + store methods + one
action-multiplexed `POST /project` RPC handler (house style — mirrors
`/queue`; no path-param routes) + `skills/project` (create/list/get) +
**chat-scoped [Project] registry block** with size logging + ghost tag +
`export_ref` populated for the two live doc bindings (health log, housing)
by hand. The P1 block lists ALL active projects for the chat (slug, emoji,
export_ref, doc_path) — per-project attributed blocks require binding and
arrive with P4 (re-review F5); the registry block alone kills doc-ID
archaeology.
*Verify:* unit tests on store/migrations; live: agent recalls a doc ID with
zero archaeology (compare against the 2026-08-12 213s baseline turn).

**P2 — Doc core + project home v1 (~1.5 wk).**
Per-project git + template + doc-write path (receipts, dirty check) +
**scheduler `event` mode** + project consumer (research.due handler with
consumer-side delta delivery) + delta discipline + SendDocument/Pin/edit in
transport (incl. a send-returning-message-id method — `Notify` returns
nothing today) + `type="document"` + pinned 📋 Projects message +
`/projects` + `shell project` CLI.
*Verify:* e2e with a synthetic project in a test chat: create → scheduled
fire → doc commit → delta + pinned row update; dirty-check test (hand-edit
then fire); transport methods against a test bot.

**P3 — Notion render + comment loop (~1-1.5 wk).** Page create + block-map
render + shared poller + comment revision turns + in-thread replies +
handled ledger + human-edit reconciliation + `last_human_activity_at`.
*Verify:* e2e: comment on a block → revision commit → block re-render →
in-thread reply; direct-edit reconciliation test; poller idempotence
(restart mid-cycle, no double-processing).

**P4 — Consolidation + attribution (~1 wk).** Topic binding at create +
`project_hook` routing + emoji reactions + disclosure tiers + correction
flow (pinned override, revert-reapply repair, `human_correction` ledger) +
promotion ask + lifecycle asks + attribution-hygiene report
(`shell attribution-hygiene`).
*Verify:* scripted classification scenarios (continuation / transition /
ambiguous / correction) against a test chat; revert-reapply test with two
projects; metric baseline captured.

**P5 — gated.** Mini App hub (precondition: stable HTTPS URL — Tailscale
Funnel; trigger: >3-4 concurrent active projects or in-Telegram doc reading
wanted). Comment webhooks via tunnel (trigger: 30-min poll latency
complaints). Google Docs adapter (trigger: Notion friction).

Migration of existing work: housing search and the trip planning notes
become the first two real projects during P2 (import current workspace
files as initial commits; bind existing Notion pages via `export_ref`).

## Decisions locked (from owner review, 08-12/13)

1. Per-project git repos, never a repo over the whole workspace.
2. Outbound Notion writes are push; inbound is ONE shared poll loop;
   webhooks are a later drop-in.
3. Heartbeat asks about end-of-life; archiving is never automatic.
4. Project home v1 = pinned message + digest; Mini App is gated v2.
5. Topics: link-and-promote, never merge; classification never load-bearing
   for project writes.
6. Attribution: emoji reactions + three disclosure tiers + one-message
   correction with git repair.
7. [Project] block: measure size first; no token cap yet.

## Owner sign-off (resolved 2026-08-13)

1. Default research cadence: **weekly**, per-project overridable. No budget
   cap for now (revisit if weekly turns prove expensive).
2. Promotion/lifecycle thresholds: proposals stand — promotion ask after a
   topic recurs ≥3 distinct days with ≥2 open commitments; staleness ask
   after 10 days without human activity.
3. Agent rollout: first agent only until P4 validates, then parity.
4. Sticky-override TTL on correction: until 6h idle or explicit topic
   change.
5. Group projects: **in scope** (see Intent section) — single-agent
   ownership, send-path topic delivery, no scheduler re-partitioning.
6. Notion inbound: **evented** through the durable task queue so webhooks
   and future cron monitors become producers of the same events.
7. (2026-08-14) Scheduler extended with a non-chat **`event` mode** instead
   of plumbing threadID through delivery callbacks — schedules can fire
   pure events; consumers own delivery. Generalizes to future cron
   monitoring triggers.
