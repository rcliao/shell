# shell

Telegram Bot to Claude Code CLI bridge. One Claude Code session per Telegram chat, persisted in SQLite.

## Architecture

- `cmd/shell/main.go` — Cobra CLI entrypoint
- `internal/config/` — JSON config from ~/.shell/config.json
- `internal/store/` — SQLite persistence (sessions + message log)
- `internal/process/` — Claude CLI subprocess lifecycle
- `internal/bridge/` — Core routing: Telegram ↔ Claude Code
- `internal/telegram/` — Bot wrapper, handlers, auth, photo download
- `internal/daemon/` — Daemon lifecycle, PID file, signal handling
- `internal/memory/` — Optional memory store integration (ghost)
- `internal/planner/` — Optional plan-execute-review loop
- `internal/reload/` — Live reload watcher (rebuild + syscall.Exec)
- `internal/worktree/` — Git worktree isolation for plan execution
- `internal/skill/` — Skill registry: loads `.claude/skills/` for system prompt
- `internal/scheduler/` — Cron/one-shot scheduler with SQLite persistence

## Commands

- `shellinit` — Create config directory and default config
- `shelldaemon` — Start the bot daemon (`--watch` for live reload)
- `shellrestart` — Send SIGHUP to running daemon (graceful restart)
- `shellstop` — Send SIGTERM to running daemon (graceful shutdown)
- `shellsend "msg"` — One-shot test without Telegram
- `shellstatus` — Show active sessions
- `shellsession list|kill <chat-id>` — Session management
- `shellpairing list|approve|allowlist|revoke` — Pairing and allowlist management

## Build & Test

```bash
make build    # Build binary
make test     # Run tests
make vet      # Run go vet
make watch    # Build and run with --watch
```

## Key Patterns

- Each Telegram message → `bridge.HandleMessageStreaming()` → `process.Agent.SendStreaming(AgentRequest)` → Claude CLI
- Default mode: `claude -p "msg" --resume <sid> --output-format stream-json`
- Bidirectional mode (`claude.bidirectional: true`): `--input-format stream-json` with stdin/stdout JSON protocol
- Typed boundaries: `AgentRequest` (bridge→process), `SendResult` (process→bridge), `AgentResponse` (bridge→telegram)
- Response processing via `processResponse()`: strips directives, collects `Photo`s, logs exchange
- Sessions persist across restarts via SQLite
- Allowlist-based auth by Telegram user ID
- Streaming responses with live Telegram message edits
- Photo/image attachments: downloaded to temp files, sent as typed `ImageAttachment`/`PDFAttachment`
- Album support: multiple photos buffered with 500ms debounce, sent as single message
- PID file at `~/.shell/shell.pid` for restart/stop commands
- SIGHUP triggers graceful restart via syscall.Exec (same pattern as reload.go)
- Config: `~/.shell/config.json` with `allowed_tools` for auto-approving Claude CLI tools
- Emoji reactions map to actions (go, stop, cancel, status, regenerate, remember, forget, retry)
- Scheduler: `/schedule add|list|delete|enable|pause` commands + `[schedule]` response directive for Claude-initiated scheduling
- Heartbeat: `/heartbeat <interval> <message>` — periodic check-in routed through Claude with session context (one per chat)
  - Quiet hours: heartbeats suppressed during configurable window (default 10 PM - 7 AM in scheduler timezone)
  - Proactive checks: heartbeat prompts Claude to check for anything needing attention
  - Memory reflection: includes memory context for heartbeat to reflect on stored knowledge
  - Background tasks: `/task add|list|done|delete` — queue tasks for heartbeat to pick up
  - Noop suppression: heartbeat responses with nothing to report are not sent to chat
  - Check-in messages: every ~4 heartbeats, a friendly check-in hint is included
- Scheduler config: `{"scheduler": {"enabled": true, "timezone": "UTC", "quiet_hour_start": 22, "quiet_hour_end": 7}}` in config.json

## Web Search

Web search is built-in via the `[search]` directive. The bridge handles it automatically —
no Bash or external tools needed. Results are fetched and fed back for you to use.

```
[search query="your search query"]
[search query="recent topic" count="5" freshness="pw"]
```

## Browser Automation

Built-in browser automation via the `[browser]` directive. The bridge launches a headless Chrome
instance, executes the actions, and feeds results back for you to use.

```
[browser url="https://example.com"]
click "#btn"
screenshot
extract ".content"
[/browser]
```

Actions: `navigate`, `click`, `type`, `wait`, `screenshot`, `extract`, `js`, `sleep`.
Screenshots are sent as photos to the chat. Extracted text and JS results are fed back for reasoning.
Requires `"browser": {"enabled": true}` in config.

## HTTP Tunnels

Expose local ports to the internet via Cloudflare quick tunnels using the `[tunnel]` directive.

```
[tunnel port="8080"]
[tunnel action="stop" port="8080"]
[tunnel action="list"]
```

- `port` — local port to expose (required for start/stop)
- `action` — `start` (default), `stop`, or `list`
- `protocol` — `http` (default) or `https`

Requires `"tunnel": {"enabled": true}` in config and `cloudflared` installed (`brew install cloudflared`).

## Process Manager

Use the `[pm]` directive to manage background processes. **NEVER** run long-running processes (servers, watchers) directly via Bash — use `[pm]` instead.

```
[pm name="myserver" cmd="node server.js" dir="/path/to/app"]
[pm action="list"]
[pm action="logs" name="myserver"]
[pm action="stop" name="myserver"]
[pm action="remove" name="myserver"]
```

- `name` — unique process name (required for start/stop/logs/remove)
- `cmd` — shell command (required for start)
- `dir` — working directory (optional)
- `action` — `start` (default when cmd provided), `stop`, `list`, `logs`, `remove`

Requires `"pm": {"enabled": true}` in config.

**Web app workflow:**
1. Write app files
2. `[pm name="web" cmd="node server.js" dir="/path/to/app"]` — starts in background
3. `[tunnel port="8080"]` — expose via public URL

## Available CLI Tools

See `TOOLS.md` for the full reference of CLI tools available via Bash. Read it when users request:
- Web research or summarization
- Creating or editing Google Docs, Sheets, Slides, or Forms
- Google Drive file management, sharing, or uploads
- Sending emails or managing calendar events
- Browser screenshots or web page interaction
- Downloading or converting media (video, audio, images)
- Document conversion (Markdown, PDF, DOCX, HTML)
- Data processing (CSV, JSON, spreadsheets)
- Translation
- QR code generation
- Any task that involves external services or file processing

Always use `--json --no-input --force` flags with `gog` for non-interactive scripting.
When creating Google artifacts, always return the shareable URL to the user.
