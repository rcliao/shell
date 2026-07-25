---
name: google
description: Read/write Google Docs, Sheets, Calendar, Drive, and Tasks via the gog CLI
usage: ~/.shell/skills/google/scripts/google docs cat <docId>
allowed-tools: Bash
tier: core
---

# Google Workspace

Read and write the family's Google ecosystem — Docs, Sheets, Calendar, Drive,
Tasks — through one wrapper. Every command below is verified against gog
0.34 — copy the shapes exactly.

**Canonical invocation** — ABSOLUTE path, works from any cwd. The wrapper
handles account AND keyring auth itself: NEVER call `gog` directly, never
probe auth, never ls/head the script.

```bash
~/.shell/skills/google/scripts/google docs cat <docId>
```

Add `--json` to any command when you need to parse output (`docs create
--json` returns `.file.id` and `.file.webViewLink`).

## Docs

```bash
~/.shell/skills/google/scripts/google docs create "Title" --json
~/.shell/skills/google/scripts/google docs cat <docId>                          # read as text
~/.shell/skills/google/scripts/google docs write <docId> --text "content" --replace   # REPLACES body
~/.shell/skills/google/scripts/google docs write <docId> --text "content" --append    # append
~/.shell/skills/google/scripts/google docs insert <docId> "text" --index 1      # insert at position
~/.shell/skills/google/scripts/google docs replace <docId> "old" "new"          # find & replace
```

⚠️ `docs write --replace` wipes the ENTIRE body. To add to an existing doc use
`--append`. Before any `--replace` on a doc you didn't create this turn,
`docs cat` it first. Markdown: add `--markdown` (works with --replace/--append).

## Sheets

```bash
~/.shell/skills/google/scripts/google sheets get <sheetId> "Sheet1!A1:D10" --json
~/.shell/skills/google/scripts/google sheets update <sheetId> "Sheet1!A1" "val1" "val2"
~/.shell/skills/google/scripts/google sheets append <sheetId> "Sheet1!A:C" "a" "b" "c"
```

## Calendar

```bash
~/.shell/skills/google/scripts/google calendar calendars                        # list calendars
~/.shell/skills/google/scripts/google calendar events --today --json           # today's events (primary)
~/.shell/skills/google/scripts/google calendar events --from 2026-08-01 --to 2026-08-07 --json
~/.shell/skills/google/scripts/google calendar create primary --summary "Title" --from "2026-08-01T10:00:00-07:00" --to "2026-08-01T11:00:00-07:00"
```

## Drive & Tasks

```bash
~/.shell/skills/google/scripts/google drive search "trip" --json
~/.shell/skills/google/scripts/google drive get <fileId> --json
~/.shell/skills/google/scripts/google tasks lists list
~/.shell/skills/google/scripts/google tasks add <tasklistId> --title "Buy cat food" --due 2026-08-01
```

## Rules

- After a write (docs write/insert, sheets update/append, calendar create),
  READ IT BACK (`docs cat`, `sheets get`, `calendar events`) and only then
  tell the user it's done — same read-back-receipt contract as notion.
  `docs write` prints a `revision` line — that plus a read-back is your proof.
- Return the shareable URL when you create something (`--json` →
  `.file.webViewLink`).
- Gmail exists under `gog gmail` but is OUT OF SCOPE for this skill — never
  send or read email unless the user explicitly asks in the current message.
- If a command fails with an auth error ("No auth for ..."), STOP and tell
  the user re-authorization is needed — do not attempt OAuth flows yourself.
- Docs owned by family members must be SHARED with the service account first;
  if you get a 403/notFound on a real doc, ask the user to share it rather
  than retrying.
