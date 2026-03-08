# shell

Claude Code CLI orchestration layer. Telegram bridge, scheduling, planning, memory, and browser automation.

Part of the [Ghost in the Shell](https://github.com/rcliao?tab=repositories&q=shell-) ecosystem.

## Setup

1. Create a Telegram bot via [@BotFather](https://t.me/BotFather)
2. Initialize config:
   ```bash
   shell init
   ```
3. Set your bot token:
   ```bash
   export TELEGRAM_BOT_TOKEN="your-token-here"
   ```
4. Edit `~/.shell/config.json` — add your Telegram user ID to `allowed_users`:
   ```json
   {
     "telegram": {
       "token_env": "TELEGRAM_BOT_TOKEN",
       "allowed_users": [123456789]
     }
   }
   ```
   (Find your user ID by messaging [@userinfobot](https://t.me/userinfobot))
5. Start the daemon:
   ```bash
   shell daemon
   ```

## Features

- **Telegram bridge** — Each chat gets its own persistent Claude Code session
- **Memory** — Semantic memory via [ghost](https://github.com/rcliao/ghost) with per-chat profiles and context injection
- **Scheduling** — Cron and one-shot schedules with quiet hours and heartbeat check-ins
- **Planning** — Execute-test-review loop with git worktree isolation
- **Browser** — Headless Chrome automation via [shell-browser](https://github.com/rcliao/shell-browser)
- **Image generation** — AI image creation via [shell-imagen](https://github.com/rcliao/shell-imagen)
- **Web search** — Built-in Brave/Tavily/DuckDuckGo search
- **Tunnels** — Expose local ports via [shell-tunnel](https://github.com/rcliao/shell-tunnel)
- **Secrets** — Encrypted secret store via [shell-secrets](https://github.com/rcliao/shell-secrets)
- **Streaming** — Live message edits as Claude responds
- **Albums** — Multi-photo support with 500ms debounce
- **Reactions** — Emoji-triggered actions (go, stop, cancel, status, retry)

## Commands

### CLI
- `shell init` — Create config directory and default config
- `shell daemon [--watch]` — Start the Telegram bot daemon
- `shell send "message"` — One-shot test without Telegram
- `shell status` — Show active sessions
- `shell session list|kill <chat-id>` — Session management
- `shell restart` — Send SIGHUP to running daemon
- `shell stop` — Send SIGTERM to running daemon
- `shell search "query"` — Web search from CLI

### Telegram Bot
- `/start` — Initialize and show welcome message
- `/new` — Reset session, start fresh conversation
- `/status` — Show current session info
- `/help` — Show available commands
- `/plan` — Start a plan execution
- `/schedule add|list|delete|enable|pause` — Manage schedules
- `/heartbeat <interval> <message>` — Set up periodic check-ins
- `/imagine <prompt>` — Generate an image

## Build

```bash
make build    # Build binary
make test     # Run tests
make vet      # Run go vet
make watch    # Build and run with --watch for live reload
```

## How It Works

Each Telegram chat gets its own Claude Code session. Messages are forwarded to:
```
claude -p "message" --resume <session-id> --output-format stream-json
```

Claude's responses are parsed for directives (`[search]`, `[browser]`, `[tunnel]`, `[generate-image]`, `[schedule]`, `[relay]`) which are executed and fed back for follow-up reasoning.

Sessions persist across restarts via SQLite. Memory context is injected via `--append-system-prompt`.

## Ecosystem

| Repo | Role |
|------|------|
| [ghost](https://github.com/rcliao/ghost) | Persistent agent memory (the "ghost" — consciousness/personality) |
| **shell** | Main orchestration app (the "shell" — vessel/runtime) |
| [shell-browser](https://github.com/rcliao/shell-browser) | Headless Chrome automation |
| [shell-imagen](https://github.com/rcliao/shell-imagen) | Image generation via Gemini |
| [shell-search](https://github.com/rcliao/shell-search) | Web search CLI |
| [shell-secrets](https://github.com/rcliao/shell-secrets) | Encrypted secret store |
| [shell-tunnel](https://github.com/rcliao/shell-tunnel) | HTTP tunnels via cloudflared |

## License

MIT
