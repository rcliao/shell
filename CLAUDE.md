# shell

Telegram Bot to Claude Code CLI bridge. One Claude Code session per Telegram chat, persisted in SQLite.

Layout, commands and build targets: `docs/ARCHITECTURE.md`, `shell --help`, `Makefile`.

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
