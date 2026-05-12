---
name: shell-pm
description: Manage background processes (start, stop, list, logs)
usage: scripts/shell-pm <start|stop|list|logs> [--name N] [--command CMD] [--dir D]
allowed-tools: Bash
---

# Process Manager

Manage background processes like web servers and watchers. **CRITICAL:** NEVER run long-running processes directly via Bash — always use this skill.

## Usage

```bash
# Start a process
scripts/shell-pm start --name myserver --cmd "node server.js" --dir /path/to/app

# List all processes
scripts/shell-pm list

# View process logs
scripts/shell-pm logs --name myserver

# Stop a process
scripts/shell-pm stop --name myserver

# Remove a stopped process
scripts/shell-pm remove --name myserver
```

## Web app workflow

1. Write app files
2. `scripts/shell-pm start --name web --cmd "node server.js" --dir /path/to/app`
3. Use tunnel skill to expose via public URL
