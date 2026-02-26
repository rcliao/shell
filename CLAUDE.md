# teeny-relay

Telegram Bot to Claude Code CLI bridge. One Claude Code session per Telegram chat, persisted in SQLite.

## Architecture

- `cmd/relay/main.go` — Cobra CLI entrypoint
- `internal/config/` — JSON config from ~/.teeny-relay/config.json
- `internal/store/` — SQLite persistence (sessions + message log)
- `internal/process/` — Claude CLI subprocess lifecycle
- `internal/bridge/` — Core routing: Telegram ↔ Claude Code
- `internal/telegram/` — Bot wrapper, handlers, auth
- `internal/daemon/` — Daemon lifecycle, signal handling

## Commands

- `relay init` — Create config directory and default config
- `relay daemon` — Start the bot daemon
- `relay send "msg"` — One-shot test without Telegram
- `relay status` — Show active sessions
- `relay session list|kill <chat-id>` — Session management

## Build & Test

```bash
make build    # Build binary
make test     # Run tests
make vet      # Run go vet
```

## Key Patterns

- Each Telegram message spawns: `claude -p "msg" --continue --session-id <sid> --output-format text`
- Sessions persist across restarts via SQLite
- Allowlist-based auth by Telegram user ID
- Configurable timeout (default 5m) and max concurrent sessions (default 4)

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
