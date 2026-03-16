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
- `internal/mcp/` — MCP stdio server: exposes PM, tunnel, relay as native Claude tools
- `internal/rpc/` — HTTP-over-Unix-socket RPC for skill scripts and MCP server
- `internal/planner/` — Optional plan-execute-review loop
- `internal/reload/` — Live reload watcher (rebuild + syscall.Exec)
- `internal/worktree/` — Git worktree isolation for plan execution
- `internal/skill/` — Skill registry: loads `~/.shell/skills/` and `.agent/skills/` for system prompt
- `internal/scheduler/` — Cron/one-shot scheduler with SQLite persistence
- `cmd/shell-search/` — Standalone web search CLI (skill binary)
- `cmd/shell-imagen/` — Standalone image generation CLI (skill binary)
- `skills/` — Skill definitions (SKILL.md + scripts)

## Commands

- `shell init` — Create config directory and default config
- `shell daemon` — Start the bot daemon (`--watch` for live reload)
- `shell restart` — Send SIGHUP to running daemon (graceful restart)
- `shell stop` — Send SIGTERM to running daemon (graceful shutdown)
- `shell send "msg"` — One-shot test without Telegram
- `shell status` — Show active sessions
- `shell session list|kill <chat-id>` — Session management
- `shell pairing list|approve|allowlist|revoke` — Pairing and allowlist management
- `shell mcp` — MCP stdio server (spawned by Claude CLI, not run manually)

## Build & Test

```bash
make build           # Build binary
make test            # Run tests
make vet             # Run go vet
make watch           # Build and run with --watch
make skills          # Build skill binaries (web-search, generate-image)
make install-skills  # Build and install skills to ~/.shell/skills/
```

## Key Patterns

- Each Telegram message → `bridge.HandleMessageStreaming()` → `process.Agent.Send(AgentRequest, onUpdate)` → Claude CLI
- Bidirectional protocol: `--input-format stream-json --output-format stream-json` with stdin/stdout JSON control protocol
- Typed boundaries: `AgentRequest` (bridge→process), `SendResult` (process→bridge), `AgentResponse` (bridge→telegram)
- Response processing via `processResponse()`: collects `Photo`s from artifacts, logs exchange
- Sessions persist across restarts via SQLite
- Allowlist-based auth by Telegram user ID
- Streaming responses with live Telegram message edits
- Photo/image attachments: downloaded to temp files, sent as typed `ImageAttachment`/`PDFAttachment`
- Album support: multiple photos buffered with 500ms debounce, sent as single message
- PID file at `~/.shell/shell.pid` for restart/stop commands
- SIGHUP triggers graceful restart via syscall.Exec (same pattern as reload.go)
- Config: `~/.shell/config.json` with `allowed_tools` for auto-approving Claude CLI tools
- Emoji reactions map to actions (go, stop, cancel, status, regenerate, remember, forget, retry)
- Heartbeat: `/heartbeat <interval> <message>` — periodic check-in routed through Claude with session context (one per chat)
  - Quiet hours: heartbeats suppressed during configurable window (default 10 PM - 7 AM in scheduler timezone)
  - Proactive checks: heartbeat prompts Claude to check for anything needing attention
  - Memory reflection: includes memory context for heartbeat to reflect on stored knowledge
  - Background tasks: `/task add|list|done|delete` — queue tasks for heartbeat to pick up
  - Noop suppression: heartbeat responses with nothing to report are not sent to chat
  - Check-in messages: every ~4 heartbeats, a friendly check-in hint is included
- Scheduler config: `{"scheduler": {"enabled": true, "timezone": "UTC", "quiet_hour_start": 22, "quiet_hour_end": 7}}` in config.json

## Tool System (Three Layers)

### MCP Tools (first-class, bridge-internal)

Claude calls these directly as native tools via the MCP protocol — no Bash, no curl.
The daemon writes `~/.shell/mcp.json` and passes `--mcp-config` to Claude CLI.

| Tool | Description |
|------|-------------|
| `shell_pm` | Process manager: start, stop, list, logs, remove background processes |
| `shell_tunnel` | HTTP tunnels: start, stop, list via Cloudflare quick tunnels |
| `shell_relay` | Send messages/photos to other Telegram chats |

**NEVER run long-running processes directly via Bash** — always use `shell_pm`.

**Web app workflow:**
1. Write app files
2. `shell_pm(action="start", name="web", command="node server.js", dir="/path")` — starts in background
3. `shell_tunnel(action="start", port="8080")` — expose via public URL

Requires `"pm": {"enabled": true}` and `"tunnel": {"enabled": true}` in config.
Cloudflared must be installed (`brew install cloudflared`).

### Skill Scripts (Bash via RPC)

Skills are pluggable capabilities loaded from `~/.shell/skills/` and `.agent/skills/`.
Each skill has a `SKILL.md` (frontmatter + instructions) and optional `scripts/` directory.
Skills inject their instructions into the system prompt and declare allowed tools.
Skill scripts call the bridge RPC server on `~/.shell/bridge.sock` via curl.

| Skill | Description |
|-------|-------------|
| `shell-schedule` | Create one-shot or cron schedules via RPC |
| `shell-remember` | Store memories and heartbeat learnings via RPC |
| `shell-task` | Mark background tasks complete via RPC |
| `web-search` | Web search via Brave/Tavily APIs |
| `generate-image` | Image generation via Google Gemini |
| `browser` | Headless Chrome automation |

### Artifact Markers (text-based, passive)

Skills output `[artifact type="image" path="..." caption="..."]` markers that the bridge
picks up and sends as Telegram photos. `[noop]` suppresses heartbeat output.

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
