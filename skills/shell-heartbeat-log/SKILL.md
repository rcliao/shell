---
name: shell-heartbeat-log
description: Inspect recent agent-level heartbeat reflection activity from the system chat
usage: scripts/shell-heartbeat-log [--limit N] [--full]
allowed-tools: Bash
core: true
tier: hot
---

# Heartbeat Log Inspector

Heartbeat reflection runs in a phantom system chat (chat_id 0) — it's the agent's "inner monologue." Output is suppressed by default; you only see it when explicitly relayed.

Use this skill to inspect what the agent has been doing during heartbeats — useful when a user asks "what have you been thinking about" or "show me your recent reflections."

## Usage

```bash
# Last 10 heartbeat exchanges (default)
scripts/shell-heartbeat-log

# Last 25 exchanges
scripts/shell-heartbeat-log --limit 25

# Full content (no truncation)
scripts/shell-heartbeat-log --full
```

## What it shows

- Time of each heartbeat (regular `[Heartbeat]` vs deep `[Heartbeat:deep]`)
- Whether the heartbeat was a noop or generated output
- Summary of any behavioral learnings, memory consolidations, or relays performed

The SHELL_DB_PATH environment variable is used automatically (set by the daemon).
