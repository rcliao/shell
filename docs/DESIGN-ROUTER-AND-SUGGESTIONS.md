# Design: one router that splits a chat into lanes, and agents that suggest how to work better

Status: **accepted direction, 2026-09-25**. Nothing is built yet. The owner
answered the open questions on the same day; see "Decisions" at the end.

Inspired by Meta's Muse (launched 2026-09-08). Muse has one main chat,
side chats that the user creates by hand, and a Goals view derived from past
conversations. It learns preferences as memories and makes more suggestions as
its long-term memory grows. Shell goes one step further on both counts. The
split into projects happens without the user doing anything, and the agent
suggests improvements to *itself and to how it works with the human*, based on
evidence from what it did.

## Intent

1. **The message router becomes a first-class part of shell.** Every
   incoming message is routed to a **lane**: a project, a topic, or general.
   The lane chooses the session (so the context is focused), the project context,
   and the model. One chat can then hold several projects without the user
   opening threads, and each project keeps its own memory of the conversation.
2. **Each agent suggests improvements from evidence.** On a fixed cadence the
   agent reviews what it did. It looks at corrections, unanswered nudges,
   reactions, skill use, routing mistakes and its own open proposals. It then
   (a) acts on what is within its own power (skills, memory, schedules), and
   (b) proposes at most a few changes that need the human. Those are either
   changes to how the two of them work together, or harness changes for the
   owner. Each suggestion is tracked until a human accepts or declines it, and
   the agent sees the outcome.

**Constraints**
- Agents stay blind until observed. The router runs inside each agent and
  decides *where a message belongs for this agent*. It never decides *which
  agent answers*, and it never waits for the other agent. `should_reply` stays
  shadow-only.
- **Not tied to any channel.** Lanes, routing and suggestions live in the
  bridge. A channel such as Telegram, the CLI (`shell chat`), or later email
  events only delivers messages and renders replies. Nothing in the router or
  the lane model may depend on Telegram. A channel *may* show lanes (for
  example as Telegram topics), but that is an optional adapter feature, and
  every step must be testable from the CLI without Telegram.
- Decisions move into the agent (owner direction, 2026-09-23). The router
  proposes a lane, and the agent can move a message to another lane or create a
  new one. A human only confirms things other people will see, such as a new
  visible topic or a suggestion aimed at them.

**Out of scope**
- Routing between agents.
- External events (backlog idea 1). They reuse the router later: an event is
  just another message to route.
- Replacing ghost retrieval.

## What exists (from the 2026-09-25 survey)

Shell already has **three routers, and none of them acts**.

| Router | What it decides | State | Evidence |
|---|---|---|---|
| Topic classifier (`internal/topic`) | topic of the turn, keyword cascade with an optional Haiku tier | Live, but keyword-only (`topic_keyword_only: true`). 79–86% of turns come out "general". Keyed by **chat only**, so all threads of a chat share one topic pointer. `topic_turn_log` is never written. | 7 d: 339 / 294 decisions, 268 / 253 of them "general" |
| Tier router | model tier per turn | Shadow. `tier_decisions` is written and no code reads it. | ~398 / 359 rows in 7 d |
| Jev shadow (`internal/decide`) | which project; open question; decision; should-reply | Shadow. `router_decisions` is written and no code reads it. | 4 of 5 meal logs placed in the health project |

What the other pieces do today:
- **Sessions** are one per `(chat, thread)`.
- **Projects** can be bound to a forum thread, and a bound thread gets a scoped
  `[Project]` block. Only 1 of the 5 active projects is bound. Nothing in shell
  can create a Telegram topic.
- **Models** are chosen per chat (`model_routing.chat_models`) or per task kind.
  Nothing chooses a model per project.

The suggestion side has the same pattern: the pieces exist, and none of them
reaches a human.
- **Proposals.** 36 proposals in the ghost namespace `loop:proposals` (Pika
  30, Umbreon 6). They are real harness bugs, and only the developer loop reads
  them.
- **Reflections journal.** 77 / 76 entries, and every entry in the last 7 days
  is `[noop]`. It is readable only through `shell reflections`.
- **Lesson-action ledger.** It exists, with 77 entries per agent.
- **OwnerEval.** A regex count of factual corrections, complaints, unanswered
  nudges and similar. It last ran on 7/12 and is never scheduled.
- **Reactions.** 👍 and 👎 are *commands* (go / stop), not a feedback signal.

Telegram: since Bot API 9.4 (2026-02-09), a bot can create forum topics
**inside a private chat** without admin rights. A DM can therefore have
visible project threads.

## Components

| Component | Change |
|---|---|
| `internal/route` (new) | One `Router`: message + context → `Route{Lane, Confidence, ModelTier, Reason}`. Backends are pluggable: Jev (typed choice), Haiku, and the keyword cascade as a fallback. Replaces the three shadows. |
| Session keying | `(chat, thread)` becomes `(chat, thread, lane)`. The default lane is `general`, so existing sessions keep working unchanged. |
| Lanes | A lane is a project slug, a topic id, or `general`. A project can declare `model` / `effort`. |
| Agent tool `lane` | `lane(action=move\|new\|list)`. The agent re-files the current message, or opens a lane. Every move is a label for the router. |
| Channel adapters | A small interface: `DeliverReply(lane, …)`, plus an optional `ShowLane(lane)`. The CLI adapter ignores lanes. The Telegram adapter can map a lane to a forum topic (possible in DMs since Bot API 9.4), but only after the agent offers and the human accepts. |
| `internal/bridge` review | A cadenced **collaboration review** per agent. It builds an evidence pack and runs one agent turn with a fixed output contract. |
| Suggestions | The ghost namespace `loop:proposals` gains a status lifecycle and an audience. Delivery goes to the right human. Outcomes are fed back to the agent. |
| Feedback signals | Reactions are also logged as feedback (👎, 🔄 and "stop" count as negative). OwnerEval is scheduled per agent. |

## Data flow

**A message arrives on any channel (Telegram, CLI, later events).**
1. The channel adapter hands the bridge a channel-neutral message: chat,
   thread, sender, text, message id. The handler records it, as it does today. The agent's router receives the
   message plus the chat's recent lanes, active projects and the lane of the
   previous message.
2. The router answers with a lane and a confidence.
   - **Sticky rule:** below a threshold, the message stays in the lane of the
     previous message. Chat moves in runs, not message by message, and flipping
     lanes on every message would fragment the context.
   - A confident new lane starts or resumes that lane's session.
3. The turn runs in the lane's session with that lane's project block and
   model.
4. The reply goes back through the same adapter, to the same chat and
   thread. The human sees nothing change.
5. If the agent decides the message belongs elsewhere, it calls `lane(move)`.
   That turn is re-run in the right lane, or the agent simply notes it for the
   next message (to be decided in P1). The move is logged as a label.

**A new subject keeps coming up.** Several messages end in `general` with the
same new subject. The agent may call `lane(new)`, and later, when it looks
durable, offer to make it a project and, optionally, a visible topic. The
agent decides to offer, and the human decides on anything visible.

**The family chat's deep reflection (collaboration suggestions).** The deep
heartbeat that already reflects on each chat does this; no new job is added.
1. It groups the chat's recent messages by lane, so the week reads as a few
   threads of work rather than one long log. Each group gets a short retro:
   what was asked, what the agent did, where it was corrected or nudged, and
   what stayed open.
2. From those retros, the agent may make **at most one** suggestion to the
   chat about how the family and the agent could work better together (for
   example: "meal logs could go under the health project; want me to keep them
   there?"). It is written in the chat's language and goes into that chat.
3. The family's answer is recorded on the suggestion, like an owner's answer.

**A review is due (weekly, per agent: the owner-facing part).**
1. The harness assembles an evidence pack for the agent's week. It contains
   OwnerEval counts, reactions, lane moves (routing mistakes), skill usage, open
   and resolved suggestions with their outcomes, and the reflections journal.
2. The agent runs one turn with an output contract:
   - **Do**: changes within its own power, which it makes now (skill, memory,
     schedule). These are auto-committed and visible to the owner.
   - **Ask**: at most 3 suggestions that need a human. Each has an audience
     (`owner` for harness or infrastructure, or the person it collaborates with,
     for how the two of them work together), the evidence it rests on, and one
     concrete change.
3. Each suggestion is delivered to its audience in their language, one message,
   with accept / decline buttons.
4. The answer is stored on the suggestion (`source_kind=stated`). The next
   review shows the agent what was accepted, declined or done. That is how it
   learns what this human values.
5. Accepted harness suggestions are added to `docs/BACKLOG.md` by the next
   development session.

## Data model

- `route_decisions`, one table per agent, which replaces `router_decisions`,
  `tier_decisions` and `topic_decisions` for new rows. Columns: `chat_id,
  thread_id, msg_id, lane_prev, lane, confidence, backend, model_tier, reason,
  label_source (none|agent_move|project_write|human), label_lane,
  created_at`.
- `sessions`: add `lane TEXT NOT NULL DEFAULT 'general'`. The unique key becomes
  `(chat_id, message_thread_id, lane)`.
- `projects`: add `model`, `effort` and `channel_ref` (an optional
  adapter-specific place the lane is shown, such as a Telegram topic id; empty
  means invisible).
- Suggestions in the ghost namespace `loop:proposals`: add fields `status`
  (proposed / accepted / declined / done / withdrawn), `audience`,
  `evidence` (refs), `decided_by`, `decided_at` and `outcome_note`.
- `feedback_events`: `chat_id, msg_id, kind (reaction|correction|nudge),
  value, created_at`.

## Interfaces

- `route.Router.Route(ctx, Msg) (Route, error)` and `route.Backend`.
- Agent tool `lane(action, slug?, reason)`. Its RPC endpoint is
  `POST /lane`.
- `bridge.RunCollaborationReview(agent)`, triggered by a schedule of kind
  `review`.
- CLI:
  - `shell route report [--days N]`: agreement with labels, lane churn, cost.
  - `shell suggestions [--status]`: list suggestions and their outcomes.
- Config:
  - `route.backend` (jev|haiku|keyword).
  - `route.sticky_threshold`.
  - `route.lanes_enabled` (per chat, off by default).
  - `review.cadence`.
  - `review.max_asks`.

## Plan

Each step is shipped, measured, and then the next one starts. Routing starts
in shadow; suggestions start with the owner.

| # | Step | Visible to family? | Pass bar before the next step |
|---|---|---|---|
| R0 | One `Router` in shadow: merge the three shadows, log `route_decisions`, labels from project writes and the agent's own `lane` calls (tool available, no effect yet) | no | 2 weeks; agreement with labels ≥ 85%, and churn below one lane change per 5 messages |
| R1 | Lanes on for a **CLI test chat** (`shell chat --chat <test id>`), then the **owner's DM**: session per lane, sticky rule, `lane` tool live | owner only | Replayed CLI conversations route as labelled; the owner judges DM answers no worse; context per turn shrinks |
| R2 | Per-lane model and effort | owner only | Cost per turn down with no quality complaints |
| R3 | Visible lanes through a channel adapter (Telegram topics first): agent offers, human accepts | per offer | Owner decides |
| R4 | Lanes in the family DM, then the group | yes | Owner decides after R1/R2 numbers |
| S0 | Close the loop, owner only: weekly review turn, suggestions with a lifecycle, and a message to the owner with buttons; schedule OwnerEval; log reactions as feedback | owner only | The owner finds at least 1 in 3 suggestions worth accepting |
| S1 | Family chat deep reflection: group by lane, retro each group, at most one collaboration suggestion into the chat, in its language | yes | Accept rate, and no complaint about noise |

S0 can start at once and does not depend on R0. The weekly check #182
(Pika reporting on both agents) either becomes the owner digest of S0
or is retired.

*Verify:*
- R0 is measured with `shell route report`.
- A lane move re-runs in the right session (test).
- In R1, the same message sent twice in two lanes gets different project blocks.
- S0 delivers a suggestion with buttons, and a decision lands on the suggestion
  and shows up in the next review's evidence pack.

## Decisions (owner, 2026-09-25)

1. **Visible lanes are fine, but do not design around Telegram.** Start
   invisible. Telegram is one more source of incoming messages; the CLI is
   another and is the first test path. Visible lanes are an optional channel
   adapter feature (R3).
2. **Collaboration suggestions go into the family chat's deep reflection.**
   Group the messages, run a retro per group, and make a suggestion (S1).
   Owner-facing harness suggestions stay in the weekly review (S0).
3. **Router backend:** keep measuring Jev. If routing evaluates well (R0 pass
   bar), Jev may route family messages too.
4. **Retire the topic classifier**, then look for more to simplify (below).

## Simplify after R0

The router makes these redundant. Each is removed only after R0 shows the
router covers it; each removal has its own PR.

| Remove | Why it goes | Data |
|---|---|---|
| Topic classifier (`internal/topic`, the prompt.go hook, topic_hook.go summaries and commitments) | Replaced by lanes. Keyword-only, 79–86% "general", keyed per chat not thread; one agent called its commitments list noise | `topic_threads`, `topic_decisions`, `conversations` drift columns, ghost `loop:topics` (kept read-only until the lane history covers the same period) |
| `topic_turn_log`, `LogTopicTurn` | Never written | table |
| Tier router shadow (`tier_router.go`) | Merged into `Route.ModelTier` | `tier_decisions` |
| Jev shadow as a separate path (`observeRouterShadow`) | Becomes the router's Jev backend | `router_decisions` stays read-only for the verdict |
| `model_routing.topic_classifier` config, `topic_keyword_only` | No classifier left | config keys |
| Reflections journal as a separate table | Folded into the review's evidence pack, if every entry stays `[noop]` | `reflections` |

After those, look again at the per-turn blocks (`[Continuing:]`, `[Topic:]`,
`[Projects]`, `[Project]`). The goal is one lane block per turn.

## Evidence

- Survey, 2026-09-25:
  - `internal/topic` classifier (prompt.go:278, topic_hook.go:29/109/238) with
    `topic_keyword_only` set in both agent configs.
  - `tier_router.go` logs at bridge.go:1427 and has no reader.
  - `internal/decide` is called from bridge.go:958 and has no reader.
  - `SessionKey{ChatID, ThreadID}` at process/session.go:20.
  - Project injection at project_hook.go:42/69.
  - No `createForumTopic` anywhere.
  - propose-backlog writes `loop:proposals`, read only by `cmd/shell-bench`.
  - Deep-heartbeat prompt at heartbeat.go:190.
  - Reflections at heartbeat.go:306.
  - OwnerEval at internal/bench/ownereval.go:106.
  - Reactions map at config.go:495.
- Jev shadow, 2026-09-23 → 25: p50 ~200 ms, 1 timeout per agent, meal logs
  placed 4 of 5, open-question hits real, `has_decision` v1 noisy (fixed to v2,
  PR #24).
- Muse (external):
  - Main chat plus manual side chats, a Goals view derived from conversations,
    memory learned from corrections, proactive suggestions.
  - Sources: TechCrunch 2026-09-08 and 2026-09-23, CNN 2026-09-23, MindStudio
    and DigitalApplied explainers.
- Telegram Bot API 9.4 (2026-02-09): `createForumTopic` works in private chats
  without admin rights.
