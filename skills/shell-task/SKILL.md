---
name: shell-task
description: Complete background tasks from heartbeat
allowed-tools: Bash
---

# Task Management

Mark background tasks as completed.

## Usage

```bash
# Complete a task
scripts/shell-task complete --id 42
```

## Options

- `--id <number>` — task ID to complete (required)

The SHELL_CHAT_ID environment variable is used automatically.
