# Shell Architecture

Telegram Bot ↔ Claude Code CLI bridge. One Claude Code session per Telegram chat, persisted in SQLite. Sessions survive daemon restarts.

## System Layers

```
┌─────────────────────────────────────────────────┐
│  Telegram (transport)                           │
│  long-poll, reactions, photos, albums, commands │
├─────────────────────────────────────────────────┤
│  Bridge (routing)                               │
│  memory injection, artifact parsing, callbacks  │
├──────────┬──────────┬───────────────────────────┤
│ Process  │ Planner  │  Scheduler                │
│ sessions │ execute→ │  cron, once,              │
│ streaming│ test→    │  heartbeat,               │
│ resume   │ review   │  quiet hours              │
├──────────┴──────────┴───────────────────────────┤
│  MCP Server        │  RPC Server                │
│  shell_pm,         │  /pm, /tunnel, /relay,     │
│  shell_tunnel,     │  /schedule, /memory, /task │
│  shell_relay       │  (Unix socket)             │
├────────────────────┴────────────────────────────┤
│  Store (SQLite)     │  Memory (ghost)           │
│  sessions, messages │  semantic search,         │
│  message_map,       │  namespaces, tiers,       │
│  schedules, tasks   │  exchange logging         │
├─────────────────────┴───────────────────────────┤
│  Utilities                                      │
│  tunnel, pm, worktree, skills                   │
└─────────────────────────────────────────────────┘
```

## Package Map

| Package | Path | Purpose |
|---------|------|---------|
| **main** | `cmd/shell/main.go` | Cobra CLI: daemon, send, status, session, pairing, restart, stop, init, search, mcp |
| **bridge** | `internal/bridge/` | Core routing: Telegram ↔ Claude. Command handling, reaction routing, artifact parsing |
| **process** | `internal/process/` | Claude CLI subprocess lifecycle. Agent interface, session management, streaming |
| **mcp** | `internal/mcp/` | MCP stdio server exposing `shell_pm`, `shell_tunnel`, `shell_relay` as native Claude tools |
| **rpc** | `internal/rpc/` | HTTP-over-Unix-socket RPC server for skill scripts and MCP server |
| **telegram** | `internal/telegram/` | Bot wrapper, handlers, policy-based auth, pairing, rate limiting, allowlist, photo/PDF download, MarkdownV2 formatting |
| **store** | `internal/store/` | SQLite persistence: sessions, messages, message_map, schedules, tasks |
| **config** | `internal/config/` | JSON config from `~/.shell/config.json` with all feature flags |
| **daemon** | `internal/daemon/` | Initialization chain, PID file, signal handling, component wiring |
| **memory** | `internal/memory/` | Semantic memory via ghost library. Namespaces, profiles, exchange logging |
| **planner** | `internal/planner/` | Plan execution: execute → test → review → decide (done/retry/blocked) |
| **project** | `internal/project/` | First-class projects: per-project git doc, Notion render + block map, comment/edit ingestion, adoption, doc budget |
| **decide** | `internal/decide/` | Typed decisions from a decision model (TypeSafe Jev): choice + probabilities, 0–1 beliefs. Shadow router records answers per turn, never acts |
| **scheduler** | `internal/scheduler/` | Cron/one-shot/heartbeat scheduler with quiet hours and noop suppression |
| **skill** | `internal/skill/` | Skill registry: loads `~/.shell/skills/` and generates system prompt |
| **search** | `internal/search/` | Web search cascade: Brave → Tavily → DuckDuckGo |
| **worktree** | `internal/worktree/` | Git worktree isolation for plan execution |
| **reload** | `internal/reload/` | Live reload: watch .go files → rebuild → syscall.Exec |

## Data Flow

```
User (Telegram)
  │ text / photo / reaction / command
  ▼
Telegram Bot (long-poll)
  │ auth check → bridge.HandleMessageStreaming()
  ▼
Bridge
  │ inject memory context + system prompt + skill instructions
  │ inject current time, sender identity
  │ convert ImageInfo/PDFInfo → process.ImageAttachment/PDFAttachment
  ▼
Process Manager
  │ AgentRequest → CLI subprocess (--mcp-config for MCP tools)
  │ parse stream events → onUpdate callback → live Telegram edits
  │ Claude calls MCP tools (shell_pm, shell_tunnel, shell_relay) directly
  │ Claude calls skill scripts via Bash for schedule/remember/task
  ▼
Bridge (response processing — processResponse())
  ├─ [artifact type="image"]   → read file → collect Photo (skill output)
  ├─ [noop]                    → suppress heartbeat output
  │
  │ log exchange to store + memory
  │ return AgentResponse{Text, Photos}
  ▼
Telegram Bot
  │ send Photos → SendPhoto
  │ send Text → edit/chunk + MarkdownV2
  ▼
User (Telegram)
```

## Tool System (Three Layers)

### MCP Tools (first-class, bridge-internal)

Claude CLI connects to `shell mcp` as a stdio MCP server. These tools are called
natively through the MCP protocol — no Bash intermediary.

```
Claude CLI ──MCP stdio──► shell mcp ──HTTP──► bridge RPC (Unix socket)
                                              │
  shell_pm     → POST /pm     → pmMgr.Start/Stop/List
  shell_tunnel → POST /tunnel → tunnelMgr.Start/Stop/List
  shell_relay  → POST /relay  → bot.SendText/SendPhoto + bridge session
```

MCP tools are auto-approved via `--allowedTools mcp__shell-bridge__shell_*`.
Config written to `~/.shell/mcp.json` by daemon, passed via `--mcp-config`.

### RPC Server (Unix socket API)

HTTP server on `~/.shell/bridge.sock` for skill scripts and MCP server:

| Endpoint | Purpose |
|----------|---------|
| `POST /pm` | Process manager operations |
| `POST /tunnel` | Tunnel operations |
| `POST /relay` | Relay messages (routes through bridge for context) |
| `POST /schedule` | Create schedules |
| `POST /memory` | Store memories |
| `POST /task` | Complete tasks |

### Skill Scripts (Bash wrappers)

Loaded from `~/.shell/skills/` and `.agent/skills/`. Each has `SKILL.md` (frontmatter)
and `scripts/` directory. Claude calls them via Bash tool, they call RPC via curl.

## Layer Interfaces

Each layer boundary has typed inputs and outputs. No plain-text encoding crosses a boundary.

### Telegram → Bridge

```go
// Input
HandleMessageStreaming(
    ctx        context.Context,
    chatID     int64,
    userMsg    string,
    senderName string,              // "heartbeat", "scheduler", "relay", "" for user
    images     []bridge.ImageInfo,  // {Path, Width, Height, Size}
    pdfs       []bridge.PDFInfo,    // {Path, Size}
    onUpdate   process.StreamFunc,  // func(delta string)
)

// Output
bridge.AgentResponse {
    Text   string   // final text, artifacts stripped
    Photos []Photo  // collected images {Data []byte, Caption string}
}
```

### Bridge → Process

```go
// Input
Agent.Send(
    ctx      context.Context,
    req      process.AgentRequest,  // {ChatID, SessionID, Text, Images, PDFs, SystemPrompt}
    onUpdate process.StreamFunc,    // nil for no streaming
)

// Output
process.SendResult {
    Text      string      // raw Claude response
    SessionID string      // Claude session ID for future --resume
    ToolCalls []ToolCall   // tool invocations observed
}
```

### Process → Claude CLI

Bidirectional protocol via stdin/stdout JSON:
```
claude -p --input-format stream-json --output-format stream-json \
    --permission-mode bypassPermissions --mcp-config ~/.shell/mcp.json \
    [--setting-sources "user,project"]

stdin (SDK → CLI):
  1. {"type":"control_request", "request":{"subtype":"initialize"}}
  2. {"type":"user", "message":{"role":"user", "content":"..."}}
  3. {"type":"control_response", ...}  (in response to CLI permission checks)

stdout (CLI → SDK):
  - system/init → session_id
  - stream_event → text deltas (onUpdate callback)
  - assistant → tool_use blocks (ToolCall extraction)
  - control_request → auto-allow (can_use_tool)
  - result → final text, terminates the turn
  - user (non tool_result) → a turn the CLI started itself (e.g. a background
    Agent subagent's <task-notification>); logged as a turn boundary
```

The parser keeps every assistant text block (`SendResult.Text`) and the same
text split at tool_use boundaries (`SendResult.TextSegments`). What a person
receives is decided in the bridge (`reply_text.go`): a short segment (≤160
bytes, and shorter than what follows it) that precedes a tool call is an
aside — "Let me check the schedule first", or the agent reciting a self-check
— and is dropped from replies bound for a real chat and logged; long pre-tool
prose, a short answer followed by "Memory saved", and anything after the last
tool call stay. Journal turns (heartbeats, anything on the system chat) keep
the full narrative. The destination decides, not the sender: a prompt-mode
schedule is system-initiated but replies into a real chat, so it is filtered.
Before this (2026-09-17) 2.8% of replies opened with such an aside. A turn
that used tools but produced no text is summarised ("✓ Bash ×2") only for
journal turns; a real chat gets the normal empty-reply handling instead.

Persistent processes (one per chat/thread) are read by a dedicated reader
goroutine and a turn pump, never by the sender: a turn the CLI produces on
its own between messages (a background subagent finishing after the agent
already replied) is parsed as a whole and handed to
`Bridge.HandleUnsolicitedTurn`, which runs the normal response pipeline with
source `followup` and pushes it to the chat as a follow-up message. Before
this (2026-09-05) such a turn sat in the pipe and the next user message
consumed it as its answer, shifting every reply one message behind. A send
whose caller context ends mid-turn returns `ErrTurnAbandoned`; the process is
kept, no fallback subprocess is spawned, and the late result also arrives as a
follow-up.

Environment variables set on Claude subprocess:
- `SHELL_CHAT_ID` — current Telegram chat ID
- `SHELL_BRIDGE_SOCK` — path to RPC Unix socket

## Security & Access Control

Multi-layered auth inspired by OpenClaw's pairing model. Fail-closed by default.

### DM Policy (`dm_policy` in config)

| Policy | Behavior |
|--------|----------|
| `allowlist` (default) | Only config `allowed_users` + dynamic allowlist |
| `pairing` | Unknown senders get an 8-char code; admin approves via CLI |
| `disabled` | All DMs denied (except config users) |

### Group Policy (`group_policy` in config)

| Policy | Behavior |
|--------|----------|
| `disabled` (default) | All group messages denied (except config users) |
| `allowlist` | Only `group_allowed_users` + dynamic allowlist |
| `pairing` | Unknown group senders get a pairing code; admin approves via CLI |

### Auth Decision Flow

```
Incoming message
  │
  ├─ Config user (allowed_users)? → ALLOW
  │
  ├─ Group message?
  │   ├─ group_policy=disabled → DENY
  │   ├─ In group_allowed_users? → ALLOW
  │   ├─ In dynamic allowlist? → ALLOW
  │   ├─ Rate limited? → SILENT DROP
  │   ├─ group_policy=pairing → PAIRING (send code)
  │   └─ → DENY
  │
  └─ DM message?
      ├─ dm_policy=disabled → DENY
      ├─ In dynamic allowlist? → ALLOW
      ├─ Rate limited? → SILENT DROP
      ├─ dm_policy=pairing → PAIRING (send code)
      └─ dm_policy=allowlist → DENY
```

### Pairing Flow

1. Unknown user messages bot → receives 8-char code (crypto-random, no ambiguous chars)
2. Admin runs `shell pairing approve <CODE>` → user added to `~/.shell/allowlist.json`
3. User's next message is authorized via dynamic allowlist
4. Codes expire after 10 minutes; max 20 pending requests

### Rate Limiting

In-memory sliding window: 5 attempts per 60 seconds per sender. Only applies to denied/pairing users. Rate-limited messages are silently dropped.

## Agent Interface

```go
type Agent interface {
    Send(ctx context.Context, req AgentRequest, onUpdate StreamFunc) (SendResult, error)
    Get(chatID int64) (*Session, bool)
    Register(sess *Session)
    Kill(chatID int64)
    KillAll()
    ActiveCount() int
    ListSessions() []Session
}

type AgentRequest struct {
    ChatID       int64
    SessionID    string            // claude session ID for --resume
    Text         string            // user message text
    Images       []ImageAttachment // {Path, Width, Height, Size}
    PDFs         []PDFAttachment   // {Path, Size}
    SystemPrompt string
}

type SendResult struct {
    Text      string
    SessionID string
    ToolCalls []ToolCall // tool calls observed
}
```

The process manager implements this interface using the bidirectional stdin/stdout JSON protocol.
Auto-retry on resume failure: falls back to fresh session.

## SQLite Schema

| Table | Key Columns | Purpose |
|-------|-------------|---------|
| `sessions` | chat_id, claude_session_id, status, created_at, updated_at | One session per chat |
| `messages` | session_id, role, content | Full conversation log |
| `message_map` | telegram_msg_id → session_id, user_content, bot_content | Reaction routing |
| `schedules` | chat_id, type, cron_expr, message, mode, next_run_at, enabled | Cron/once/heartbeat |
| `tasks` | chat_id, description, status, created_at | Background task queue |

## File Paths

| Path | Purpose |
|------|---------|
| `~/.shell/config.json` | Main configuration |
| `~/.shell/shell.db` | SQLite database |
| `~/.shell/shell.pid` | Daemon PID file |
| `~/.shell/mcp.json` | MCP config (auto-generated) |
| `~/.shell/bridge.sock` | RPC Unix socket |
| `~/.shell/pairing.json` | Pending pairing requests |
| `~/.shell/allowlist.json` | Approved users |
| `~/.shell/worktrees/` | Git worktree checkouts |
| `~/.shell/skills/` | Installed skills |

## Skills in the prompt

Skills are `SKILL.md` files in `~/.shell/skills/` (shared, installed from
`skills/`) and `~/.shell/agents/<agent>/skills/` (the agent's own). Tiers:
`core` (always full), `hot` (pre-loaded), `lazy` (one catalog line; the
agent reads the file on demand). A hot skill contributes only its
`<!-- hot -->` … `<!-- /hot -->` rules section when it has one, plus a
pointer to the full file. Hot skills are packed into `skill.HotTierBudget`
(3,200 tokens, estimated per rune class) with the agent's own skills first;
one that does not fit renders as a catalog line that says "hot, NOT loaded"
and is logged at startup and on reload (`skills: hot skills over the prompt
budget`). `status: draft` caps a skill to lazy until it graduates.

**Self-authored skills.** Each deep heartbeat carries a skill retrospective
(`bridge.buildSkillRetroBlock`): per skill, runs, failures, `SKILL.md` reads
and last use over 30 days, from the `tool_uses` log (`store.SkillUsage`), the
hot skills that are not loaded, and the agent's `playground/` drafts. The
agent may draft into `skills/playground/<name>/` (never loaded), graduate a
draft, change tiers or retire a skill. After every heartbeat the bridge
commits any change under the agent's own skills dir to the git repo holding
it (`~/.shell`), author = the agent, reloads skills, and sends one line with
the revert command to `agent.owner_chat_id` (0 = commit silently).
`USAGE.jsonl` changes alone never commit. No approval step: notify + revert.
`shell skills report [--days N] [--agent A]` shows, per agent, each skill's
owner, tier, whether it reaches the prompt, real usage, playground drafts and
the agent's self-authored commit count.

### Lanes (R1)

In a chat listed in `route.lane_chats` (off by default; `{"*": 0}` = every
chat, except a project's own forum topic and the system chat), each real turn is
routed **before** its session is chosen, by Jev with the v2 question and a
1.5 s timeout; on a timeout or error the thread keeps its previous lane.
- A **project lane** gets its own Claude session, on a negative "session
  thread" allocated in `lane_sessions`, and that project's scoped
  `[Project]` block.
- The **general lane** keeps the chat's existing session.

The turn keeps two thread ids. The session thread is used for the process
key, session row, rotation, prefix hash and compaction. The real thread is
used for delivery, transcript, message maps and the shadow. Acted-on
decisions are logged with `source = lane`.

## Weekly review and suggestions

Once a week (`review.cron`, default Wednesday 10:30), each agent that has
`agent.owner_chat_id` set runs a review. It is an `agent.review` event on the
durable queue (`internal/daemon/review.go`), with a schedule registered by
dedup key `agent:review`.

The daemon builds an evidence pack for the agent's last 7 days:
- OwnerEval counts.
- Reactions, which are logged to `feedback_events` whatever command they also
  trigger.
- Skill usage.
- The agent's suggestions: open ones, and ones decided this week together with
  the owner's words.
- Its older `loop:proposals` headlines.
- Its last reflections.

It then runs one system turn. The agent changes what it can itself and files
at most three suggestions with the `shell_suggestion` tool (a table in its
`shell.db`). The daemon sends the owner the agent's summary and the new
suggestions.

The owner answers in plain words in their DM. That chat's agent records the
answer with `shell_suggestion(action=decide)`, which is refused from any other
chat. The owner can also use `shell suggestions decide`. `shell suggestions
review-now` runs the review on the next tick. Design:
`docs/DESIGN-ROUTER-AND-SUGGESTIONS.md` (S0).

## Message router (R0: shadow)

`internal/route` decides a message's **lane**: one of the chat's active
projects, or `general`. The sticky rule (`route.sticky_threshold`, default
0.6) keeps an unsure switch in the thread's previous lane. There are two
backends:
- **jev**: the typed-decision model. Live, it reuses the Jev shadow's
  `which_project` answer through `decide.Shadow.OnWhichProject`, so there is
  no second call.
- **keyword**: a local baseline built from project title words.

Every real turn in a chat with projects logs one `route_decisions` row per
backend (`source = live`, with the Telegram message id). Nothing on the turn
path reads these rows yet.

Scoring:
- `shell route replay` re-routes the last N days of real user messages (from
  the agent's own `messages`), in thread order (`source = replay`).
- `shell route judge` labels those messages with a stronger model through the
  Claude CLI (`route.judge_model`).
- `shell route label` records the owner's override.
- The `shell_lane` tool lets the agent label the message it is answering.

Labels go in `route_labels`, keyed by chat, thread and text hash; no text is
stored. `shell route report` scores each backend against the strongest label,
next to the always-general baseline. Design:
`docs/DESIGN-ROUTER-AND-SUGGESTIONS.md` (R0).

## Shadow Router

When the `TYPESAFE_API_KEY` secret resolves (secret store, then
environment), every real user turn and peer relay — not heartbeats,
prewarm or scheduler turns — is also shown — after
its context is built, on its own goroutine with a 5 s deadline, fail-open —
to a decision model with a few typed questions: which active project is
this about (choice over slugs + `none`), on peer-agent turns whether to reply
at all, and whether the message holds an open question or a decision. The
state sent is the message text (clipped to 2,000 runes) and where it was
said; never the transcript or memory. Answers land in `router_decisions`
beside the facts needed to score them (the project whose own thread this
was, whether it was a peer turn, and the Telegram message id so each answer
can be checked against the words it judged). Nothing reads those rows on the
turn path.
Pass criteria and the verdict schedule: plan § P3.7.

## Progress Voice

While a turn runs, the placeholder message ticks every 2 s. Its wording comes
from `<workspace>/progress-phrases.json`, a file each agent owns and may
rewrite in its own voice (the environment prompt says so). The handler loads
it with an mtime check every 30 s, validates each phrase to one short plain
line, and falls back to built-in defaults per slot. Tool names map to
families (search / browse / memory / file / shell / default) so the screen
never shows a raw tool name.

## Reaction System

Emoji reactions on Telegram messages route to actions via `config.reaction_map`:

| Emoji | Action | Behavior |
|-------|--------|----------|
| 👍 | go | Approve plan / unblock |
| 👎 | stop | Reject plan / stop blocked task |
| ❌ | cancel | Cancel active plan |
| 📋 | status | Show session info |
| 🔄 | regenerate | Re-invoke Claude on original exchange |
| 📌 | remember | Store bot response to memory |
| 🗑 | forget | Delete exchange from log |
| 🔁 | retry | Retry blocked plan task |

Reactions work via `message_map` which links each Telegram message ID back to the original user/bot exchange and session.

## Planner Loop

```
/plan "goal"
  │
  ▼ Claude drafts plan (markdown task list)
  │
  ▼ User reacts 👍 (go) or 👎 (stop)
  │
  ▼ For each task:
  │   ├─ Create worktree branch (if enabled)
  │   ├─ Execute (Claude runs task)
  │   ├─ Test (run test_cmd)
  │   ├─ Review (Claude reviews diff + test output)
  │   └─ Verdict:
  │       ├─ done → next task
  │       ├─ needs_revision → retry (up to max_retries)
  │       └─ needs_human → block, await user reaction
  │
  ▼ Merge worktree → notify completion
```

Plan states: `idle` → `drafting` → `executing` → `done` (or `blocked` → resume).

Multi-repo support: tasks can target different repos, each with its own worktree.

## Heartbeat System

Periodic check-ins routed through Claude with full context:

1. Scheduler fires every N minutes (`/heartbeat 30m "Check inbox"`)
2. Bridge enriches with: recent exchanges, heartbeat insights, pending tasks, memory
3. Claude responds with awareness of conversation state
4. `[noop]` suppresses output when nothing to report
5. Claude uses `scripts/shell-remember --action heartbeat-learning` for insights
6. Claude uses `scripts/shell-task complete --id N` for task completion
7. Memory reflection runs after each heartbeat cycle

Every beat is an ephemeral turn: a fresh CLI spawn with no `--resume`. The
enrichment carries all the context a beat needs, and resuming the system
chat's session only replayed its growing history after the cache had lapsed
(~88k cache-creation tokens per hourly beat for a `[noop]`, vs ~30k cold).
The enrichment lists recent history once per chat, not once per session
row (a forum group has one session per thread).

Every Nth beat (`scheduler.deep_reflect_interval`) is a **deep reflection**
beat on the `heartbeat_deep` model. It carries extra context and is journaled
to the `reflections` table:

- **Pinned memory audit.** Two cuts, in order of consequence: the
  *system-prompt cut* (operating pins that did not fit `memory.system_budget`
  this generation — packed newest-created-first by `packOperatingPins`, the
  same helper the composed prompt uses, exposed via `SystemPromptPinCut`) and
  the *retrieval cut* (what `ghost_context` can surface under its
  importance-ranked sub-budget). The system-prompt half has no minimum: one
  dropped pin is a rule the agent lacks on conversation turns. The beat's first
  job is to shrink the pin set (merge / trim / unpin) until nothing is dropped.
- **Journal contract.** A deep beat with nothing to send writes `[noop]` on the
  first line, then a short journal. The bridge blanks any response containing
  `[noop]` before delivery, so the journal is recorded but never reaches a chat.
- The skill-inventory retro block is not injected: its usage meter is only
  written by the `run-skill` wrapper, which the agent bypasses.

Quiet hours (default 10 PM–7 AM) suppress heartbeat firing.

## Scheduler

Three schedule types:
- **cron** — recurring (5-field cron or aliases: @hourly, @daily, etc.)
- **once** — fire-and-forget at a specific time
- **heartbeat** — periodic check-in with enriched context

Three modes:
- **notify** — plain message sent to chat
- **prompt** — routed through Claude for reasoning
- **event** — not bound to chat delivery: the fire enqueues its payload on the
  task queue and is done; a consumer owns everything downstream (projects use
  this for research passes and the Notion poll)

1-minute tick loop checks `GetDueSchedules()` and fires matching entries.

## Projects

A **project** binds a living document, a chat (or forum topic), a research
schedule and a memory tag into one named unit. Design, decisions and phases:
`docs/PLAN-PROJECT-WORKSPACE.md`.

**Who does what.** The family talks in Telegram. The agent owns the doc:
it writes through the project skill (`doc-write` over RPC), each write is a
commit in the project's own git repo (`workspace/projects/<slug>/`), and the
commit hash is the receipt. A render task mirrors the new rev to a Notion
page, touching only the sections whose hash changed (the `block_map` records
section → block ids). A weekly **event** schedule fires one bounded research
pass per project; the consumer runs the turn and delivers a ≤3-line delta to
the project's chat. One shared poll job lists comments on every mapped block
and turns each unanswered human thread into an event; the agent fixes the doc
and replies in the thread.

**Two kinds of Notion binding.**

| | Rendered page | Adopted page |
|---|---|---|
| Made by | the renderer | a human |
| `block_map` | sections → blocks, with hashes | `adopted: true`, top-level block ids |
| Comments → events → in-thread reply | yes | yes |
| Rendered / reconciled | yes | **never** — the renderer speaks only headings, bullets and paragraphs and would erase tables and checkboxes |
| Page edit | reconciled into the doc as a human-edit commit | counts as human activity; block list re-read |
| Bound with | `project create` (skill) | `shell project adopt <slug> <url>` |

**Focus: what a turn sees.** In a project's own thread — a group forum topic
whose id is the project's `message_thread_id` — the turn gets a scoped
`[Project]` block: that project alone, plus the 決定 (decisions) and 待決定
(open questions) sections quoted from its doc. The thread id decides; nothing
is classified. Anywhere else the turn gets the chat-wide `[Projects]` list.
Two projects claiming one thread is ambiguous and falls back to the list.

**Needs you.** The pinned 📋 list marks each active project with `❓N`, the
number of open items under its doc's 待決定 section — bulleted or numbered,
not checked off, not struck through.

**Rules the write and poll paths enforce.**
- *Doc budget (24 KB) and log cap (8 KB).* The research prompt states size vs
  budget; `doc-write` refuses an agent write that leaves the doc, or its
  更新紀錄 section, over budget **and** larger than before. Shrinking is always
  accepted; human edits are never refused. 決定 is never cut.
- *Poll backoff.* A project with human activity, research, or creation in the
  last 7 days is swept every 30-minute tick; a quiet one every 6 hours
  (`notion_polled_at`, written without touching `updated_at`).
- *Stagger.* Research schedules register at minute `10 + (id mod 5) × 10`,
  never on the hour.
- *Lifecycle.* `active | paused | archived`. Archiving is an owner action,
  never automatic; it disables the research schedule and removes the project
  from the poll set, the pinned list and the agent's [Project] block.

## Memory

Powered by the `ghost` library for semantic memory:

- **Namespaces**: `shell:chat:CHAT_ID`, `shell:chat:CHAT_ID:heartbeat`
- **System namespaces**: always-on context (identity, ltm tiers)
- **Global namespaces**: cross-chat background context
- **Profiles**: per-chat config for budgets and namespaces

Key operations:
- `InjectContext()` — prepend relevant memories to user message
- `SystemPrompt()` — load always-on namespaces
- `LogExchange()` — store conversation for future recall
- `RunReflect()` — promote/decay/prune memories post-heartbeat

## Secrets

Design and evidence: `docs/DESIGN-SECRETS.md`. Secrets live in the
`shell-secrets` store (age-encrypted file, local identity file, no OS
keychain) and resolve through `config.Secret(name)`: store first, then the
environment. Config refers to secrets by **name** (`telegram.token_env`,
`notion.token_secret`); values are read at startup and handed to the
component that needs them — the Telegram client in-process, the Notion
token into the Notion MCP server's env only, the Jev key to the decider.

A Claude CLI child never inherits them. `process.Manager.childEnv` strips
every store-managed name, every configured reference and anything ending in
`_BOT_TOKEN` from the child environment, then adds `secrets.passthrough`
(default: what skill scripts read — `GEMINI_API_KEY`, `BRAVE_SEARCH_API_KEY`,
`TAVILY_API_KEY`, and `NOTION_TOKEN`, since the notion skill is a Bash
script) with their values. The planner's subprocesses use the same policy.
The lists are computed at daemon start; `claude.env` names bypass stripping
by operator choice. Exported
variables would otherwise survive every in-place restart, so stripping is
active on each spawn rather than a one-time omission. `shell secrets doctor`
reports each reference's source and the child-env policy without printing a
value; `shell-secrets doctor` covers the store itself.

## Configuration

`~/.shell/config.json` — all features are opt-in via flags:

```json
{
  "telegram": { "token_env", "allowed_users", "reaction_map" },
  "claude": { "binary", "model", "model_routing": { "conversation", "heartbeat", "heartbeat_deep", "compaction", "chat_models": { "<chat_id>": "<model>" } }, "timeout", "max_sessions", "work_dir", "allowed_tools", "disallowed_tools", "setting_sources" },
  "store": { "db_path" },
  "memory": { "enabled", "db_path", "budget", "profiles", "chat_profiles" },
  "planner": { "enabled", "test_cmd", "conventions", "max_retries", "worktree" },
  "scheduler": { "enabled", "timezone", "quiet_hour_start", "quiet_hour_end" },
  "tunnel": { "enabled", "cloudflared_bin", "max_tunnels" },
  "pm": { "enabled", "max_procs", "log_lines" },
  "reload": { "enabled", "source_dir", "debounce" },
  "secrets": { "enabled", "store_path" }
}
```

## Daemon Initialization

`daemon.New(config)` wires everything in order:

1. Open secret store (if enabled)
2. Export secrets to env for child processes
3. Open SQLite store
4. Load skills from `~/.shell/skills/` and `.agent/skills/`
5. Merge allowed-tools (config + skills + MCP auto-approve)
6. Write MCP config to `~/.shell/mcp.json`
7. Create process manager (with MCP config path + bridge socket path)
8. Initialize memory store (if enabled)
9. Initialize planner (if enabled)
10. Initialize tunnel manager (if enabled)
11. Initialize process manager (if enabled)
12. Create bridge with all components
13. Create Telegram bot
14. Wire async callbacks (notifier, cron parser)
15. Create RPC server for skill scripts + MCP
16. Initialize scheduler (if enabled)
17. Start reload watcher (if enabled)

`daemon.Run(ctx)` starts RPC server + Telegram long-poll + scheduler tick loop.

## CLI Commands

| Command | Description |
|---------|-------------|
| `shell init` | Create config dir and default config |
| `shell daemon [--watch]` | Start bot daemon (--watch for live reload) |
| `shell restart` | Send SIGHUP to running daemon |
| `shell stop` | Send SIGTERM to running daemon |
| `shell send "msg"` | One-shot test without Telegram |
| `shell status` | Show active sessions |
| `shell session list\|kill` | Session management |
| `shell search "query"` | Web search from CLI |
| `shell mcp` | MCP stdio server (spawned by Claude CLI) |
| `shell project list\|show\|archive\|bind\|adopt` | Owner ops on the project registry (`--config` selects the agent) |
