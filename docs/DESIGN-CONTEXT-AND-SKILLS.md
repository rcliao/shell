# Design: shared context that is true, skills that load, agents that author their own

2026-09-23. Owner-approved plan following the response-quality review
(`~/.shell/evolve-reviews/response-quality-review-2026-09-23.md`). Evidence
appendix at the end.

## Intent

Three defects made good agents give worse answers, and one dormant loop is
the owner's stated direction:

1. **The shared group context lies by omission.** The transcript block holds
   only agent replies (never a human message), has no thread, and hides each
   agent's own replies even from other threads. Agents miss corrections,
   leak coordination from other threads into answers, and "forget" what they
   themselves found an hour earlier elsewhere.
2. **Search fails silently.** The web-search skill prints "No results found"
   and exits 0 when the provider is blocked; agents then answer from memory
   and sometimes say they checked.
3. **Skills the agent wrote do not reach its prompt.** All hot skills share a
   1,000-token budget measured at bytes ÷ 4, so a Chinese-heavy skill counts
   three times too small per character yet still overflows, and is dropped
   without a log line.
4. **Self-authoring is dormant.** The deep-heartbeat skill retrospective was
   switched off because its usage meter never recorded anything. Agents have
   authored one skill each; each should become more distinct in what it does.

**Constraint (owner decision, 2026-09-23):** agents in the group answer
independently — "blind until observed". Nothing here makes an agent wait
for, read, or coordinate with the other before it answers. Recording what
was said in the group is observation after the fact and is in scope.

Out of scope: undated rotation summaries and task-list noise, raw API-error
delivery and doc-write honesty, a reply-language guard (review items 4–6).

## Components

| Component | Change |
|---|---|
| `internal/transcript` | `thread_id` column; human rows recorded (deduplicated across agents); thread-scoped reads; own replies from other threads |
| `internal/telegram` handler | records every human group message, before the respond/yield decision |
| `internal/bridge` | records agent replies with their thread; builds the block from the new reads |
| `internal/search`, `cmd/shell-search`, `skills/web-search` | a blocked provider is an error, not an empty result; the skill points to the built-in WebSearch first |
| `internal/skill` | CJK-aware token estimate; `<!-- hot -->` rules section; demotions reported |
| `internal/store` | per-skill usage from `tool_uses` |
| `internal/bridge` heartbeat | skill retrospective back in deep beats, on the real meter; skill changes committed + owner notified |
| `cmd/shell` | `shell skills report` |
| config | `agent.owner_chat_id` — where skill-change notices go |

## Data flow

**A human writes in a group thread.** Both daemons receive it. Each records
it in the shared transcript (`chat_id`, `thread_id`, `telegram_msg_id`,
sender, text) before deciding whether to answer; the second insert is a
no-op (unique on chat + message id for human rows). A message addressed to
the other agent is still recorded — the yielding agent observes it.

**An agent builds a turn.** The transcript block now has two parts: every
recent message in *this* thread from anyone but itself (humans included,
each line with its local time), then — separately labelled — its own last
few replies from *other* threads in the last 24 h. Its own replies in this
thread stay out (the session already has them). Other agents' messages from
other threads stay out: that is how coordination about thread A leaked into
an answer in thread B.

**An agent searches.** If no provider works, the script exits non-zero with
"search unavailable — use the built-in WebSearch tool". The skill text leads
with WebSearch and keeps the place-verification contract.

**The prompt is assembled.** A hot skill contributes its `<!-- hot -->` …
`<!-- /hot -->` section if it has one (the rules that must always hold),
else its whole body; the rest stays on disk behind a pointer. Tokens are
estimated per rune class (ASCII ≈ 4 chars/token, other runes ≈ 1 token).
A hot skill that still does not fit is listed in the catalog as "hot, not
loaded (over budget)" and logged once at startup.

**A deep heartbeat runs.** The retrospective shows each skill's real usage
(runs and last use over 30 days, from `tool_uses`; script calls and reads
of its SKILL.md) and the playground drafts, with the action menu. The agent
may author into `skills/playground/<name>/` (never loaded), graduate a
draft into `skills/<name>/`, change tiers, or retire.

**After any heartbeat.** The harness checks the agent's skills directory in
the `~/.shell` git repo. Any change is committed (author = the agent) and
the owner gets one line in their DM: what changed and the revert command.
No approval step — notify and revert (owner decision).

## Data model

`~/.shell/shared/transcript-v2.db` `messages`:
- `+ thread_id INTEGER NOT NULL DEFAULT 0` (older rows read as thread 0)
- `+ UNIQUE (chat_id, telegram_msg_id) WHERE sender_type='human' AND telegram_msg_id > 0`

Config: `agent.owner_chat_id` (int64; 0 = no notices).

SKILL.md: optional `<!-- hot -->` / `<!-- /hot -->` markers in the body.

## Interfaces

- `transcript.Store.Record` ignores a duplicate human row.
- `transcript.Store.RecentThread(chatID, threadID, tokenBudget)`,
  `transcript.Store.OwnElsewhere(chatID, threadID, agent, limit, since)`,
  `transcript.FormatTranscript(thread, own []Entry, self string)`.
- `bridge.Bridge.RecordHumanMessage(chatID, threadID, msgID, at, sender, text)`.
- `skill.EstimateTokens` changes value; `skill.Registry.Demoted() []string`.
- `store.Store.SkillUsage(since) map[string]SkillUse`.
- CLI: `shell skills report [--days N]`.

## Plan

1. Shared context (transcript + handler + bridge), tests.
2. Search honesty, tests.
3. Skill loading, tests.
4. Self-authoring loop (meter, retro, commit + notify), tests.
5. `shell skills report`.
6. Fresh-context review; deploy; each agent restructures its own hot skill
   into a rules section (self-authored, via a maintenance turn).

*Verify:* a human message appears once in the shared transcript and in the
other agent's next block; a turn in thread B carries no peer text from
thread A; a blocked search exits non-zero; `meal-memo`'s rules are in Pika's
prompt (startup log lists no demotions); a deep beat shows non-zero usage;
a skill edit produces a commit and an owner DM line.

## Evidence

- `transcript-v2.db`: 381 agent rows, 0 human rows in 7 days;
  `RecordTranscript`'s only caller records agent replies (`bridge.go`);
  `FormatTranscript` skips own entries; no thread column.
- 9/23 02:09 UTC: Pika's answer in thread 2479 ended with a note about a
  reminder from thread 1419; 9/19: Umbreon repeated an error Pika had just
  been corrected on; 9/21: Umbreon "had not checked" hours it checked in
  another thread 26 minutes earlier.
- `registry.go`: `HotTierBudget = 1000` for all hot skills;
  `EstimateTokens = (bytes+3)/4`; `meal-memo` 8,854 bytes; the agent read
  its SKILL.md by hand 17 times in 14 days; grade emojis it forbids appeared
  in chat 9/18–9/21.
- `heartbeat.go`: retro not injected since 2026-09-02 — USAGE.jsonl never
  written. `tool_uses` records every skill script call by path (30 d: Pika
  notion 396, browser 316, web-search 111; Umbreon browser 223, notion 131).
- web-search: DuckDuckGo denied inside the agent's sandbox, HTTP 202 outside;
  both render "No results found", exit 0.
