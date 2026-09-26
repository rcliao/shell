# Design: Discord as a messaging platform

Status: built, pending a live check with real bots (2026-09-26). Research: `docs/RESEARCH-MESSAGING-PLATFORMS.md`.

## Intent

The family is moving from Telegram to Discord entirely: phones and desktops, DMs and the family room with
both agents. Telegram stays running during the move, and each conversation switches over when its Discord
channel is linked. After the move, Telegram can be turned off by removing its token.

The move must not cost the agents their past. Sessions, memories, schedules, projects and lanes are keyed
by internal chat ids. So a linked Discord channel takes over the internal id of the Telegram chat it replaces.
Everything keyed on that id carries over unchanged.

People are linked by hand, once: each Discord user id maps to the Telegram user id that person already has.
Labels, canonical names, allowlists and memory provenance then work unchanged.

## Components

| Component | Where | Role |
|---|---|---|
| `discord.Bot` | `internal/discord/bot.go` | Gateway session (discordgo), outbound sends, the `outbound` interface |
| `discord.Handler` | `internal/discord/handler.go` | Inbound: auth, group addressing, attachments, turn run with streaming edits, reactions, text commands |
| `discord.Addresses` | `internal/discord/address.go` | Discord ids ↔ internal (chat, thread) ids; user links |
| `discord` text helpers | `internal/discord/format.go` | 2,000-char chunking that keeps code fences intact; streaming tail view |
| `daemon.routedOutbound` | `internal/daemon/outbound.go` | Picks Telegram or Discord for every outbound send by chat id |
| Config `discord` | `internal/config/config.go` | token env, user links, chat links |

## Data flow

**A family member writes in the Discord family channel**
1. Discord delivers `MESSAGE_CREATE` to each agent's bot over the gateway websocket. No public endpoint is needed.
2. The handler drops the bot's own messages and other bots' messages. This is a deliberate v1 loop guard,
   not parity: Discord shows bots each other's messages, whereas Telegram mostly doesn't. The Telegram handler
   has peer-bot exchange limits (`isPeerBot`, `botExchangeLimit`), and they can be ported when peer messages
   are enabled. The guard also keeps the owner's "blind until observed" decision.
3. The handler resolves the author through `users` (Discord id → Telegram id). An unlinked author is not
   answered. A linked author then goes through the existing Telegram policy (`telegram.Auth.Check` with the
   Telegram id), unchanged: `allowed_users`, `group_allowed_users`, the dm/group policy and the rate limiter.
   The daemon passes Auth in as a function, so `discord` does not import `telegram`.
   The channel resolves as follows:
   - A message in a thread resolves its **parent** channel first, by link or else by the fixed rule. That
     gives the chat. The thread is +thread_snowflake unless the thread itself has a `chats` entry.
   - A thread inside the linked family channel therefore stays in the linked chat.
   - The channel type and parent id come from discordgo's state cache (the `Guilds` intent). On a cache miss,
     one `Channel` API call is made.
4. In a group, the handler records the human message in the shared transcript, then applies the same addressing
   rules as Telegram: an @mention of this bot, a reply to it, a name prefix, autonomous mode.
5. It downloads attachments (images, PDFs) to temp files, opens the pending-turn ledger, reacts 👀 and posts
   "Thinking…".
6. `bridge.HandleMessageStreamingEvents(chat, thread, …)` runs the turn. Text deltas are flushed by editing the
   placeholder at most once every 1.5 s, showing the tail when the text is longer than 2,000.
7. The final reply replaces the placeholder. When it is longer than 2,000, the placeholder is deleted and the
   reply is sent as chunks. Media and documents are sent as attachments. The message map is saved, the ledger
   is completed, and the handler reacts ✅ (or 🤔 when the reply is a question).

**The scheduler fires a reminder for an internal chat.** `routedOutbound.SendText(chat, thread, text)` finds a
Discord address for (chat, thread) and sends there. It finds one either through a chat link or because the chat
id is in the Discord range. Otherwise the send goes to Telegram. Relay, project home, heartbeat and replay all
use this path.

## Data model

No new tables and no migration. Ids:

- **Discord-native conversations** use a fixed rule. A Discord snowflake is ≥ 10^17 for anything created after about October 2015. This is enforced as a floor: a lower id is rejected and logged. Telegram ids are
  always below 10^16 in magnitude, so the two ranges never meet.
  - A DM channel becomes chat = +channel_id. That keeps "positive = private".
  - A guild channel becomes chat = −channel_id. That keeps "negative = group".
  - A thread inside a channel becomes thread = +thread_channel_id. Thread ids stay positive, because negative
    thread ids are reserved for lanes.
  - Both agents compute the same ids, so the shared transcript and A2A agree without coordinating.
- **Linked conversations** (continuity) come from config, never from the database:
  ```json
  "discord": {
    "enabled": true,
    "token_env": "PIKAMINI_DISCORD_TOKEN",
    "users": { "<discord user id>": <telegram user id> },
    "chats": { "<discord channel or thread id>": { "chat_id": <internal chat id>, "thread_id": 0 } }
  }
  ```
  The reverse lookup (internal → Discord) is built from the same map at startup.
  **Both agents' configs must carry identical `users` and `chats`**, or the shared transcript and A2A disagree.
  The daemon logs the link counts at startup so that a mismatch is visible.
- A Discord message id is a snowflake. It fits Go's `int` on 64-bit and is unique across Discord, so
  `message_map`, the ledger key (`chat:msg`) and `WithTelegramMsgID` take it unchanged. For edits and pins,
  where the outbound interface carries no thread, the bot keeps a bounded message id → channel id map. When the
  id is missing from the map, it falls back to the chat's base channel.

## Interfaces

- New config block `discord` (above), plus `Config.DiscordToken()`.
- The daemon's `outbound` gains no methods. `routedOutbound` implements it over {telegram, discord}, and `Start`
  runs both.
- `discord.NewBot(token, br, AgentConfig) (*Bot, error)` returns a type that implements the daemon `outbound`.
- No CLI changes in this pass. Linking is done by editing config, which is the one-time manual step the owner chose.

## Plan and finish line

1. The config block and address mapping, with unit tests (ranges, sign rules, links, reverse lookup).
2. Formatting helpers: chunking at 2,000 that keeps code fences, and the streaming tail. With unit tests.
3. The Discord bot, covering outbound sends and inbound turns. The handler talks to Discord through a small
   `api` interface so it can be tested with a fake.
4. Daemon wiring: `routedOutbound`, Discord start/stop, the duplicate-token guard for Discord tokens.
5. Docs: ARCHITECTURE.md, a setup guide (`docs/DISCORD-SETUP.md`: create the app, intents, invite, link ids).

**Finish line for this pass:** `make build` and `go test ./...` are green. The handler's fake-API tests cover a
DM turn, a group turn with a mention, an ignored unlinked user, and a long reply that gets chunked. Routing tests
show that sends for linked, native and Telegram chats each reach the right transport. A live round-trip with a
real Discord bot then needs the owner's bot token.

**Out of scope (next passes):**
- Native slash-command registration. Text commands such as `/new` already work.
- Button components beyond link buttons.
- Voice notes, stickers and albums.
- Message coalescing and absorption.
- Streaming for regenerate reactions.
- The role-based `group_domain` routing, which is off on both agents today.
- A `shell discord link` CLI.
- Moving old Telegram forum-topic sessions onto Discord threads beyond what a `chats` entry per topic gives.

## Verification

- A baseline `go test ./...` was green before any change (23 packages; run outside the sandbox because the
  tests bind local ports).
- After each step: `go build ./...` and the package tests.
- At the end: a full `go test ./...`, `make build`, and a fresh-context review of the diff against this page.
- Live check, once the owner has made the bot: DM the agent on Discord, check that the reply streams, and check
  that a scheduled message for a linked chat arrives on Discord.
