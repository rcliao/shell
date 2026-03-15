---
name: shell-remember
description: Store memories and heartbeat learnings
allowed-tools: Bash
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
```

## Options

- `--content <text>` — memory content (required)
- `--kind <semantic|episodic|procedural>` — memory kind (default: semantic)
- `--action <remember|heartbeat-learning>` — type of memory (default: remember)

The SHELL_CHAT_ID environment variable is used automatically.
