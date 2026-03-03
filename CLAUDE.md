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
