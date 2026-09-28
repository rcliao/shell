---
name: project
description: Project registry — create, list, and look up first-class projects (multi-week research work bound to a doc, chat, and schedule); read/write the canonical project doc with commit receipts
usage: ~/.shell/skills/project/scripts/project create --title "..." [--emoji E --export-ref ID --doc-path PATH --instructions "..." --lang L]
allowed-tools: Bash
tier: hot
---

# Project registry

<!-- hot -->
Script: `~/.shell/skills/project/scripts/project` (absolute path). Your
`[Projects]` block lists this chat's projects with their doc ids — use them;
never re-derive an id.
- **No receipt, no claim.** `create` prints `Project <slug> created`;
  `doc-write` prints `Doc <slug> committed: rev <hash>`. Without that line
  nothing was saved — say so and retry. Never invent a rev.
- `doc-write` replaces the whole doc: `doc-read` first, edit, write it all.
  A project in a post shares ONE doc with the other agent (doc-read says so):
  if doc-write answers "changed since you read it", doc-read again and redo
  your edit on top — never write your old copy over theirs.
- The doc is a working page (24 KB budget, 8 KB for `更新紀錄`): replace stale
  content instead of appending. A refused write names the section to cut;
  shrinking is always accepted.
- Heading roles: `目標`/`限制` keep · `決定` dated one-line decisions, never
  cut · `現況`/`選項` current state only · `待決定` open questions, remove
  when answered (record the answer in `決定`) · `更新紀錄` recent entries,
  older folded into one line.
- Managed docs mirror to Notion by themselves — never edit that page by hand.
- Areas (travel, home, gaming…) hold many projects, one forum post each. Asked
  for an area: `create --kind area --place new` yourself, never ask for ids;
  the other agent keeps its own list, so run it even if it did. New areas
  need a yes, posts don't. See "Areas" below.
<!-- /hot -->

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

# Read the canonical doc (printed with the rev it was served at)
~/.shell/skills/project/scripts/project doc-read housing-search-2026

# Write the canonical doc (full content, from a file or stdin) — prints the commit rev
~/.shell/skills/project/scripts/project doc-write housing-search-2026 --file /tmp/doc.md --attribution "research pass"
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
- **Never claim a doc was saved without the printed rev.** `doc-write` prints
  `Doc <slug> committed: rev <hash>` — that hash is the receipt. No printed
  rev = no write happened; re-read the error and retry. Never invent or
  paraphrase a rev.
- `doc-write` replaces the WHOLE doc: always `doc-read` first, edit, then
  write the full revised content. If the user edited the doc file on disk,
  their version is auto-committed separately before yours (look for
  `human-edit(local):` in history) — never overwrite it silently without
  reading first.
- **The doc has a size budget (24 KB), and its log has its own (8 KB).** It is
  a working page, not a log: replace stale content instead of appending under
  it. `doc-write` REFUSES a write that leaves the doc (or the `更新紀錄`
  section) over budget and larger than before, and says which. A write that
  shrinks it is always accepted.
- **The headings have roles — keep them, consolidate by role:**
  - `目標` / `限制` — what the project is for. Never cut these to save space.
  - `決定` — dated one-line decisions ("2026-09-01 booked the ryokan, not the
    hotel"). The project's memory. **Never cut.** When something gets decided
    in chat or in a log entry, write it here. Add the heading (after `限制`)
    if an older doc lacks it.
  - `現況` / `選項` — the CURRENT state and the options still open. Delete
    options that lost and status that is no longer true; do not keep both
    versions.
  - `待決定` — open questions that need a human. One line each; remove a line
    the moment it is answered (and record the answer under `決定`).
  - `更新紀錄` — recent dated entries in full; fold older ones into ONE dated
    summary line. Git history keeps the detail.
- Projects created without `--doc-path` get a managed doc automatically
  (`projects/<slug>/doc.md` in your workspace, its own git repo, template
  sections 目標/限制/決定/現況/選項/待決定/更新紀錄). Pass `--doc-path` only to
  bind an EXISTING external file — doc-read/doc-write do not work on those.
- **Managed docs mirror to Notion automatically.** When the daemon is
  configured (a `NOTION_TOKEN` plus `notion.project_parent_page_id` in
  config), every successful `doc-write` queues a background render: the first
  one creates the project's Notion page (icon = project emoji), later ones
  update only the changed `##` sections. You never render by hand and never
  edit that page with the notion skill — write the canonical doc; the mirror
  follows within a minute. `get` prints `notion_url` once the page exists;
  that is the link to share when the user asks where the doc lives. A project
  whose `export_ref` was registered by hand (pre-existing page or database)
  is left untouched by the mirror and gets no `notion_url`.

## Comment loop (Notion feedback)

The daemon polls each project's Notion page (~every 30 min) for new comment
threads and direct page edits. When the user comments, YOU get a bounded
revision turn in the project's chat session with the comment text. Contract:

- **The conversation happens in Notion.** Your turn's visible reply is posted
  back INTO the user's comment thread on the page — do NOT also message the
  chat, relay, or notify anyone unless the user explicitly asked for that in
  the comment itself.
- One fix pass per comment thread: apply exactly what the comment asks via
  `doc-write` (the printed rev is the receipt), then reply in ≤2 lines in the
  project's language describing what changed. A question gets an answer in
  the reply; touch the doc only if needed. Never re-dump the doc.
- The user resolving the comment in the Notion UI is the acknowledgment —
  each thread is answered once; there is no back-and-forth loop.
- Direct page edits are folded into the canonical doc automatically as
  `human-edit(notion)` commits and the page is re-rendered — you never
  reconcile the page by hand, and `doc-read` always reflects human edits
  after the next poll.

## Options (create)

- `--title <text>` — required; the human-readable project name
- `--emoji <e>` — project emoji (list row, doc icon, P4 reactions)
- `--chat <id>` — target chat (default: current `SHELL_CHAT_ID`)
- `--thread <id>` — the project's thread on its platform: a Telegram forum
  topic or a Discord thread/post id (default 0 = the chat itself). With an
  area, prefer `--place auto`, which creates the thread for you.
- `--export-ref <id>` — external doc id (export kind defaults to notion)
- `--doc-path <path>` — canonical doc path, e.g. `workspace/projects/<slug>/doc.md`
- `--instructions <text>` — standing guidance injected with the project row
- `project set-instructions <slug> --instructions "<text>"` replaces them.
  The message router also reads them to decide which messages belong to this
  project. If your weekly review shows messages routed to the wrong project
  (or missed), sharpen them, for example: "meal logs (早餐/午餐/晚餐 memo),
  snacks, medication taken".
- `--lang <code>` — the user's language for this project (e.g. `zh`, `en`)
- `--cadence daily|weekly|monthly` — autonomous research cadence (default
  `weekly`). Create registers the schedule itself; on each fire the daemon
  runs ONE bounded research pass in the project's chat and posts a ≤3-line
  delta there. Archiving or pausing the project disables the schedule;
  re-activating re-enables it — never manage `project:<slug>` schedules by
  hand via shell-schedule.

## Areas

An **area** is an umbrella for one recurring kind of work (travel, home,
school, gaming). It has its own text channel for loose talk and a forum for
its projects: **one post = one project**. Its doc holds what is true across
all its projects (who comes along, budgets, lessons learned). Sub-topics
(flights, hotels) are sections of a project's doc, not more places. The
server's channel list is how the family sees what you track, so every
active project should live in an area.

**Making an area (it needs a person's yes).**
- Asked for one ("make a gaming area"): do it yourself. Never ask for ids.
  `project create --title 遊戲 --emoji 🎮 --kind area --place new [--channel-name gaming] [--tags 想玩,在玩,玩完]`
  This finds or creates the channel and its forum, and registers the area.
  Reply with the two mentions the receipt prints.
- You have **your own** project list; the other agent has its own. If the
  other agent already made the area, run the **same command** anyway: it
  reuses the channels and registers your row. Never decide who does it, and
  don't hand it off.
- Not asked, but a kind of work keeps coming back (your review lists
  recurring subjects and projects with no area)? **Propose** it in one line
  and wait for a yes. Never create a channel unasked.

**Projects in an area (no yes needed).**
- `project create --title "<name>" --emoji E --area <area> --stage <s> --place auto --content "<first message>"`
  opens the project's post, or **joins** an open post with the same title,
  so the post stays the one shared record. Tell people where it is (`<#thread id>`).
- `project join --thread <post id>` — you're in a post you don't track yet
  (your block says so): register it as your own project before working on it.
- `project move <slug> --area <area> [--stage <s>] [--place auto] [--content <text>]`
  — file an existing project under an area.
- `project stage <slug> --stage <s>` — its step, shown as the post's tag.
- `project archive <slug>` — archive; its post is closed too.

A post's first message is its live summary: shell re-renders it from the doc
on every doc-write and stage change, so keeping the doc's 目標/決定/待決定
current keeps the post current. Don't edit it by hand.

In an area's own channel your `[Project]` block lists its projects with
their threads: answer there, then point to the project's post.
