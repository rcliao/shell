---
name: google
description: Read/write Google Docs, Sheets, Calendar, Drive, and Tasks via the gog CLI
usage: ~/.shell/skills/google/scripts/google docs cat <docId>
allowed-tools: Bash
tier: core
---

# Google Workspace

Read and write the family's Google ecosystem — Docs, Sheets, Calendar, Drive,
Tasks — through one wrapper.

**Canonical invocation** — ABSOLUTE path, works from any cwd. The wrapper
handles account/auth itself: NEVER call `gog` directly, never probe auth,
never ls/head the script.

```bash
~/.shell/skills/google/scripts/google docs cat <docId>
```

Add `--json` to any command when you need to parse the output.

## Docs

```bash
~/.shell/skills/google/scripts/google docs create "Title" --json
~/.shell/skills/google/scripts/google docs cat <docId>                    # read as text
~/.shell/skills/google/scripts/google docs write <docId> "content"        # REPLACES body
~/.shell/skills/google/scripts/google docs insert <docId> "text" --end    # append
~/.shell/skills/google/scripts/google docs find-replace <docId> "old" "new"
~/.shell/skills/google/scripts/google docs info <docId> --json            # title/url
```

⚠️ `docs write` replaces the ENTIRE body. To add to an existing doc, use
`docs insert --end`. Before any destructive write to a doc you didn't create
this turn, `docs cat` it first.

## Sheets

```bash
~/.shell/skills/google/scripts/google sheets get <sheetId> "Sheet1!A1:D10" --json
~/.shell/skills/google/scripts/google sheets update <sheetId> "Sheet1!A1" --values '[["a","b"]]'
~/.shell/skills/google/scripts/google sheets append <sheetId> "Sheet1!A1" --values '[["row"]]'
```

## Calendar

```bash
~/.shell/skills/google/scripts/google calendar list --json
~/.shell/skills/google/scripts/google calendar events <calId> --from today --to +7d --json
~/.shell/skills/google/scripts/google calendar create <calId> "Title" --from "2026-08-01 10:00" --to "2026-08-01 11:00"
```

## Drive & Tasks

```bash
~/.shell/skills/google/scripts/google drive search "name contains 'trip'" --json
~/.shell/skills/google/scripts/google drive upload <path> --json
~/.shell/skills/google/scripts/google tasks lists --json
~/.shell/skills/google/scripts/google tasks add <listId> "Buy cat food" --due 2026-08-01
```

## Rules

- After a write (docs write/insert, sheets update, calendar create), READ IT
  BACK (`docs cat`, `sheets get`, event get) and only then tell the user it's
  done — same read-back-receipt contract as the notion skill.
- Return the shareable URL when you create something (`--json` gives it).
- Gmail exists under `gog gmail` but is OUT OF SCOPE for this skill — never
  send or read email unless the user explicitly asks in the current message.
- If a command fails with an auth error ("No auth for ..."), STOP and tell
  the user re-authorization is needed (`gog auth add <account>` in their own
  terminal) — do not attempt browser OAuth flows yourself.
