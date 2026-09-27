# Design: project areas (umbrella projects), travel first

2026-09-27. **Proposal for the owner's review; nothing is built.** It was
asked for by the Discord session (shell-37) on the owner's behalf: travel
spans many trips, each with its own sub-topics, and a flat project list does
not fit it. It builds on `docs/DESIGN-DISCORD.md`, the lanes in
`docs/DESIGN-ROUTER-AND-SUGGESTIONS.md`, and `docs/PLAN-PROJECT-WORKSPACE.md`.

## Intent

The family's recurring kinds of work are travel, the home, school and
health. Each is a stream of separate projects that share knowledge: the
same family constraints, preferences, people and past decisions. Today a
project is flat: one doc, one chat or thread. We want:

- **An area** for each recurring kind (`travel`). It is a place for loose
  talk and new ideas, plus one living doc of what holds across all its
  projects: "we need 2 rooms", "one parent avoids red-eyes", "past trips and how
  they went".
- **A project** for each concrete effort in the area (`日本行程-2027`), with
  its own place, doc, lane and research.
- **Sub-topics** of a project (flights, hotels, itinerary, budget) as
  sections of the project doc, not as more places.

It should work the same on Telegram and Discord, and the agent should do the
filing: people just talk.

## Model

The model is one table and two new fields. An area is a project row with
`kind = area`, and a project names its area.

| Field | Values | Meaning |
|---|---|---|
| `kind` | `project` (default) \| `area` | An area is a long-lived umbrella and is never "done". |
| `area` | slug of an area, or `""` | This project belongs to that area. |
| `stage` | area-defined, e.g. 規劃中 / 已訂 / 完成 | A finer lifecycle than `status` (active/paused/archived), shown as a Discord forum tag. |
| `message_thread_id` | unchanged | Any platform's thread id: a Telegram topic or a Discord thread or post. Only its doc comment changes. |

Why an area is a project and not a new entity: it reuses the doc, the
Notion mirror, research schedules, the `[Project]` block, lanes, the agenda
and the weekly review.
- The area doc holds cross-project knowledge and an auto-rendered index of
  its projects (stage, dates, link).
- The area's research pass (for example monthly) consolidates finished
  projects' lessons into the area doc.

**Places, on Discord:**
- **The area** gets a text channel, e.g. the existing `#旅遊-travel`, bound
  to the area project (`discord.chats` entry → the family chat + that
  channel's thread id; `project bind`). This is where travel talk and new
  trip ideas go.
- **Its projects** get a forum channel, `#旅遊-trips`: one post = one trip =
  one project, with `message_thread_id` = the post's snowflake (a positive
  thread in the family chat, per the Discord design). Discord cannot nest
  threads and cannot convert a text channel into a forum, hence two
  channels.
- **Forum tags** mirror `stage`. Archiving the project archives (closes) the
  post.

**On Telegram (while it lasts):**
- The area is a forum topic.
- Each project is a forum topic, created by the bot with `createForumTopic`.
  Stages have no tags; the topic title carries an emoji.

## Routing and lanes

- **Inside a project's own post:** the thread decides. The project is
  bound to that thread, so lanes skip it (as today for bound Telegram
  topics). The turn gets the scoped `[Project]` block, including the
  "record decisions in the doc" line. There is nothing new to build: a
  Discord thread id is just a thread id.
- **Inside the area channel:** the area project is bound to that channel,
  so the turn gets the area's `[Project]` block (the doc and the index of
  trips). A message that is really about one trip ("book the Kyoto
  hotel") is answered there. The area block tells the agent which post
  belongs to that trip, and the agent can point people to it ("continuing
  in #日本行程-2027"). It never moves messages itself.
- **Anywhere else** (the family main channel, a DM): lanes route as today.
  The candidates are the chat's active projects, including every trip's
  project (same chat) and the area. A trip message routes to that trip's
  lane. A new trip idea routes to the area lane.
- **New projects.** The router feedback loop already surfaces recurring
  subjects without a project. For an area, the agent proposes in the area
  channel: "Want me to start a project for the Vegas trip? I'll open a post
  in #旅遊-trips." A `yes` creates the project and its post.

## Interfaces

- **Skill and CLI.**
  - `project create --area travel [--place auto]` creates the project and,
    with `--place auto`, its place: a forum post in the area's forum
    (Discord) or a forum topic (Telegram). It records `message_thread_id`,
    so agents never handle raw ids.
  - `project create --kind area --forum <channel id>` creates an area and
    records its forum.
  - `project stage <slug> <stage>` sets the stage and syncs the forum tag.
  - `project archive` also closes the post.
  - The `--thread` doc changes from "Telegram forum topic id" to "the
    project's thread on its platform (Telegram topic or Discord thread or
    post)".
- **Platform capability.** Creating a place needs a small interface both
  platforms implement:

  ```go
  type Places interface {
    CreateThread(chatID int64, parent string, title string) (threadID int64, err error)
    SetThreadTags(chatID, threadID int64, tags []string) error
    ArchiveThread(chatID, threadID int64) error
  }
  ```

  Discord implements it with forum posts and tags, Telegram with forum
  topics (tags become a no-op). The RPC `project create` calls it.
- **Config.** An area's forum lives on the area row (for example
  `place_ref`), not in config, so an agent can make a new area without a
  deploy.

## Does it generalise?

Yes, which is why "area" is the right noun rather than "travel".

| Area | Projects (examples from the last 14 days of real chat) |
|---|---|
| travel | 日本行程-2027, the NorCal trip (archived), a Vegas drive |
| home | the housing search, bathroom glass repair, home internet |
| school | the UCSD academic plan, Milan study abroad |
| health | the meal log (one standing project), medication |

The router's candidate-project list (weekly review) already finds these:
"family visit", "dining out", "plant care", "study abroad planning". An
area turns "a candidate" into "a candidate in an area", which is where it
belongs. The health log shows that an area can also hold one standing
project and no trips.

## Migration

1. Create the forum `#旅遊-trips` with tags 規劃中 / 已訂 / 完成. Keep
   `#旅遊-travel` (text) as the area channel.
2. Create the area project `travel` (kind area; doc: family travel
   constraints and a trip index) and bind it to `#旅遊-travel`.
3. **日本行程-2027 (pikamini):**
   - set `area=travel` and `stage=規劃中`;
   - create its post;
   - bind `message_thread_id` = the post id;
   - post the doc's summary (decisions, open questions) as the first message and pin it;
   - its research schedule and Notion mirror are unchanged.
4. **北加家庭遊 (umbreonmini, archived):** optional, as history. It gets a
   closed post with a short retrospective, which its lessons also seed into
   the area doc.
5. Old Telegram sessions stay where they are. Nothing is copied: the post
   starts with the doc's summary, and lanes and continuity cover the rest.

## Out of scope

- **Nested places** (a thread per sub-topic): Discord cannot nest, and
  sub-topics are doc sections.
- **A registry shared between agents.** Projects live in each agent's own
  `shell.db`: Japan is Pika's, NorCal was Umbreon's. The other agent sees
  the post as an ordinary thread, which fits "blind until observed". Sharing
  one registry is plan P4 and needs its own design.
- **Automatic moves.** The agent never relocates people's messages between
  channels; it points.
- **Stages beyond one ordered list per area**, and workflow rules on them.
- **Calendar or booking integrations** for trips (external events, later).

## Plan and verification (once approved)

1. Schema: `kind`, `area`, `stage`, `place_ref`, with tests.
2. The `Places` interface on Discord and Telegram, with a fake-transport
   test.
3. Skill and CLI (`--area`, `--place auto`, `stage`, archive closes the post)
   and the area index render.
4. Migrate travel (steps 1–4 above) with the owner watching.

Verification:
- A message in the Japan post gets the Japan `[Project]` block.
- A trip message in `#旅遊-travel` gets the area block and a pointer to the
  post.
- A new trip idea in a DM routes to the area lane.
- Stage changes flip the post's tag, and archiving closes it.

## Evidence

- `store.Project` today has no parent or kind. `MessageThreadID` is
  documented as a "Telegram forum topic ID" but is already platform-neutral
  (Discord threads map to +snowflake).
- Lanes already skip a project-bound thread (`bridge/lanes.go`), so a post
  bound to a project routes deterministically with no router call.
- Current projects:
  - pikamini: `日本行程-2027` (active, thread 0), plus the health log,
    housing search and AI briefing;
  - umbreonmini: `北加家庭遊` (archived), plus the housing search.
- Candidate projects from the router feedback loop (2026-09-26): family
  visit, fruit ripening, cleaning supplies, plant care (50 messages over 7
  days), family outings.
