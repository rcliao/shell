# teeny-relay

Telegram Bot to Claude Code CLI bridge. One Claude Code session per Telegram chat, persisted in SQLite.

## Architecture

- `cmd/relay/main.go` — Cobra CLI entrypoint
- `internal/config/` — JSON config from ~/.teeny-relay/config.json
- `internal/store/` — SQLite persistence (sessions + message log)
- `internal/process/` — Claude CLI subprocess lifecycle
- `internal/bridge/` — Core routing: Telegram ↔ Claude Code
- `internal/telegram/` — Bot wrapper, handlers, auth, photo download
- `internal/daemon/` — Daemon lifecycle, PID file, signal handling
- `internal/memory/` — Optional memory store integration (agent-memory)
- `internal/planner/` — Optional plan-execute-review loop
- `internal/reload/` — Live reload watcher (rebuild + syscall.Exec)
- `internal/worktree/` — Git worktree isolation for plan execution
- `internal/scheduler/` — Cron/one-shot scheduler with SQLite persistence

## Commands

- `relay init` — Create config directory and default config
- `relay daemon` — Start the bot daemon (`--watch` for live reload)
- `relay restart` — Send SIGHUP to running daemon (graceful restart)
- `relay stop` — Send SIGTERM to running daemon (graceful shutdown)
- `relay send "msg"` — One-shot test without Telegram
- `relay status` — Show active sessions
- `relay session list|kill <chat-id>` — Session management

## Build & Test

```bash
make build    # Build binary
make test     # Run tests
make vet      # Run go vet
make watch    # Build and run with --watch
```

## Key Patterns

- Each Telegram message spawns: `claude -p "msg" --resume <sid> --output-format stream-json`
- Sessions persist across restarts via SQLite
- Allowlist-based auth by Telegram user ID
- Streaming responses with live Telegram message edits
- Photo/image attachments: downloaded to temp files, passed as file path references in prompt
- Album support: multiple photos buffered with 500ms debounce, sent as single message
- PID file at `~/.teeny-relay/relay.pid` for restart/stop commands
- SIGHUP triggers graceful restart via syscall.Exec (same pattern as reload.go)
- Config: `~/.teeny-relay/config.json` with `allowed_tools` for auto-approving Claude CLI tools
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
