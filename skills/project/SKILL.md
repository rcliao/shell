---
name: project
description: Project registry — create, list, and look up first-class projects (multi-week research work bound to a doc, chat, and schedule)
usage: ~/.shell/skills/project/scripts/project create --title "..." [--emoji E --export-ref ID --doc-path PATH --instructions "..." --lang L]
allowed-tools: Bash
tier: hot
status: draft
---

# Project registry

A **project** is a named unit of multi-week research work (trip planning,
housing search): one slug binding the living document, the chat, scheduled
research, and memory. The registry is why you never re-derive a doc ID —
active projects for the current chat are injected into your context every
turn as a `[Projects]` block, external doc id (`export_ref`) included. Use
that id directly; never go archaeology-digging for it.

**Canonical invocation.** The script lives at the ABSOLUTE path
`~/.shell/skills/project/scripts/project` (works from any cwd — never guess
repo-relative paths). `SHELL_CHAT_ID` is already set for the current chat;
pass `--chat` only to register a project for a DIFFERENT chat.

```bash
# Create a project (slug is derived from the title server-side; CJK titles work)
~/.shell/skills/project/scripts/project create --title "Housing Search 2026" \
  --emoji 🏠 --export-ref <notion-page-id> \
  --doc-path workspace/projects/housing-search-2026/doc.md \
  --instructions "keep the options table sorted by price" --lang en

# List projects for this chat (all chats: --chat 0)
~/.shell/skills/project/scripts/project list

# Show one project in full
~/.shell/skills/project/scripts/project get housing-search-2026
```

## Hard rules

- **Never claim "saved" or "created" without the printed confirmation.** The
  script prints `Project <slug> created` plus the stored row — if you did not
  see that line, the project does NOT exist; re-read the error and retry.
- **Slug is server-derived** from the title when not given (lowercase,
  CJK-safe, punctuation → `-`; duplicates get `-2`, `-3`, …). Report the slug
  the server printed, not the one you assumed.
- **Emoji**: any emoji is accepted and stored, but one outside Telegram's
  reaction set prints a warning (`emoji not reaction-capable; reactions will
  fall back to 👀`). Pass the warning on when choosing an emoji with the user.
- `export_ref` is the external doc id (Notion page id). Registering it here
  is what makes it appear in your `[Projects]` block every turn — set it as
  soon as the doc exists.

## Options (create)

- `--title <text>` — required; the human-readable project name
- `--emoji <e>` — project emoji (list row, doc icon, P4 reactions)
- `--chat <id>` — target chat (default: current `SHELL_CHAT_ID`)
- `--thread <id>` — Telegram forum topic id for group projects (default 0)
- `--export-ref <id>` — external doc id (export kind defaults to notion)
- `--doc-path <path>` — canonical doc path, e.g. `workspace/projects/<slug>/doc.md`
- `--instructions <text>` — standing guidance injected with the project row
- `--lang <code>` — the user's language for this project (e.g. `zh`, `en`)
