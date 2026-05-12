---
name: shell-tunnel
description: Expose local ports to the internet via Cloudflare tunnels
usage: scripts/shell-tunnel <start|stop|list> [--port N]
allowed-tools: Bash
---

# HTTP Tunnels

Expose local ports to the internet using Cloudflare quick tunnels.

## Usage

```bash
# Start a tunnel
scripts/shell-tunnel start --port 8080

# Start with HTTPS
scripts/shell-tunnel start --port 8080 --protocol https

# List active tunnels
scripts/shell-tunnel list

# Stop a tunnel
scripts/shell-tunnel stop --port 8080
```

Do NOT use Bash for cloudflared directly — always use this skill.
