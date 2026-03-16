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
  - result → final text, terminates the event loop
```

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

Quiet hours (default 10 PM–7 AM) suppress heartbeat firing.

## Scheduler

Three schedule types:
- **cron** — recurring (5-field cron or aliases: @hourly, @daily, etc.)
- **once** — fire-and-forget at a specific time
- **heartbeat** — periodic check-in with enriched context

Two modes:
- **notify** — plain message sent to chat
- **prompt** — routed through Claude for reasoning

1-minute tick loop checks `GetDueSchedules()` and fires matching entries.

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

## Configuration

`~/.shell/config.json` — all features are opt-in via flags:

```json
{
  "telegram": { "token_env", "allowed_users", "reaction_map" },
  "claude": { "binary", "model", "timeout", "max_sessions", "work_dir", "allowed_tools", "setting_sources" },
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
