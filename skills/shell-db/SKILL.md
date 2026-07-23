---
name: shell-db
description: Read-only queries against your own databases (sessions, schedules, memory, tasks) with correct paths and schema built in
usage: ~/.shell/skills/shell-db/scripts/shell-db query "SELECT ..." [shell|memory|tasks]
allowed-tools: Bash
core: true
tier: core
---

# Database inspection (read-only)

Query your own databases WITHOUT guessing paths or column names. The script
resolves the correct file for you and always opens read-only, so it can never
corrupt live data.

**Canonical invocation** — the script lives at the ABSOLUTE path
`~/.shell/skills/shell-db/scripts/shell-db` (works from any cwd):

```bash
~/.shell/skills/shell-db/scripts/shell-db dbs                          # where everything lives
~/.shell/skills/shell-db/scripts/shell-db tables shell                 # list tables
~/.shell/skills/shell-db/scripts/shell-db schema schedules             # exact columns
~/.shell/skills/shell-db/scripts/shell-db query "SELECT id, label, next_run_at, enabled FROM schedules WHERE enabled=1"
```

## Databases

- `shell` — your agent's `shell.db`: sessions, schedules, ledgers, usage
- `memory` — your agent's ghost `memory.db`
- `tasks` — the SHARED cross-agent task store (`~/.shell/shared/tasks.db`)

Do NOT query `~/.shell/shell.db` — that file is stale legacy data.

## Key schemas (so you don't guess)

- **schedules**: `id, chat_id, label, message, schedule, timezone, type` (once|cron)`, mode` (notify|prompt)`, next_run_at, last_run_at, enabled, created_at`
  — there is NO `name`, `cron`, `at`, or `next_run` column.
- **sessions**: `id, chat_id, thread_id, claude_session_id, status, updated_at, ...`
- **messages**: `id, session_id, role, content, created_at` (join sessions for chat_id)
- **tasks** (shared store): `id, from_agent, to_agent, description, status, created_at, ...`

## Rules

- This skill is for READING. To create/modify schedules use **shell-schedule**;
  to mutate tasks use **shell-task**. Never write these tables with raw SQL —
  hand-written timestamps have silently broken the scheduler before.
- Prefer `schema <table>` over guessing a column; a failed guess wastes a turn.
