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
