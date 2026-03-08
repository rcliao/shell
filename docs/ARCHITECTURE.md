# Shell Architecture

Telegram Bot ↔ Claude Code CLI bridge. One Claude Code session per Telegram chat, persisted in SQLite. Sessions survive daemon restarts.

## System Layers

```
┌─────────────────────────────────────────────────┐
│  Telegram (transport)                           │
│  long-poll, reactions, photos, albums, commands │
├─────────────────────────────────────────────────┤
│  Bridge (routing)                               │
│  directive parsing, memory injection, callbacks │
├──────────────┬──────────────────────────────────┤
│  Process Mgr │  Planner   │  Scheduler          │
│  sessions,   │  execute → │  cron, once,        │
│  streaming,  │  test →    │  heartbeat,         │
│  resume      │  review    │  quiet hours        │
├──────────────┴────────────┴─────────────────────┤
│  Store (SQLite)     │  Memory (ghost)           │
│  sessions, messages │  semantic search,         │
│  message_map,       │  namespaces, tiers,       │
│  schedules, tasks   │  exchange logging         │
├─────────────────────┴───────────────────────────┤
│  Utilities                                      │
│  search, browser, imagen, tunnel, pm, worktree  │
└─────────────────────────────────────────────────┘
```

## Package Map

| Package | Path | Purpose |
|---------|------|---------|
| **main** | `cmd/shell/main.go` | Cobra CLI: daemon, send, status, session, pairing, restart, stop, init, search |
| **bridge** | `internal/bridge/` | Core routing: Telegram ↔ Claude. Directive parsing, command handling, reaction routing |
| **process** | `internal/process/` | Claude CLI subprocess lifecycle. Agent interface, session management, streaming |
| **telegram** | `internal/telegram/` | Bot wrapper, handlers, policy-based auth, pairing, rate limiting, allowlist, photo/PDF download, MarkdownV2 formatting |
| **store** | `internal/store/` | SQLite persistence: sessions, messages, message_map, schedules, tasks |
| **config** | `internal/config/` | JSON config from `~/.shell/config.json` with all feature flags |
| **daemon** | `internal/daemon/` | Initialization chain, PID file, signal handling, component wiring |
| **memory** | `internal/memory/` | Semantic memory via ghost library. Namespaces, profiles, exchange logging |
| **planner** | `internal/planner/` | Plan execution: execute → test → review → decide (done/retry/blocked) |
| **scheduler** | `internal/scheduler/` | Cron/one-shot/heartbeat scheduler with quiet hours and noop suppression |
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
  │ inject memory context + system prompt
  │ inject current time, sender identity
  ▼
Process Manager
  │ spawn: claude -p "..." --resume <sid> --output-format stream-json
  │ parse stream events → onUpdate callback → live Telegram edits
  ▼
Bridge (response processing)
  ├─ [search query="..."]      → search API → re-prompt with results
  ├─ [browser url="..."]       → headless Chrome → re-prompt with results
  ├─ [pm cmd="..."]            → background process → re-prompt with status
  ├─ [tunnel port="..."]       → cloudflared → re-prompt with URL
  ├─ [relay to=CHAT_ID]        → send to another chat
  ├─ [schedule cron="..."]     → save to store
  ├─ [remember]...[/remember]  → store to memory namespace
  ├─ [generate-image]          → Imagen API → send photo
  ├─ [noop]                    → suppress heartbeat output
  ├─ [heartbeat-learning]      → store insight to heartbeat NS
  └─ [task-complete id=N]      → mark background task done
  │
  │ log exchange to store + memory
  ▼
Telegram Bot
  │ SendText (split + MarkdownV2) / SendPhoto
  ▼
User (Telegram)
```

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

### Persistence

| File | Purpose |
|------|---------|
| `~/.shell/allowlist.json` | Pairing-approved users (file-locked) |
| `~/.shell/pairing.json` | Pending pairing requests (file-locked, cross-process) |
| `config.json:allowed_users` | Static super-admin list |
| `config.json:group_allowed_users` | Static group allowlist |

### CLI Commands

```
shell pairing list       # Show pending pairing requests
shell pairing approve X  # Approve a pairing code
shell pairing allowlist  # List dynamically approved users
shell pairing revoke ID  # Revoke a user from dynamic allowlist
```

## Agent Interface

```go
type Agent interface {
    Send(ctx, chatID, claudeSessionID, message, systemPrompt) → SendResult
    SendStreaming(ctx, chatID, claudeSessionID, message, systemPrompt, onUpdate) → SendResult
    Get(chatID) → *Session
    Register(chatID, claudeSessionID)
    Kill(chatID)
    KillAll()
}
```

The process manager implements this interface. Claude CLI is invoked as:
```
claude -p "message" --resume <session_id> --output-format stream-json \
  --append-system-prompt "..." --allowedTools "tool1,tool2" ...
```

Auto-retry on resume failure: falls back to fresh session.

## SQLite Schema

| Table | Key Columns | Purpose |
|-------|-------------|---------|
| `sessions` | chat_id, claude_session_id, status, created_at, updated_at | One session per chat |
| `messages` | session_id, role, content | Full conversation log |
| `message_map` | telegram_msg_id → session_id, user_content, bot_content | Reaction routing |
| `schedules` | chat_id, type, cron_expr, message, mode, next_run_at, enabled | Cron/once/heartbeat |
| `tasks` | chat_id, description, status, created_at | Background task queue |

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
5. `[heartbeat-learning]` stores insights for future heartbeats
6. `[task-complete id=N]` marks background tasks done
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
- `ParseMemoryDirectives()` — extract `[remember]` blocks from response
- `RunReflect()` — promote/decay/prune memories post-heartbeat

## Configuration

`~/.shell/config.json` — all features are opt-in via flags:

```json
{
  "telegram": { "token_env", "allowed_users", "reaction_map" },
  "claude": { "binary", "model", "timeout", "max_sessions", "work_dir", "allowed_tools" },
  "store": { "db_path" },
  "memory": { "enabled", "db_path", "budget", "profiles", "chat_profiles" },
  "planner": { "enabled", "test_cmd", "conventions", "max_retries", "worktree" },
  "scheduler": { "enabled", "timezone", "quiet_hour_start", "quiet_hour_end" },
  "browser": { "enabled", "headless", "timeout_seconds" },
  "tunnel": { "enabled", "cloudflared_bin", "max_tunnels" },
  "pm": { "enabled", "max_procs", "log_lines" },
  "reload": { "enabled", "source_dir", "debounce" },
  "secrets": { "enabled", "store_path" }
}
```

## Daemon Initialization

`daemon.New(config)` wires everything in order:

1. Open secret store (if enabled)
2. Export search API keys to env
3. Open SQLite store
4. Create process manager
5. Initialize memory store (if enabled)
6. Initialize planner (if enabled)
7. Create bridge with all components
8. Create Telegram bot
9. Wire async callbacks (image sender, chat action, notifier)
10. Initialize scheduler (if enabled)
11. Initialize tunnel manager (if enabled)
12. Initialize process manager (if enabled)
13. Start reload watcher (if enabled)

`daemon.Run(ctx)` starts Telegram long-poll + scheduler tick loop.

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
