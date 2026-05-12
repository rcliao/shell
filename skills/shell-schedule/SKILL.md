---
name: shell-schedule
description: Create one-shot or recurring scheduled reminders
usage: scripts/shell-schedule once --at "ISO8601" --message "..." [--tz TZ --mode notify|prompt]
allowed-tools: Bash
core: true
---

# Scheduler

Create one-shot or recurring scheduled messages/prompts.

## Usage

```bash
# One-shot reminder
scripts/shell-schedule once --at "2024-03-15T09:00:00" --message "Team standup reminder"

# Recurring cron schedule
scripts/shell-schedule cron --expr "0 9 * * 1-5" --message "Daily standup"

# With timezone override
scripts/shell-schedule once --at "2024-03-15T09:00:00" --tz "America/Los_Angeles" --message "Meeting"

# With prompt mode (routes through Claude instead of plain notification)
scripts/shell-schedule cron --expr "@daily" --message "Check inbox" --mode prompt
```

## Options

- `--at <datetime>` — RFC3339 or local datetime for one-shot schedules
- `--expr <cron>` — cron expression or alias (@daily, @hourly, @weekly, @monthly)
- `--message <text>` — schedule message/label
- `--tz <timezone>` — timezone override (default: scheduler timezone)
- `--mode <notify|prompt>` — notify sends plain text, prompt routes through Claude (default: notify)

The SHELL_CHAT_ID environment variable is used automatically.

**WARNING:** Do NOT use `[schedule]` text directives in your response — they are silently stripped and do nothing. Always use this script via Bash.

**CRITICAL:** Do NOT use `CronCreate` for reminders — it is session-only and **dies on every session restart**. This script (`shell-schedule`) writes to SQLite and persists across restarts. It is the ONLY reliable way to create scheduled reminders.

When a user asks "remind me at 9 PM to do X", ALWAYS use:
```bash
scripts/shell-schedule once --at "21:00" --message "Reminder: do X" --mode notify
```
