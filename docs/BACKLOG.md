# Backlog

Ideas for shell that nobody is working on yet. Each has a short entry. When one is
picked up it gets a design doc (`docs/DESIGN-*.md` or `docs/PLAN-*.md`) and its
entry here links to it and moves to **Picked up**.

This is the owner-facing list. `.evolve/BACKLOG.md` is the older
improvement-loop backlog (V2-H*); it has not been touched since 2026-08-07.

## Direction

The owner's direction, 2026-09-23: agents become more autonomous and evolve to meet
what the family needs. Ghost is the memory that lets an agent act on its own. Shell
is the infrastructure around the agent, and it widens what the agent *can* do. It
does not decide for the agent.

Test for every entry: does it move a decision *into* the agent, or into harness
code and owner approval? Prefer the first. The harness gives true signals
(meters, transcripts, notices) and power the owner can undo (commits, revert). It
does not give rules. Standing constraint: in the group, agents answer
independently ("blind until observed"). Nothing here makes one agent wait for or
coordinate with the other before it answers.

Progress is tracked in `~/.shell/evolve-reviews/autonomy-watch.md` (weekly check,
schedule #182).

## Accepted suggestions (from the agents' weekly reviews)

These were filed by the agents themselves in their first reviews (2026-09-25)
and accepted by the owner. When one ships, mark it done with
`shell suggestions decide <id> done --note "…" --config <agent config>`, so the
agent sees the outcome at its next review.

| Agent #id | Suggestion | Where |
|---|---|---|
| pikamini #1 — **done** (PR #31; family asides were already filtered since 9/17, this fixed the review summaries) | Working notes written before tool calls leak into family replies. Deliver only the final message. This also fixes the review summaries, which carry lines like "Filed. Now the DO step…" | `internal/process/protocol.go` (prefers all text over the final result) |
| pikamini #2 — **done** (PR #33) | The CLI's synthetic API-error messages are relayed to Telegram as normal replies. Detect them; retry or alert instead | bridge reply path |
| pikamini #3 — open: ghost repo, needs routing there | ghost `Context()` tag filters are a hard AND, which silently narrows recall. Make tags a scoring boost | ghost repo (`ContextParams.Tags`) |
| umbreonmini #1 — **done** (PR #32) | The save checker does not count `plantlog`, the Google skill's writes, or `project doc-write` as writes. 3 of 4 flagged "confabulations" were real saves, and every OwnerEval number built on that checker was inflated | `isPersistenceTool` (write verify / OwnerEval) |
| umbreonmini #2 — **done** (PR #32) | `shell-remember`, `shell-relay` and `shell-task` fail silently when the sandbox blocks the bridge socket, yet count as successful writes. They should exit non-zero and say why | `skills/*/scripts` |

Status 2026-09-25: four of five shipped the same day, and each was marked
done so the agent sees it at its next review. pikamini #3 is in the ghost
repo.

## Ideas

### 1. External events: things happening in the world reach the agent

**Why.** Today an agent acts when someone messages it, when a schedule fires,
or on a heartbeat. The heartbeat is a timed "anything to do?" that usually finds
nothing and still costs a full turn. An agent that hears "an email arrived", "a
calendar event moved" or "the page you watch changed" can react or act ahead
without being asked. That is the move from reactive to proactive.

**What exists.** Most of the plumbing:
- Event-mode schedules put a task on the queue. `project.event` is the first
  kind (`internal/project/event.go`, `internal/daemon/projectevents.go`).
- One Notion poller turns what it sees into normalized events
  (`notion.comment.created`, `notion.page.edited`). The code already names "a
  future webhook producer" that emits the same shapes. This is the pattern to
  generalize.
- `docs/TASKS.md` step 3 designs the durable queue. It has a generic `kind` and a
  JSON `payload`, per-kind tool authority, and three handler tiers. In tier 2,
  **an agent writes the worker as a skill** that declares `task-kinds:` in its
  frontmatter.
- `gog` already reads Gmail and Calendar (`TOOLS.md`).

**Shape.**
1. **Producers.** Deterministic pollers on a cron: Gmail, Calendar, Notion
   (exists), and later RSS or web-page watches. Webhooks can come later where a
   service offers them.
2. **Events.** Each producer emits a normalized event: `source`, `kind` (such as
   `email.received`), a dedup id, a one-line summary and a reference. The event
   does **not** carry the full content: the agent fetches that only if it cares.
   Email is sensitive, and the queue should not become a copy of the inbox.
3. **Subscriptions.** The agent owns these. Its own skill declares which events it
   handles. The retro shows which subscriptions fired and what the agent did with
   them, so it can drop or add some.
4. **Triage.** Before any expensive turn, a cheap filter decides "worth a turn?".
   The agent writes the rules, or a typed decision model answers it. This is
   where the Jev shadow router could stop being shadow-only (see idea 5). Without
   triage, one full agent turn per email is too costly.
5. **The agent turn.** It reacts (tells someone, files something) or quietly
   updates memory. Queue guarantees apply: once, leased, with evidence of
   completion.

**Open questions for the owner.**
- Should an agent-written worker that runs unattended need your one-time read
  before it can run? TASKS.md says yes, "promotion is a gate". The skill loop
  shipped 2026-09-23 is notify-and-revert with no gate. Workers that act on email
  at 3am may deserve the stricter rule.
- Whose inbox does an agent watch, and which agent? One family member's email is
  not another's to hand to an agent.
- Should events that no one has subscribed to be kept (so an agent can discover
  a need later) or dropped?

**Routing.** An event is just another message for the router in
`docs/DESIGN-ROUTER-AND-SUGGESTIONS.md`, which puts it in a project lane.

**First step.** Finish TASKS.md step 3 far enough to handle an event kind. Then
add one producer, a Gmail poll of the owner's own inbox only, with one subscription that
the agent writes itself. Measure how many events it triages to a turn and what it
does with them.

### 2. Heartbeats that know when to skip — measured, decision open

Measured 2026-09-26, over 7 days per agent. About 80% of heartbeats end in
`[noop]` (55 of 66 for one agent, 50 of 65 for the other), and heartbeats cost
about $28 per agent per week. Cache warm-ups cost more ($36 and $26) and
roughly quadruple on deploy days: about $2 a day normally, $8–9 on days with
several restarts, since every restart re-warms each active session, lane
sessions included.

Options:
- Skip a heartbeat when nothing happened since the last one (no new
  messages in active chats, nothing due).
- Let the agent pick its own heartbeat cadence (the autonomy direction).
- Batch deploys.

Skipping changes how proactive the agents are, so it is the owner's call.

### 3. Know who wrote each memory — traced

Traced 2026-09-26:
- Most "unset" rows were not the agents' own. The development assistant's
  `agent:claude-code` memories are stored in the same `memory.db` files
  (242 and 175 rows). The weekly check (#182) now counts only each agent's own
  namespace.
- The agents' own unset rows are their daily briefing entries (written
  with `ghost_put` and no source: the agent's choice) and harness exchange
  summaries. The summaries come through ghost's consolidate, which drops
  `source_kind`; that is a **ghost-repo fix**.
- Open question: should the development assistant's memories live in the
  agents' memory files at all? Retrieval is namespace-isolated, so nothing
  leaks into their prompts, but it muddies every count.

### 4. The shared transcript records everything a person says

Related fix shipped 2026-09-26 (PR #43): warm-ups, heartbeats, the review and
the system chat no longer reach the shared transcript.


**Why.** Photo albums (`processAlbum`) and bot commands bypass `HandleMessage`, so
they are never recorded. The other agent then misses them after the fact.

**First step.** Record in `processAlbum` and in the command path, using the same
`RecordHumanMessage` call.

### 5. Graduate the decision model out of shadow — in design

Folded into `docs/DESIGN-ROUTER-AND-SUGGESTIONS.md` (step R0 merges the Jev,
tier and topic shadows into one router). Kept here for the history.


**Why.** Jev has run in shadow mode since P3.7, and the verdict is due around
2026-09-28. The owner agreed Jev should help curate ghost memory (reflect, link
related memories). A working typed-decision layer is also the triage step in
idea 1.

**First step.** Write the verdict from the recorded shadow decisions. If Jev
passes, the first place it acts is memory curation, not routing. Routing stays
off while the agents are blind until observed.

### 6. Agent-layer commits that never collide

**Why.** Both daemons commit into the same `~/.shell` repo. The in-process mutex
does not cover two processes, so a simultaneous commit hits `index.lock`. The
failed commit is retried after the next heartbeat. That is fine today; with
event-driven workers (idea 1) committing more often, it will not be.

**First step.** Use a file lock around the commit, or give each agent its own
repo.

### 7. Response-quality items left out of the 2026-09-23 run

From `~/.shell/evolve-reviews/response-quality-review-2026-09-23.md`:
- Undated rotation summaries and task-list noise in the context.
- Raw API errors delivered as replies, and doc writes claimed without proof.
- A reply-language guard.

### 8. Secrets hygiene

- `OP_SERVICE_ACCOUNT_TOKEN` leaks into the child environment (possible
  `secrets.strip`).
- `shell/.env` still holds the Jev key. Delete it now that the key is in the
  encrypted store.

## Picked up

- 2026-09-25, **in design**: a first-class message router (one chat split into
  project lanes, with a model per lane), and agents suggesting improvements to
  themselves and to how they work with a human. See
  `docs/DESIGN-ROUTER-AND-SUGGESTIONS.md` (inspired by Meta's Muse). Waiting on
  the owner's answers to its open questions.

- 2026-09-23: shared context, search honesty, skills that load, self-authoring
  loop, and `shell skills report`. See `docs/DESIGN-CONTEXT-AND-SKILLS.md`
  (PR #21) and the hot budget raised to 3,200 (PR #22).
