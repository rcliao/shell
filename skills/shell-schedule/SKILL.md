---
name: shell-schedule
description: Create one-shot or recurring scheduled reminders
allowed-tools: Bash
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
