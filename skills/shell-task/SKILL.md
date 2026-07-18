---
name: shell-task
description: Create, complete, and manage tasks (self-decomposition and agent delegation)
usage: "~/.shell/skills/shell-task/scripts/shell-task create --to self --description \"research X\""
allowed-tools: Bash
core: true
tier: core
---

**Canonical invocation:** the script lives at the ABSOLUTE path
`~/.shell/skills/shell-task/scripts/shell-task` — works from any cwd; never
guess repo-relative paths or probe with ls/head.

# Task Management

Create, delegate, and complete tasks. Use for:
- **Self-decomposition**: Break complex requests into smaller steps
- **Agent delegation**: Ask a peer agent to verify, review, or handle something
- **Background work**: Queue tasks for later processing

## Commands

```bash
# Create a self-task (you'll process it yourself)
~/.shell/skills/shell-task/scripts/shell-task create --to self --description "step 1: research medication interactions"

# Delegate to a peer agent
~/.shell/skills/shell-task/scripts/shell-task create --to umbreon_mini_bot --description "verify this health advice" --context "I told the user ibuprofen is safe with Flonase"

# Create with a goal ID (links related tasks)
~/.shell/skills/shell-task/scripts/shell-task create --to self --description "step 2: summarize findings" --goal abc123

# Complete a task with result
~/.shell/skills/shell-task/scripts/shell-task complete --id abc123def4 --result "ibuprofen is safe with Flonase, no interactions"

# Mark a task as failed
~/.shell/skills/shell-task/scripts/shell-task fail --id abc123def4 --reason "could not verify, source unavailable"

# List your pending tasks
~/.shell/skills/shell-task/scripts/shell-task list

# Check a specific task's status
~/.shell/skills/shell-task/scripts/shell-task status --id abc123def4
```

## Options

- `--to <agent|self>` — target agent (bot username or "self" for yourself)
- `--description "..."` — what needs to be done (required for create)
- `--context "..."` — brief context to help the target agent understand (optional)
- `--goal <id>` — link to a parent goal/task for traceability (optional)
- `--id <hex>` — task ID (required for complete/fail/status)
- `--result "..."` — task result (required for complete)
- `--reason "..."` — failure reason (required for fail)
- `--chat <id>` — override chat ID (use when running from heartbeat where SHELL_CHAT_ID=0)

## When to Use

Before diving into a complex request, consider:
1. Should I break this into subtasks? → `create --to self`
2. Would a peer agent add value (verification, different perspective)? → `create --to <peer>`
3. Can I handle it in one step? → Just do it, no task needed

Don't over-decompose simple requests. Tasks are for multi-step or collaborative work.

## TTL — tasks are for ACTIVE work, not waiting

Every task auto-FAILS when its TTL expires (default **60 minutes**; the sweeper
runs continuously). Tasks are a hand-off/decomposition mechanism for work being
done NOW — never a timer. For "check again tomorrow", "in N hours", or any
real wait, use the shell-schedule skill instead (this failure happened twice
on 7/15: a 24h re-check self-task silently expired at 60min). If a task
legitimately needs longer active work, pass a bigger `--ttl <minutes>` at
creation.
