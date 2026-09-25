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

**P3.5 — Adoption, doc budget, poll economy (from the 2026-09-18 review).**
A month of production data showed the plumbing works and the human surface is
unused. What the numbers said, and what each item does about it:

| Evidence (08-18 → 09-18) | Change |
|---|---|
| 1,419 comment sweeps, 0 comments ever. The Notion pages people actually open are hand-made shared pages that match no project's `export_ref`. | **Adopt** an existing page (watch-only) |
| One doc grew 0.9 KB → 101 KB, 25 appends, 0 compactions. The research prompt embeds the whole doc and demands the whole doc back, so research turns slowed until 5 of 7 timed out. Every "tidy this doc" request removed ~45% of the content. | **Doc budget**, enforced at the write path |
| Sweep time 34 s → 190 s: one `ListComments` per mapped block, per project, every 30 min, whether or not anyone has touched the project in weeks. | **Poll backoff** for quiet projects |
| Every research schedule registers at 09:00, next to the daily briefing; two share a chat and serialize. | **Stagger** research fire times |
| `review_after` is NULL on every row; nothing ever asked about the trip that ended. | Archived by hand now; the **staleness ask** stays in P4 (decision 3 stands: ask once, never automatic) |

*Adopt (watch-only).* `shell project adopt <slug> <notion-url>` binds a page a
human made. Data flow: owner runs adopt → CLI verifies the integration can
read the page (fails loudly if the page was never shared with it) → lists the
page's top-level blocks → stores `export_ref` and a block map flagged
`adopted: true`. From then on the poller lists comments on those blocks and
emits the same `notion.comment.created` events as for a rendered page; the
agent replies in-thread. What adoption must never do: the renderer and the
edit reconciler are **hard no-ops** for an adopted map. Both only speak
headings, bullets and paragraphs, and these pages hold tables and checkboxes
— a render pass would erase them. A page edit on an adopted project moves
`last_human_activity_at` and nothing else. The adopted map is refreshed from
the page on each full poll, since humans add and remove blocks freely.

*Doc budget.* A doc has a soft budget of 24 KB — one default for every
project until a real doc needs more. Two layers, prompt then mechanism:
1. The research prompt states the current size and the budget, and says:
   over budget → consolidate before adding. This makes the weekly research
   turn the maintenance job; no new schedule.
2. `doc-write` refuses an **agent** write that is over budget *and* larger
   than the doc it replaces, with an error that names the largest sections.
   A write that shrinks an over-budget doc is always accepted, so the way out
   is never blocked. Human edits are never refused. Git history is the
   archive — nothing is lost by cutting.

*Poll backoff.* New column `notion_polled_at`. A project with human activity
or a doc write in the last 7 days is polled every tick (30 min); a quiet one
every 6 h. Not gated on the page's `last_edited_time`: a new comment does not
reliably move it (see the poller's comment).

*Stagger.* Research schedules register at minute `10 + (project id mod 5) × 10`
of the 09:00 hour — :10 through :50, never :00, stable per project.

*Staleness ask — NOT built here.* It stays in P4 with the other lifecycle
asks (sign-off 2: 10 days without human activity, or `review_after` passed →
the heartbeat asks once). It changes what the heartbeat says to the family,
which deserves its own change. Until then archiving is an owner action:
`shell project archive <slug>`.

Out of scope: webhooks (still P5), section ownership between two agents on
one page, folding the weekly AI-briefing project into the daily briefing
schedule (undecided), a long-turn path for interactive "tidy the doc"
requests (the budget removes the need to ask).
*Verify:* unit tests per item; live: sweep log shows fewer projects and a
shorter duration; an oversize write is refused and a shrinking one lands;
an owner comment on an adopted page produces an in-thread reply; the next
Monday research run finishes inside its timeout.

**P3.6 — Focus: a thread per project, a coordinator, one "needs you" view
(owner-approved 2026-09-18).** Prompted by the redesigned Claude Code Projects
(2026-09-17: a coordinator that decides whether each request belongs in an
existing thread or a new one, shared project memory that keeps decisions, and
an overview of what needs the user). Shell has most of the parts; they are not
wired for focus. What a month of data said:

- Topic labels are categories, not workstreams: "family" 496 turns, "meals"
  498, and "travel" mixing three different trips. 609 of 633 topic threads
  have ≤2 turns — leftovers of a finer classifier, still in the table.
- The family group already uses Telegram forum topics, and thread-bound
  routing is in this plan (see *Attributed turn*) — nothing binds the two.
- The [Project] block lists every active project for the chat, never the one
  in play. Open commitments pile up unseen (27 and 21 in the top two topics).
- The doc budget worked on its first live run with no prompt help: a 101 KB
  doc was refused an append and rewritten to 10 KB 27 seconds later. The
  bloat was 99% update log; every other section was untouched.

Five units, each shippable alone, in this order:

1. **Log budget + decisions section (doc template).** The six headings stay
   (live docs and section hashes depend on them) and gain roles:
   目標/限制 protected; 現況/選項 current state, prunable; 待決定 = "needs
   you" (feeds unit 4); 更新紀錄 capped. New heading **決定** — dated one-line
   decisions, the project's shared memory, never cut by consolidation.
   `CheckBudget` gains a per-section cap for 更新紀錄 (8 KB — a daily-briefing
   doc logs ~1.2 KB a day, so this holds about a week) with the same
   asymmetry as the doc budget: over the cap AND larger than before →
   refused, naming the section; shrinking always accepted. Enforcement only —
   the refusal message proved sufficient, so no new prompt text.
2. **Forum topic per group project, scoped context.** `project create` in a
   forum-enabled group creates a Telegram topic named after the project and
   stores its id in `message_thread_id`; archive closes it, re-activate
   reopens it. A turn in that (chat, thread) gets a [Project] block for THAT
   project only — slug, doc path, export ref, 決定 and 待決定 — instead of
   the chat-wide list. Deterministic: no classifier. Needs the bot to be a
   group admin with *Manage Topics*; without it create still succeeds,
   unbound, and says so once. Groups only — DMs keep topic-bound routing
   (threaded DMs were rejected in research).
3. **Coordinator in the general thread.** A message in the general thread
   that matches an active project (project title/slug/ghost-tag terms — NOT
   the coarse topic label) gets that project's scoped block and a one-line
   tag on the reply pointing at the project's topic. Decision 5 stands:
   classification is never load-bearing for project WRITES — a match scopes
   context and tags the reply; it never writes a doc by itself. The promotion
   ask (sign-off 2: ≥3 distinct days, ≥2 open commitments) ships here.
4. **"Needs you" on the pinned list + one digest.** Each pinned project row
   shows its open 待決定 count; the heartbeat carries one digest line instead
   of scattered asks. Topic open commitments older than 14 days are asked
   about once, then dropped.
5. **Topic table hygiene.** Threads with ≤2 turns and no activity in 30 days
   are archived (soft, reversible), after a backup and an FK check. Last,
   because it is a production data operation and nothing else depends on it.

Out of scope: parallel worker threads (no work to split in a family
assistant; the release itself notes they exhaust usage limits faster),
per-section budgets beyond the log, topics in DMs.
*Verify:* unit tests per unit; live: a log-only append over the cap is
refused and a trimmed one lands; a project created in the group gets its own
topic and a turn there sees one project, not the list; a general-thread
message about the trip is tagged; the pinned row shows a needs-you count.

**P3.7 — Jev shadow router + progress voice (owner-approved 2026-09-22).**

*Why.* Shell's per-turn LLM classifier (Haiku) was removed in July: 8.5 s
p50 on the user path and a new orphan topic on 92–95% of calls. Keywords are
free and instant but cannot tell one trip from another. TypeSafe's Jev is a
decision model — state + typed questions in, a choice with calibrated
probabilities out, no prose — that the agent-harness ecosystem converged on
within a week of release for exactly the decisions Opus makes badly or
expensively: should the agent reply, which workstream is this, what to keep.
Smoke test 2026-09-22 on a synthetic Traditional Chinese message: correct
project at p=0.93 / confidence 0.9, open-question noul 0.96, three questions
answered in 531 ms for 547 input tokens.

*Shadow first, act later.* One request per user turn, fired asynchronously
after the turn's context is built, with a 5 s timeout, fail-open: it can
never slow or block a turn. Nothing acts on the answer. Every answer is a
row in `router_decisions` beside what actually happened, so the verdict is
computed, not argued:

| Question | Type | Ground truth | Pass |
|---|---|---|---|
| `which_project` — choice over active project slugs + `none` | Choice | messages in a bound forum thread (labelled by construction); 100 hand-labelled general-thread turns | accuracy ≥ 90% among answers above threshold; abstain ≤ 20% |
| `should_reply` — on peer-agent turns in the group | Choice {reply, noop} | what Opus did that turn (reply sent vs `[noop]`) | agreement ≥ 90%; count the Opus turns it would have skipped |
| `has_open_question`, `has_decision_v2` (v1 `has_decision` until 2026-09-25: fired on meal logs) | Noul | 待決定 / 決定 edits that followed | logged now, feeds unit 4b; no pass bar yet |

Plus p50 latency < 500 ms and tokens per call recorded, so cost is computable
when pricing appears. Thresholds 0.7 / 0.8 / 0.9 are all evaluated from the
same rows. Verdict after ≥ 5 days of data. Gating (scoped block in the
general thread; noop suppressing the Opus turn) is its own later change.

*Constraints.* Decision 5 stands: a router scopes context and tags replies;
it never writes a doc. Every user message goes to a third party with no
published retention terms — the owner accepted this for the experiment. The
key lives in the encrypted secret store (`shell-secrets set TYPESAFE_API_KEY`),
like the Notion and Telegram tokens — never in config, the DB or a log; an
error body that echoes it is scrubbed before it is recorded. Only real user
turns and peer relays are observed; heartbeats, prewarm and scheduler turns
have no ground truth and are skipped. Ghost (reflect, memory relationships) is out of scope here.

*Progress voice.* The placeholder already ticks every 2 s ("Running Bash...");
the wait is not silent, it is generic. Each agent gets a phrase file in its
own workspace it may rewrite in its own voice (the environment prompt tells
it so); the handler loads it with an mtime cache, validates it, and falls
back to the defaults. Tool names map to families (search / browse / memory /
file / shell) so a phrase can be about what is happening, never a raw
`mcp__ghost__ghost_put`.

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
