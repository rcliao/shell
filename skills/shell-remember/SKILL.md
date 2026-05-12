---
name: shell-remember
description: Store memories, heartbeat learnings, and behavioral improvements
usage: scripts/shell-remember --action remember|heartbeat-learning|behavioral --content "..." [--kind semantic|procedural]
allowed-tools: Bash
core: true
---

# Memory Store

Store information to long-term memory for future recall.

## Usage

```bash
# Store a memory
scripts/shell-remember --content "User prefers concise responses"

# Store with specific kind
scripts/shell-remember --content "When deploying, always run tests first" --kind procedural

# Store a heartbeat learning
scripts/shell-remember --action heartbeat-learning --content "User is most active in mornings"

# Store a behavioral learning (how to respond better — injected into future conversations)
scripts/shell-remember --action behavioral --content "When mami asks about food storage, include safety timeframe" --kind procedural
```

## Options

- `--content <text>` — memory content (required)
- `--kind <semantic|episodic|procedural>` — memory kind (default: semantic)
- `--action <remember|heartbeat-learning|behavioral>` — type of memory (default: remember)

## Action Types

- `remember` — general fact/knowledge storage (semantic by default)
- `heartbeat-learning` — patterns discovered during heartbeat reflection (episodic)
- `behavioral` — specific behavior changes for how you interact with users (procedural, injected into future conversations)

The SHELL_CHAT_ID environment variable is used automatically.
