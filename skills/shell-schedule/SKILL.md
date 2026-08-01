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

## Prefer the `shell_schedule` tool

You have a native `shell_schedule` tool. Use it rather than the Bash script —
it takes structured arguments instead of shell quoting, and it can tell you
whether a schedule actually ran:

```
shell_schedule(message="Reminder: take medication", at="21:00")
shell_schedule(action="list")
shell_schedule(action="describe", id=14)
shell_schedule(action="cancel", id=14)
```

`describe` is the one to reach for when someone says a reminder never arrived.
It returns the next fire times AND the recent run history — each attempt with
its outcome, how long it took, and how long it sat queued. That distinguishes
the three cases that look identical from the outside: never fired, fired and
failed, fired and was delivered.

The chat is filled in from your environment; pass `chat_id` only to schedule
for a DIFFERENT chat.

The Bash script below remains available and behaves identically — both write
the same rows through the same endpoint.

**Canonical invocation — copy this shape exactly.** The script lives at the
ABSOLUTE path `~/.shell/skills/shell-schedule/scripts/shell-schedule` (works
from any cwd — never guess repo-relative paths, never `source` it, never
probe with ls/head). `SHELL_CHAT_ID` is already set in your environment for
the current chat; only override it to schedule for a DIFFERENT chat.

```bash
~/.shell/skills/shell-schedule/scripts/shell-schedule once --at "2026-07-20T09:00:00" --tz "America/Los_Angeles" --message "..." --mode prompt
```

Verify after creating: the script prints `Schedule #<id> created` followed by
the next fire times — if you did not see those lines, the schedule does NOT
exist; re-read the error and retry rather than assuming success.

**Registration is idempotent.** The same (chat, type, expression, message)
registered twice returns the EXISTING row and prints
`Schedule #<id> already existed` instead of creating a duplicate. Report that
honestly — say the reminder was already set, don't claim you made a new one.

**Read the previewed fire times before replying.** They are already resolved
in the schedule's timezone, so a wrong cron expression or a reminder landing
tomorrow instead of today is visible right there. If they look wrong, delete
and re-create rather than telling the user it's set.

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

## Response fields

`POST /schedule` (what this script calls) returns:

- `id` — the schedule row ID
- `type` — `once` or `cron`
- `created` — `true` when a new row was inserted, `false` when an identical
  registration already existed and was returned instead
- `status` — `created` | `existing` (string twin of `created`)
- `next_run` — the stored next run, in UTC
- `next_runs` — the next 3 exact fire times, resolved in the schedule's
  timezone. Firing jitter (a few seconds to at most 9 minutes of spread, so
  both agents' jobs don't all land on `:00`) is not shown — these are the
  scheduled occurrences.

## Operator commands (Bash, read-only)

- `shell schedules [--all] [--config <path>]` — every schedule with its next 3
  fire times, enabled state, pause reason, and last successful run. This is
  the fastest way to verify a schedule really exists and will fire when you
  think it will.
- `shell job-runs [n] [--schedule <id>] [--config <path>]` — the fire ledger.
  One row per fire ATTEMPT with outcome `fired_ok` | `spawn_failed` |
  `turn_failed` | `skipped_quiet_hours` | `skipped_disabled` |
  `skipped_overlap` | `interrupted` | `running`. Use it when a reminder
  "didn't arrive": it distinguishes a schedule that never fired from one that
  fired and failed. (`shell_schedule(action="describe")` gives you the same
  history without leaving the tool.)

Outcomes worth recognizing:

- `skipped_overlap` — the previous run was still going, so this fire was
  dropped (heartbeats) or queued (reminders). Normal under load, not an error.
- `interrupted` — the daemon restarted mid-run. The work did not finish.
- a repeated `turn_failed` on attempts 1..N — a transient failure that was
  retried. Only the last attempt's outcome reflects what the user saw.

A schedule that hits an unrecoverable config error (bad cron expression,
unparseable time, no target chat) is auto-paused with a machine-readable
`paused_reason` instead of retrying forever — `shell schedules --all` shows it.
Re-enabling always recomputes the next run from NOW; missed occurrences are
never replayed.

**WARNING:** Do NOT use `[schedule]` text directives in your response — they are silently stripped and do nothing. Use the `shell_schedule` tool or this script.

When someone asks "remind me at 9 PM to do X":

```
shell_schedule(message="Reminder: do X", at="21:00")
```
