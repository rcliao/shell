# Research: first-class projects — a living doc bound to a Telegram conversation

2026-08-11. Multi-agent research pass (3 codebase readers, 2 web researchers,
3 independent design proposals, 2 judges, 1 synthesis).

**Revised 2026-08-12 after owner review — see "Revision after owner review"
below. The Telegram-as-canvas direction (rich messages, Threaded Mode,
callback buttons) is dropped; the human surface is a commentable external doc
(Notion first). The registry, git-backed project folder, and scheduler
integration stand.**

**2026-08-13: superseded for delivery purposes by
`PLAN-PROJECT-WORKSPACE.md`, which consolidates this research plus
owner-review decisions (per-project git, shared poller, lifecycle nudges,
topic link-and-promote, emoji attribution transparency).**

## Research Question

Today, multi-week research work for the family (trip planning, housing search)
lives in one of two places: an external Google/Notion doc the agent edits
through stateless skills, or an ad-hoc markdown file in the agent workspace.
Both are tracked *outside* Telegram, and nothing binds the work together.

Q1. What does shell already have that a "project" concept could build on, and
what is missing?

Q2. What does the current Telegram Bot API (10.2) make possible for an in-chat
living document, and what does prior art (Claude Tag, Claude Projects, Canvas,
PR-review loops) say about the right collaboration shape?

Q3. What should shell build?

## Summary

Shell has all the raw ingredients — thread-keyed sessions, topic_threads with
rolling summaries and open commitments, a prompt-mode scheduler, heartbeats,
ghost memory, a skills system with a read-back-receipt discipline — but they
are islands with no foreign keys between them. The #1 observed failure mode is
**doc-identity amnesia**: the notion/google skills deliberately store no doc
IDs ("this skill stores none"), so the binding between a project and its
document exists only in ghost memory and conversation, fragile across
rotations.

Meanwhile the platform moved under us: Bot API 9.3 (Dec 2025) enabled forum
topics in bot **private chats** (Threaded Mode), and 10.1 (June 2026) added
**Rich Messages** — real document blocks (headings, tables, collapsible
sections, anchors) sent once and edited in place indefinitely. A pinned,
continuously edited doc per project topic, entirely inside Telegram, is now
possible. No Telegram bot is known to do this yet.

**Recommendation: workspace-canonical, chat-projected.** The canonical doc is
markdown in the agent workspace (git-backed, commit hash = write receipt),
bound by a new `projects` entity in shell.db to a chat/thread, a schedule, and
a ghost tag. Telegram gets *projections*: a pinned summary card plus short
deltas now, a full rich-message pinned doc later (spike-gated). External docs
demote to optional one-way exports. Registry lands first — it is
weekend-shippable and kills doc-ID amnesia before any new Bot API surface.

## Findings

### [Q1] The ingredients exist; the binding doesn't

- **Workspace** is a freeform scratch dir created by the daemon
  (`daemon.go:454-462`) and advertised only as prose in the system prompt
  ("for multi-day projects, keep a notes file" — `prompt.go:57-62`). No
  schema, no index, no lifecycle, no versioning, no delivery path to chat.
  The real-world housing-search file is 79 lines of freeform markdown with no
  machine-readable link to anything.
- **External-doc skills** are stateless bash wrappers with a read-back-receipt
  contract. Doc IDs are stored nowhere by design (`skills/notion/SKILL.md:37`);
  every session must re-derive them from memory or conversation.
- **topic_threads** (`store.go:425-446`) is the closest existing primitive: a
  per-(chat, topic) rolling summary + open_commitments. But it is
  conversation-state only — no doc pointer, no schedule link, no user-facing
  surface, and `Commitment.DueAt` exists with nothing that fires it.
- **Sessions are already thread-keyed** — `UNIQUE(chat_id,
  message_thread_id)` (`store.go:176-184`), so a forum topic already gets its
  own session. The conversation leg of a project binding is free.
- **Scheduler prompt-mode already runs full autonomous agent turns**
  (`scheduler.go:571-581`) — a project can get periodic research turns with no
  new executor. But `schedules` has no `message_thread_id`; fires land in
  thread 0 (`storeadapter.go:113`), the known topic-routing gap.
- **The Buzz ADR already named this gap**: "a canvas per Telegram topic — a
  living document bound to a conversation" is the one capability shell lacks
  outright, to be built on shell's own doc machinery, not an external store
  (`ADR-BUZZ-ADOPTION.md:134-147`). This research executes that follow-up.

### [Q1] Telegram surface is deliberately narrow — and too narrow for this

`Transport` is Notify/SendPhoto/SendVideo only (`transport.go:9-18`); grep
finds zero uses of sendDocument, pinChatMessage, createForumTopic, or inline
keyboards anywhere. `AllowedUpdates` subscribes Message + MessageReaction only
(`bot.go:44-48`). Artifact markers accept image/video; `type="document"` is
silently dropped (`artifacts.go:48-68`). Consequences: a project doc can never
be delivered as a file, a summary can never be pinned, and there are no
buttons for approve/reject interactions. The topic *classifier*
(`topic_hook.go:145-225`) is keyed by chatID only and never sees the Telegram
thread ID — a separate concept from forum topics, with a confusing "ThreadID"
naming collision.

### [Q2] The platform now supports an in-chat living doc — with sharp edges

- **Rich Messages (10.1, June 2026)**: h1-h6, tables, nested lists with
  display-only checkboxes, collapsible `details`, code blocks, in-doc anchor
  links; sent via `sendRichMessage`, edited in place via `editMessageText` +
  `rich_message`. **Size cap undocumented — a spike must probe it.**
- **Bots can edit their own messages with no age limit** (the 48h window
  applies to business messages; `deleteMessage` is capped at 48h — so edit,
  never delete-and-resend).
- **Forum topics in bot DMs** (9.3, BotFather Threaded Mode): the agent DM can
  become per-project threads; bots can `createForumTopic` and pin per-topic.
- **Pinning is free and silent in DMs** (no admin rights); but bots cannot
  enumerate pins — shell must persist its own doc message IDs.
- **Dead ends**: native checklists are business-account-only; rich-message
  checkboxes fire no update when tapped (simulate with inline-keyboard
  callbacks + re-edit); `callback_data` caps at 64 bytes (token indirection
  required); ~1 msg/sec/chat rate limit (debounce doc edits); no native
  comment/suggestion model — replies anchor to whole messages, not spans.

### [Q2] Prior art converges on one durable pattern

Claude Tag makes the conversation container (Slack channel) the project
boundary, with channel-scoped memory, async staged work posted into threads,
and self-scheduled follow-ups. Claude Projects bundles knowledge + standing
instructions + per-project memory. ChatGPT Canvas's split-pane UX was quietly
deprecated — the durable pattern is **a persistent agent-owned artifact with
surgical edits and an approval loop**, as in PR review (propose, don't edit;
one fix pass per feedback message). A CHI-style study of agents in
collaborative editors found users prefer propose-then-approve and manual
triggers over silent autonomous edits. Telegram-native prior art is empty:
no known bot maintains a first-class living document. This would be genuinely
novel, using primitives that shipped in the last 8 months.

### [Q3] Three designs were proposed and judged

| Proposal | Core bet | User-lens | Eng-lens |
|---|---|---|---|
| A. Canvas-in-chat | Pinned rich-message doc per topic, Telegram-maximalist | **8** | 6.5 |
| B. Workspace-canonical | Git-backed workspace doc + shell.db registry; chat gets rendered projections | 4.5 | **8.5** |
| C. Sync-bridge | Keep Notion/GDoc human-facing; shell tracks registry + sync loop | 7 | 7 |

The judges split — but A and B share the same canonical store and differ only
in the Telegram projection, so the choice collapses to delivery sequencing.
The engineering judge is right that an undocumented rich-message size cap must
not be load-bearing for the data model, and that the local-file hot path (no
network on the turn path, where every past latency regression surfaced) is the
correct skeleton. The user judge is right that a ≤1000-char summary card +
`.md` attachment is **not an acceptable end state** for a non-technical
Telegram-only user — it is the degraded mode. So: build B's skeleton, run A's
render spike early, and treat the rich-message pinned doc as the *committed*
Phase 3 upgrade unless the spike fails.

C is rejected as the spine — a polling sync loop is a solo-maintainer sink
(lossy diffs, auth expiry gating research turns) and preserves the
leave-Telegram problem — but three of its ideas graft in whole:
registry-first sequencing, `instructions` + `notify_policy` columns
(Claude-Projects-style standing preferences, quiet by default), and one-way
structured export to Notion for comparison-shaped projects only (housing and
flights want sortable tables; rich-message tables are display-only).

## Recommended Architecture

### Data flow — trip-planning domain story

**Create.** The family user DMs the agent: "幫我規劃一趟旅行" (plan a trip for
me). Guided by the new `project` skill (replacing the "keep a notes file"
prose), the agent runs `project create --title <title> --chat $SHELL_CHAT_ID
--thread $SHELL_THREAD_ID` → RPC `POST /project`. The daemon inserts a
`projects` row, scaffolds `workspace/projects/<slug>/doc.md` from a template
(goals / constraints / current state / options / to-decide / change log), and
git-commits — **the printed commit hash is the write receipt**, same contract
as the notion/google skills. The agent seeds the doc from conversation + ghost
recall, tags all memory writes `project:<slug>`, renders a pinned summary card
into the thread (`pinChatMessage` — free and silent in DMs), and
self-registers a prompt-mode schedule (`dedup_key=project:<slug>`, message:
"read the project doc, do ONE bounded research pass, update the doc, post a
short delta"). Reply to the user: 1-2 lines, in their language.

**Autonomous research.** The schedule fires next morning. With
`schedules.message_thread_id` now plumbed, the fire lands in the project
thread's own session. The bridge's new `project_hook` — keyed by
(chat_id, thread_id), which topic_hook today is not — injects a **[Project]
block**: doc summary, status, standing `instructions`, open questions, and
deterministic ghost retrieval by tag. The agent researches (shell-search /
browser), edits the doc, commits (receipt logged to `write_verifications` with
`project_id`), edits the pinned card in place, and posts a **≤3-line delta**
in the user's language — never a full re-dump.

**Feedback days later.** The user replies in the thread, or replies to a
specific delta message; `message_thread_id` routes to the project session
(existing plumbing), and a reply-to on a delta resolves via `project_messages`
for coarse section anchoring. Small factual edits apply immediately → commit →
delta. Structural rewrites propose-then-confirm (Phase 4: inline ✅/❌
buttons; until then, typed confirmation) — one revision pass per feedback
message.

**Owner review.** `shell project show <slug>` on the CLI (doc path, git log,
schedule state, last runs), or `/project export` in chat → `sendDocument` of
the full markdown. A hand-edit to `doc.md` over SSH shows up as a commit or
dirty tree with no matching receipt → classified as a human edit → surfaced
per `notify_policy`.

**Lifecycle.** "This is settled" → `project archive`: schedule disabled by
dedup_key, `status=archived`, pinned card stays as the record. A deleted pin
is detected on failed edit → re-send + re-pin from the canonical doc
(canonical-store-wins, always).

**Phase-3 upgrade (spike-gated).** The summary card becomes a pinned **rich
message** rendering the whole doc (headings, tables, collapsible archived
sections, anchor-link TOC), edited in place. Optionally, Threaded Mode makes
topic = project in the agent DM. Nothing in the data model changes — only the
renderer.

### Data model (all additive; per-agent shell.db; indexes AFTER table rebuilds
per the `store.go:611` migration-order trap)

- **NEW `projects`**: `id, slug UNIQUE, title, status(active|paused|archived),
  chat_id, message_thread_id, doc_path, pinned_msg_id, doc_rev, instructions,
  notify_policy(quiet|announce), ghost_tag, schedule_dedup_key,
  topic_thread_ref (nullable FK → topic_threads), lang, export_kind,
  export_ref, last_research_at, created_at, updated_at`.
- **NEW `project_messages`**: `project_id, tg_message_id, chat_id, thread_id,
  kind(pin|delta|snapshot|export), created_at` — reply routing, peer of
  `message_map`.
- **ALTER `schedules` += `message_thread_id`** (default 0), plumbed
  ScheduleEntry → fire payload → onPrompt/onNotify. Independently valuable;
  closes the known thread-blind-schedules bug.
- **ALTER `write_verifications` += `project_id`** (nullable) — human-edit
  classification.
- **Doc layer**: `workspace/` becomes a git repo (`git init` beside the
  existing MkdirAll). Git log = revision history; commit hash = receipt.
- **Phase 4**: `project_proposals` (patch, summary, callback_token,
  status pending|applied|rejected) — 64-byte callback-token indirection.
- **Ghost**: no schema change — `project:<slug>` tag on InjectContext /
  LogExchange for project-bound turns.
- **Deliberately unchanged**: sessions (wrong anchor — they rotate by
  generation), shared tasks.db (agent isolation is intentional; a project is
  owned by exactly one agent).

### Interfaces

- **Transport** — Phase 2: `SendDocument`, `PinChatMessage`/`Unpin`,
  edit-by-persisted-message-id (debounced, via existing `withFloodRetry`);
  `parseArtifacts` accepts `type="document"`; relay gains `document_path`.
  Phase 3 (spike-gated): `SendRichMessage` + edit-with-`rich_message`,
  `CreateForumTopic`/`CloseForumTopic`. Phase 4: `AllowedUpdates +=
  callback_query`, `answerCallbackQuery` handler.
- **RPC**: `POST /project`, `GET /projects`, `GET /project/:slug`,
  `POST /project/:slug/doc` (write + commit + render + delta),
  `POST /project/:slug/status`, `GET /project/:slug/export`.
- **Skill** `skills/project` (hot tier, bash-over-RPC, shell-schedule shape):
  `create|list|get|doc-read|doc-write|render|status|export`. SKILL.md hard
  rules: never claim "saved" without the printed commit hash; deltas ≤3
  lines; match the user's language; structural rewrites propose-then-confirm;
  one revision pass per feedback message.
- **Bridge**: `project_hook.go` (peer of topic_hook, keyed by chat+thread)
  rendering the [Project] block; heartbeat enrichment gains an
  active-projects block (slug, last_research_at, open questions) so stalled
  projects get nudged.
- **CLI**: `shell project list|show|archive` for the owner. **The family user
  gets zero commands** — she speaks naturally; the agent drives the skill.

## Phased Delivery

1. **Registry (weekend).** `projects` table + store methods, RPC
   create/get/list, `skills/project` (create/list/get), [Project] prompt
   block, ghost tag wiring, `schedules.message_thread_id` end-to-end. Ships
   standalone: doc IDs never lost again; scheduled fires land in the right
   thread. No new Bot API surface.
2. **Spike (1-2 days, parallel).** Test bot probing: rich-message size cap,
   edit behavior with `details` blocks, Go library support for 10.1/9.3,
   Threaded Mode DM UX. Output: go/no-go evidence for Phase 3.
3. **Core loop (~1-2 weeks).** git-init workspace, doc template, per-project
   research schedule, pinned summary card + delta discipline,
   SendDocument/Pin, `type="document"` artifacts, `project_messages` reply
   routing, heartbeat block, `shell project` CLI, human-edit detection.
   **The trip scenario works end-to-end here.**
4. **Render upgrade (spike-gated, ~1 week).** Rich-message pinned doc; optional
   DM forum topics; pin-loss recovery; archive lifecycle. Fallback: summary
   card stays.
5. **Interaction + export (~1 week).** callback_query + ✅/❌ approval on
   proposals; one-way Notion export for comparison-shaped projects.

Each phase deploys via `make build` + SIGHUP. Total ~3-4 weeks part-time.

## Open Questions

1. **Group projects under always-double routing:** v1 hosts projects in DMs
   with single-agent ownership. Are family-group projects in scope, and if so
   what is the ownership rule — owner = creating agent with the peer deferring
   via a2a, or something stricter? (Both agents currently answer every group
   message.)
2. **Threaded Mode on the family user's DM:** enabling it converts her entire
   agent DM into a forum-style chat — a visible UX change to her main
   surface. Acceptable, or should projects live in her flat DM (thread 0) /
   human-created group topics until she is comfortable?
3. **Cost & cadence:** each scheduled research fire is a full prompt-mode
   agent turn with tools. Default cadence per project (daily? 2-3×/week?),
   and should a monthly per-project budget cap be enforced?
4. **Notion's role:** the recommendation is one-way export only, for
   structured comparison projects — no inbound sync loop. Given Notion MCP
   was dropped for token cost, is even one-way export in, or is
   workspace + Telegram the whole story?
5. **Owner edit path & canonicity:** proposed rule is canonical-store-wins —
   edits via chat, CLI, or direct git commit; uncommitted hand-edits get
   auto-classified as human edits. Sufficient, or is a merge story /
   write-lock needed for concurrent agent+human edits?

## Revision after owner review (2026-08-12)

Owner comments on the first pass set two constraints that supersede parts of
the recommendation above:

1. **Don't force Telegram to be the project surface.** Telegram is the
   family's communication protocol, not a canvas — it was not built for
   live-document collaboration, and the rich-message/Threaded-Mode bet asks
   the platform to be something it isn't. Pinning a status message is fine as
   *tracking*, but the project experience should live where shell controls it.
2. **The family user must be able to read and comment on a real doc** — the
   ability to point at a specific place in the document and say "change this"
   is crucial, and chat is a poor medium for positional feedback. Notion (or
   Google Docs) already has the document + comment model; the adapter should
   evolve to close that loop rather than rebuilding commenting in chat.

### What changes

- **Dropped**: Phase 3 (rich-message pinned doc, Threaded Mode DM topics) and
  Phase 4 callback buttons. The rich-message spike is no longer needed.
- **Kept** (owner-confirmed): the `projects` registry, the git-backed project
  folder — scoped to the projects dir only, its repo strictly separate from
  any code repo — the prompt-mode research schedule, the ghost tag, and
  Telegram for notification + conversational steering (short deltas with a
  doc link; optional pinned status message as a tracker).
- **Changed**: the human-facing surface is a **Notion page per project**,
  rendered from the canonical workspace markdown. Feedback flows back through
  **Notion comments**, which become a first-class inbound channel into agent
  turns.
- **Deferred, per owner**: the [Project] block token budget. Measure first —
  log the injected block size per turn (alongside the existing
  `turn: context sizes` log line) and decide on a cap from data.

### The comment loop (verified against the Notion API, 2026-08)

The API supports everything the loop needs, with known hard edges:

- Integrations can **list comments** on a page/block and **reply into an
  existing discussion thread** (`discussion_id`) — so the agent can answer
  the family user's comment in place ("done — changed the hotel to X ✅").
- **Webhooks exist for comments**: `comment.created` / `comment.updated` /
  `comment.deleted`, delivered typically within a minute, plus
  `page.content_updated` for detecting direct human edits. Webhooks require a
  public endpoint (shell-tunnel could serve); **polling via the existing
  scheduler is the v1 choice** — no new public surface, and per-project
  polling piggybacks on the research schedule or a light cron.
- **Integrations cannot resolve comments** and cannot retrieve resolved ones.
  Convention: the agent replies in-thread when it has addressed a comment;
  the human resolves it in the UI, which is the acknowledgment. Shell tracks
  handled `discussion_id`s in the project row to avoid re-processing.
- **Integrations cannot start a new inline thread anchored to a text span**
  (page-level comments only), and comment anchoring granularity is the
  block. Consequence: the renderer must write the doc as reasonably
  fine-grained blocks (one block per option/section) so block-level anchors
  carry enough position information.

### Canonicality (resolves Open Question 5)

The workspace markdown stays canonical; the Notion page is a rendered
projection with a persisted **section → block-ID map** (stored per project)
so re-renders are surgical block updates, never full-page replaces. Two
inbound paths reconcile human input:

- **Comments** (the primary path): comment → agent turn → edit canonical →
  commit (receipt) → re-render changed blocks → reply in-thread.
- **Direct edits** (allowed but reconciled): `page.content_updated` (or a
  poll-time diff against the last rendered state) → agent folds the human
  change into canonical markdown as an explicitly-attributed human edit —
  this replaces the git-dirty-tree human-edit classifier for the family
  user's edits; the owner's SSH/git edits still use the pre-write dirty
  check from the review comments.

This is a bounded version of the sync-bridge proposal's loop: one page per
project, rendered from one canonical file, with block-map state — not a
general two-way document sync.

### Revised phases

1. **Registry (unchanged, weekend).** `projects` table + RPC + skill +
   [Project] block (with size logging) + ghost tag.
   `schedules.message_thread_id` is split out as its own change (review
   comment 3) and is now lower priority — Telegram threads are no longer a
   project surface.
2. **Doc + render (~1 week).** Git-backed project folder (projects dir only),
   doc template, Notion renderer with block map, `export_ref` on the project
   row (kills doc-ID amnesia — see incident note below), research schedule
   producing doc updates + Telegram delta with link.
3. **Comment loop (~1 week).** Poll unresolved comments per active project;
   comment → bounded revision turn (one pass per comment thread, PR-review
   style) → in-thread reply; handled-thread ledger; direct-edit
   reconciliation.
4. **Later, optional**: comment webhooks via shell-tunnel for sub-minute
   latency; Google Docs as an alternate surface behind the same adapter
   interface if Notion friction appears.

### Project home (added 2026-08-13, second research round)

The owner flagged the weakest part of the revised design: how users *see*
their projects ("post a message and list them" is developer ergonomics, not
an experience). A three-way research round (Buzz deep-dive incl. the repo's
own ADR docs; project-home UX across ChatGPT/Claude/Manus/Devin/Notion/Slack;
Telegram Mini App feasibility) converged on the following.

**What the industry converged on.** Every mature agent product added a
dedicated list surface — none ship "scroll back through chat" as the answer.
The unit of the list is the work item, never the conversation, and a row
carries exactly: **name, status, needs-you flag, last activity, one artifact
link**. The converged status vocabulary is Devin's
(`working / waiting-for-you / done / stalled`), and the needs-you bit is the
single highest-value signal. Chat-first products that deliberately skip a
dashboard (CRMChat, Mira, the OpenClaw multi-agent setup) all replace it with
the same move: **the dashboard inverted into a scheduled digest message** —
one product found its morning digest eliminated dashboard visits entirely.
Buzz's project model independently matches shell's registry design: a project
(draft NIP-MP) is a *thin metadata card* binding artifacts to one linked
discussion channel; its Home surface is an assembled-at-read-time digest of
"what needs you" with a zero-notification default; its activity feed renders
agent work as mutate-in-place "verb → object → outcome" lines. Separate
run-success from task-success (Claude Code routines doc is explicit: green =
the run didn't crash) — a row's status must be an outcome claim the user can
check ("draft ready — needs your review"), not a process state.

**Recommendation — staged:**

- **v1 (ships with the core loop): the inverted dashboard.** One
  bot-maintained pinned **📋 Projects message** per chat: one row per active
  project — status emoji, name, needs-you flag, last-activity date, Notion
  link — edited in place on every change (bot-own messages have no edit age
  limit; the pin makes it one tap from the chat header). A `/projects`
  command re-renders it on demand. The existing heartbeat contributes a
  digest line **only when something needs the user** (zero-notification
  default). Rows deep-link both ways: → Notion doc, → the t.me message link
  of the project's conversation; the Notion page header links back to the
  chat. Archived projects leave the list; Notion is the archive. Cost:
  ~zero new surface — it reuses PinChatMessage + editMessageText already in
  Phase 2.
- **v2 (committed upgrade path, gated on outgrowing v1): Telegram Mini App
  project hub.** Feasibility verdict is GO-lean: `initData` HMAC gives
  family members zero-login cryptographic auth (allowlist by Telegram user
  ID, ~30 lines of Go); no Telegram review process; the go-telegram/bot
  library already in use supports web_app buttons and setChatMenuButton;
  build is 2–4 days (static page + `GET /api/projects` + auth middleware on
  the existing RPC/HTTP plumbing). **Hard precondition: a stable HTTPS
  URL** — quick tunnels' rotating URLs break every launch path except
  re-sent DM buttons; Tailscale Funnel on the already-tailnetted server is
  the zero-cost fix (Cloudflare named tunnel + cheap domain is the
  alternative). Group-chat entry requires a BotFather-registered
  `t.me/<bot>/<app>` link (fixed URL); DM buttons may carry dynamic URLs.
  Inline Notion rendering is v3 at best (Notion refuses iframes; would need
  server-side render via the existing REST skill). Trigger for building v2:
  more than ~3-4 concurrent active projects, or the family user wanting
  doc-reading without leaving Telegram.

**Data-model impact:** none beyond what the registry already carries — the
pinned Projects message ID slots into the existing `pinned_msg_id`
(repurposed: one per chat rather than per project, resolving review comment
4 in favor of a chat-level tracker), and `last_human_activity_at` +
`review_after` (already added for lifecycle) supply the row fields. The
Mini App's `/api/projects` reads the same `projects` table.

### Incident note (2026-08-12, motivating `export_ref`)

The morning of this revision, a live turn for the family user spent ~2 of
3.5 minutes re-deriving a Notion database ID — ghost searches, four sqlite
greps over memory DBs, and a scan of historical tool-use rows — before one
`create-row` call. First visible reply: 213s (typical: 15-50s). The registry's
`export_ref` column is the structural fix; the doc-ID-amnesia failure mode is
no longer hypothetical.

## Evidence Appendix

Codebase (verified this pass):

- `internal/store/store.go:176-184` — sessions UNIQUE(chat_id, message_thread_id)
- `internal/store/store.go:425-446` — topic_threads: summary + open_commitments,
  "per-conversation operational state, not memory"
- `internal/scheduler/storeadapter.go:113-114`, `internal/daemon/daemon.go:721-726`
  — schedules thread-blind, fires hardcode thread 0
- `internal/scheduler/scheduler.go:571-581` — prompt-mode schedules run full
  agent turns
- `internal/bridge/transport.go:9-18` — Transport = Notify/SendPhoto/SendVideo
  only; zero grep hits for SendDocument/Pin/ForumTopic/InlineKeyboard
- `internal/telegram/bot.go:44-48` — AllowedUpdates = Message + MessageReaction
- `internal/bridge/artifacts.go:48-68` — `type="document"` silently dropped
- `internal/bridge/topic_hook.go:145-225` — classifier keyed by chatID only;
  "ThreadID" naming collision
- `internal/daemon/daemon.go:454-462`, `internal/bridge/prompt.go:57-62` —
  workspace = prose-advertised scratch dir, no lifecycle
- `skills/notion/SKILL.md:37` — "this skill stores none"
- `internal/store/store.go:611-617`, `internal/store/tasks.go:115-128` —
  migration-order trap; write-first SQLITE_BUSY pattern to copy
- `internal/scheduler/messageturn.go:28-74` — transport-neutral MessageTurn/Sink
  seam
- `docs/ADR-BUZZ-ADOPTION.md:134-147` — follow-up #3: "a canvas per Telegram
  topic… shell's own doc skill, not Buzz's store"

Telegram Bot API (10.2, July 2026):

- Rich Messages (10.1): https://core.telegram.org/bots/api#sendrichmessage —
  size cap undocumented, spike required
- Edit own messages, no age limit; `rich_message` param:
  https://core.telegram.org/bots/api#editmessagetext (48h window is
  business-messages-only; deleteMessage capped at 48h)
- Forum topics in bot private chats (9.3):
  https://core.telegram.org/bots/api#createforumtopic + BotFather Threaded Mode
- Pinning free/silent in DMs; bots cannot enumerate pins:
  https://core.telegram.org/bots/api#pinchatmessage
- Checklists business-account-only:
  https://core.telegram.org/bots/api#sendchecklist
- Inline keyboards, callback_data ≤64 bytes:
  https://core.telegram.org/bots/api#inlinekeyboardbutton
- Rate limits ~1 msg/sec/chat: https://core.telegram.org/bots/faq

Project-home round (2026-08-13):

- Buzz project = metadata card + one linked channel (draft NIP-MP):
  https://github.com/block/buzz/blob/main/docs/nips/NIP-MP.md ; Home digest +
  zero-notification default, verb→object→outcome activity rendering:
  https://github.com/block/buzz/blob/main/VISION_ACTIVITY.md ; repo's own
  ADR already rejected Buzz's canvas/store (raw-markdown editing, no write
  guard) — docs/ADR-BUZZ-ADOPTION.md:78-91
- Sessions/tasks-as-todo-list convergence: Devin session states
  (working/waiting_for_user/finished):
  https://docs.devin.ai/api-reference/v3/sessions/post-organizations-sessions ;
  Claude Code web sessions + routines (green = run success, not task
  success): https://code.claude.com/docs/en/routines ; ChatGPT
  Projects/Tasks: https://help.openai.com/en/articles/10291617-tasks-in-chatgpt
- Digest-replaces-dashboard pattern (chat-native):
  https://crmchat.ai/blog/ai-chatbots-in-telegram-mini-apps ;
  https://www.dan-malone.com/blog/building-a-multi-agent-ai-team-in-a-telegram-forum
- Mini App: initData validation spec —
  https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app ;
  keyboard/inline web_app buttons are private-chat-only with dynamic URLs;
  group entry needs BotFather-registered fixed URL —
  https://core.telegram.org/bots/webapps

Notion API (verified 2026-08-12 for the revision):

- Comments: list per page/block, create, reply via `discussion_id`; cannot
  resolve, cannot retrieve resolved, cannot open a new inline thread on a
  text span; block-level anchoring —
  https://developers.notion.com/docs/working-with-comments
- Webhooks: `comment.created/updated/deleted` (non-aggregated),
  `page.content_updated` (aggregated); signal-only payloads, ~1 min delivery,
  requires public endpoint —
  https://developers.notion.com/reference/webhooks-events-delivery

Prior art:

- Claude Tag (channel = project boundary, channel-scoped memory,
  self-scheduled follow-ups):
  https://venturebeat.com/technology/anthropic-launches-claude-tag-replacing-its-slack-app-with-a-persistent-ai-teammate-that-learns-monitors-and-works-autonomously
- Agents in collaborative editors (propose-don't-edit, explicit approval):
  https://arxiv.org/html/2509.11826v1
- Codex PR-review one-fix-pass rule:
  https://developers.openai.com/codex/integrations/github
- ChatGPT Canvas deprecation (durable pattern = persistent artifact with
  surgical edits, not the split pane):
  https://medium.com/@mubashirburfat4/i-used-chatgpts-canvas-feature-for-six-months-then-openai-quietly-killed-it-88c542f1a63f
- Notion 3.0 (structured records for comparisons; bounded autonomy budget):
  https://www.notion.com/blog/introducing-notion-3-0
- Community report: streaming edits degrade rich formatting — doc messages
  must be edited via whole-document re-render only, never the streaming path
  (NousResearch/hermes-agent issue #46009)
