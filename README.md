# teeny-relay

Telegram Bot to Claude Code CLI bridge. Chat with Claude Code from Telegram.

## Setup

1. Create a Telegram bot via [@BotFather](https://t.me/BotFather)
2. Initialize config:
   ```bash
   relay init
   ```
3. Set your bot token:
   ```bash
   export TELEGRAM_BOT_TOKEN="your-token-here"
   ```
4. Edit `~/.teeny-relay/config.json` — add your Telegram user ID to `allowed_users`:
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
   relay daemon
   ```

## Commands

### CLI
- `relay init` — Create config directory and default config
- `relay daemon [-v]` — Start the Telegram bot daemon
- `relay send "message"` — One-shot test without Telegram
- `relay status` — Show active sessions
- `relay session list` — List all sessions
- `relay session kill <chat-id>` — Kill a session

### Telegram Bot
- `/start` — Initialize and show welcome message
- `/new` — Reset session, start fresh conversation
- `/status` — Show current session info
- `/help` — Show available commands

## Build

```bash
make build
```

## How it works

Each Telegram chat gets its own Claude Code session. Messages are forwarded to:
```
claude -p "message" --continue --session-id <sid> --output-format text
```

Sessions persist across restarts via SQLite. The `--session-id` flag maintains conversation context.
