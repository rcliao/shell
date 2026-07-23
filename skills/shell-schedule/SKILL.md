---
name: shell-schedule
description: Create one-shot or recurring scheduled reminders
usage: ~/.shell/skills/shell-schedule/scripts/shell-schedule once --at "ISO8601" --message "..." [--tz TZ --mode notify|prompt]
allowed-tools: Bash
core: true
tier: core
---

# Scheduler

Create one-shot or recurring scheduled messages/prompts.

**Canonical invocation — copy this shape exactly.** The script lives at the
ABSOLUTE path `~/.shell/skills/shell-schedule/scripts/shell-schedule` (works
from any cwd — never guess repo-relative paths, never `source` it, never
probe with ls/head). `SHELL_CHAT_ID` is already set in your environment for
the current chat; only override it to schedule for a DIFFERENT chat.

```bash
~/.shell/skills/shell-schedule/scripts/shell-schedule once --at "2026-07-20T09:00:00" --tz "America/Los_Angeles" --message "..." --mode prompt
```

Verify after creating: the script prints `Schedule #<id> created` — if you
did not see that line, the schedule does NOT exist; re-read the error and
retry rather than assuming success.

## Usage

```bash
# One-shot reminder
~/.shell/skills/shell-schedule/scripts/shell-schedule once --at "2024-03-15T09:00:00" --message "Team standup reminder"

# Recurring cron schedule
~/.shell/skills/shell-schedule/scripts/shell-schedule cron --expr "0 9 * * 1-5" --message "Daily standup"

# With timezone override
~/.shell/skills/shell-schedule/scripts/shell-schedule once --at "2024-03-15T09:00:00" --tz "America/Los_Angeles" --message "Meeting"

# With prompt mode (routes through Claude instead of plain notification)
~/.shell/skills/shell-schedule/scripts/shell-schedule cron --expr "@daily" --message "Check inbox" --mode prompt
```

## Options

- `--at <datetime>` — when the one-shot fires. Accepted: RFC3339, `"2026-07-23 21:00"` (local to tz), or bare `"21:00"` = the NEXT occurrence (today if still ahead, else tomorrow). Past date-times are rejected with an error — fix the time and retry, never assume it was created.
- `--expr <cron>` — cron expression or alias (@daily, @hourly, @weekly, @monthly)
- `--message <text>` — schedule message/label
- `--tz <timezone>` — timezone override (default: scheduler timezone)
- `--mode <notify|prompt>` — notify sends plain text, prompt routes through Claude (default: notify)

The SHELL_CHAT_ID environment variable is used automatically.

**WARNING:** Do NOT use `[schedule]` text directives in your response — they are silently stripped and do nothing. Always use this script via Bash.

**CRITICAL:** Do NOT use `CronCreate` for reminders — it is session-only and **dies on every session restart**. This script (`shell-schedule`) writes to SQLite and persists across restarts. It is the ONLY reliable way to create scheduled reminders.

When a user asks "remind me at 9 PM to do X", ALWAYS use:
```bash
~/.shell/skills/shell-schedule/scripts/shell-schedule once --at "21:00" --message "Reminder: do X" --mode notify
```
