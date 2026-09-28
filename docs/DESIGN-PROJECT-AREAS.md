# Design: project areas (umbrella projects), travel first

2026-09-27. **Approved by the owner 2026-09-27; plan steps 1–3 built (branch `areas-build`), migration pending.** It was
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
- **Both channels must belong to the family chat.** `Addresses.Inbound` maps a
  post to its parent's chat only when the parent (the forum) is in
  `discord.chats`. Otherwise a post resolves to chat = −forum snowflake, a
  separate chat with no family memory or projects. Two ways to fix this:
  - (a) add a `discord.chats` entry for the forum (family chat; its
    `thread_id` is ignored for posts). This is a config edit for every new
    area forum.
  - (b) **guild auto-join** (B1 in `docs/DISCORD-PARITY.md`): any channel in
    a configured guild defaults to a topic of the family chat.

  **Recommended: (b), built first**, so that an agent that creates an area or
  forum never needs a config change or deploy. This design assumes (b). If
  the owner picks (a), the migration adds the entry in step 1, and
  `project create --kind area` must also write that link.

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
   `#旅遊-travel` (text) as the area channel. Prerequisite: guild auto-join
   (B1) is live, or the forum has its own `discord.chats` entry. Check this
   by posting a test message in the forum: it must resolve to the family chat.
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

## Part 2: agents make areas, and the post is the shared record

Added 2026-09-27, after the first live request. The owner asked both agents
in the main channel for a gaming area. Both checked the code, found that
nothing could create a channel, and asked the owner for ids. They then
spent five messages handing the registration back and forth, because each
believed there was one registry. Owner decisions, the same day:
- **Channels need a yes; posts don't.**
- **The post is the shared record** between the two agents, rather than
  one project registry shared by both.
- The sidebar should show what the agents are tracking.

**Who does what.**
- *Explicit request.* A person asks for an area. Whichever agent answers
  runs `project create --title 遊戲 --emoji 🎮 --kind area --place new`.
  Shell finds or creates the area's text channel and its forum (with stage
  tags) in the chat's Discord server, in the category of the chat's main
  channel. It sets the channel topic to point at the forum, and registers
  the area bound to that channel with `place_ref` = the forum. The agent
  replies with both channels as mentions.
- *The other agent.* It runs the same command. Creation is **find-or-create
  by name**, so it reuses the channels and registers its own row. Nobody
  hands off, and nobody asks for ids.
- *Implicit.* An agent notices a kind of work that keeps coming back (the
  router's recurring subjects), or active projects that have no place. It
  **proposes** an area in chat or in its weekly review, and only a yes
  creates it. Filing a new project into an existing area as a post needs no
  yes: the agent does it and says so.
- *The post as the shared record.*
  - `create --area X --place auto` first looks for an open post with the
    same title in the forum. If it finds one it **joins** it (binds to that
    post) instead of opening a second one.
  - An agent that is in a post it has no project for runs `project join`
    on that thread. That registers its own row for the post: title from the
    post, area from the forum, stage from the tag.
  - The turn block hints at this whenever a Discord thread belongs to none
    of the agent's projects.

**Interfaces.**
- RPC `create`: `place: "new"` for areas, plus optional `channel_name` and
  `tags`.
- RPC `join`: takes `message_thread_id` and returns the project it made or
  found.
- `rpc.Places` gains:
  - `EnsureAreaPlaces(chatID, name, tags) (channelThread int64, placeRef string, err error)`
  - `FindThread(chatID, placeRef, title) (threadID int64, ok bool, err error)`
  - `ThreadInfo(chatID, threadID) (title, placeRef, tags, err)`

  These are Discord only. On Telegram, `place: new` is refused (the family
  has moved to Discord), and areas there bind a topic by hand as before.
- The skill documents the rules above. The weekly review's evidence adds
  the active projects that have no place and the areas that exist.

**Not doing.**
- A per-area pinned index. Each agent would pin its own, so every area
  channel would show two lists that don't agree. The forum is the shared
  index.
- Creating a channel without a yes.
- Deleting or renaming channels.
- A shared registry (plan P4).

**Verified by.**
- Fake-API tests: a second `EnsureAreaPlaces` reuses both channels, and a
  same-title create joins the existing post.
- An RPC test for `join`.
- Live: the owner's gaming request results in two channels, one area row
  on each agent, and no ids asked for.

## Part 3: one doc per post, a summary the agent owns

Added 2026-09-28, after a day of real use. The posts worked: 21 of 21
project messages were written in the project's own post. Three things did
not:
- **Two docs per shared trip.** Each agent kept its own doc and ran its own
  research on the same post: Japan was 13.9 KB with 7 commits on one agent
  and 5.2 KB with 5 on the other; Taiwan 11 vs 13 commits. That is double the
  cost, and the two versions drift apart.
- **No live summary in a post a person opened.** Discord lets only a
  message's author edit it.
- **An agent told the family the summary would follow its doc.** It could
  not: every edit it tried was refused.

The owner approved one doc per post. This reverses part 2's "each agent
keeps its own notes" for docs; memories stay separate.

**Who does what.**
- *Linking.* A project in an area with a Discord post, and an area bound to
  its channel, keeps its doc in one git repo at
  `~/.shell/shared/projects/post-<thread>/`. Each agent's
  `workspace/projects/<slug>` is a symlink to it, so every reader and
  writer that goes through the workspace (doc-read, doc-write, research,
  Notion mirror, the [Project] block) works unchanged.
  - The first agent to link **moves** its doc and history there and
    becomes the post's **owner** (the `owner` file).
  - A later agent's doc is backed up. If it has content of its own (more
    than the scaffold commit), it is kept as `notes-<agent>.md`, and that
    agent's turn block asks it to fold anything missing into the shared doc
    and then delete the file.
  - Linking happens when a place is created or joined, and for existing
    projects at daemon startup, under a lock shared by both agents.
- *Writing without lost updates.* Every path that shows an agent the doc
  (doc-read, the research and comment prompts) records the rev this agent
  saw. A doc-write to a shared doc is refused, under a lock shared by both
  agents, when the doc moved since: "changed since you read it (now rev X);
  doc-read again and redo your edit". A write with no read first is refused
  the same way.
- *Research once.* Only the owner runs a shared doc's research pass; the
  other agent's schedule fires and skips.
- *Summary.* The owner keeps the post's live summary. If it cannot edit the
  post's first message (a person opened the post), it posts its own
  "summary" message, pins it, and edits that one from then on
  (`summary.json` in the shared dir).
- *Stage.* The weekly review lists area projects that have no stage.

**Not doing.**
- Merging the two agents' memories.
- Notion: each agent's project may still mirror to its own page; both pages
  feed the one doc.
- Shared docs for projects that have no post (DM projects stay
  single-agent).

**Verified by.** Tests for:
- link: first agent moves its doc, second agent keeps its notes;
- the stale-write refusal;
- only the owner doing research and the summary;
- the summary falling back to an owned, pinned message.

Live: Japan and Taiwan each end up with one doc after the restart.

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
