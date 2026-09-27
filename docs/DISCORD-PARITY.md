# Discord vs Telegram: parity grid and Discord-native backlog

Status: review, 2026-09-27.

Sources:
- Telegram: a code inventory of `internal/telegram` plus live agent config toggles. Spot-checked: the typing
  indicator is never called; an unknown command replies "Unknown command".
- Discord: `internal/discord`.
- Usage: counts from the agents' stores and logs over the last 30 days, unless stated otherwise.

Legend:
- ✅ at parity or better
- 🟡 partial
- ❌ missing on Discord
- ➖ missing on both, or not applicable

## 1. Parity grid

### Inbound

| Feature | Telegram | Discord | Notes |
|---|---|---|---|
| Text, forum topics as separate sessions | ✅ | ✅ | Channels and threads map to topics |
| Photos, image files, PDFs | ✅ | ✅ | |
| Albums (several images, one turn) | ✅ 500 ms debounce | ✅ | Discord puts them in one message by design |
| Stickers | ✅ image + emoji + set name | ✅ | The name plus the image (PNG/APNG/GIF); Lottie stickers get words only. Before this fix a sticker-only message was dropped silently |
| Sender label `[From: …]` | ✅ | ✅ | Via linked Telegram id |
| Reply-to context quoted into the prompt | ➖ | ➖ | Used for routing only on both. See backlog B6 |
| Voice, audio, video, edits, forwards | ➖ | ➖ | Not handled on either |

### Group behaviour

| Feature | Telegram | Discord | Notes |
|---|---|---|---|
| Record every human message in the shared transcript | ✅ | ✅ | |
| @mention me / reply to me → answer; @peer → skip | ✅ | ✅ | |
| Name-prefix addressing, both-named → both answer | ✅ | ✅ | |
| Autonomous mode + silent [noop] | ✅ | ✅ | Live on both agents |
| Peer-bot exchange limits (3, 30 s cooldown) | ✅ | ➖ | Discord ignores bot messages entirely (loop guard) |
| Domain routing | built, off | ➖ | Off on both agents |

### Turn mechanics

| Feature | Telegram | Discord | Notes |
|---|---|---|---|
| Status reactions 👀 🕐 ⏳ ✅ 🤔 ❌ | ✅ | ✅ | Discord removes the previous one first |
| Placeholder progress voice | ✅ | ✅ | Shared `internal/progress` (PR #52) |
| Streaming edits | ✅ ~1/s | ✅ 1.5 s | Discord's edit limit is undocumented |
| Formatting | MarkdownV2 conversion | ✅ native Markdown | Tables are fenced (PR #51) |
| Chunking | ✅ 4,096 | ✅ 2,000, reopens code fences | About 1% of replies are over 2,000 |
| Rate-limit retry | ✅ own flood retry | ✅ discordgo retries 429 | |
| Typing indicator | dead code | ✅ | Discord is better |
| Busy retry, empty-DM retry, pending-turn ledger | ✅ | ✅ | Shared bridge |
| Coalesce queued messages from one sender | ✅ on | ❌ | Rare: about 1 in 30 days |
| Absorb into the active turn | umbreonmini only | ❌ | About 15 since July |
| Friendly errors | ✅ | 🟡 | Two generic messages |
| End-to-end timing | ✅ | 🟡 | Receive lag is recorded as 0 |

### Commands and reactions

| Feature | Telegram | Discord | Notes |
|---|---|---|---|
| `/new /status /help /remember /forget …` | ✅ 18 registered | ✅ as text | Discord reaches every bridge command, including `/usage`, `/review`, `/digest` |
| Command menu | ➖ none | ✅ slash commands | 9 family-facing commands; operator commands stay text-only |
| Reaction actions (🔄 📌 🗑 👍 👎 📋 🔁 ❌) | ✅ | ✅ | Usage: 4 |
| Regenerate streams into the same message | ✅ | 🟡 | Discord posts a new message, not streamed |
| ✅ / ❌ result mark after a reaction action | ✅ | ✅ | On the reacted reply |
| Hint for an unmapped emoji | ✅ | ➖ | Deliberately off: family members react to each other |

### Outbound and access

| Feature | Telegram | Discord | Notes |
|---|---|---|---|
| Proactive text, photo, video, document, dedup | ✅ | ✅ | Routed by chat (`routedOutbound`) |
| Link buttons, pin/unpin, edit by id | ✅ | ✅ | |
| Replay after a crash | ✅ | ✅ | Replies route to Discord |
| Access control | pairing + allowlist | ✅ manual links + same allowlist | Pairing is deliberately not ported |

**Parity gaps worth closing (in order):**
1. ~~Stickers~~ and ~~result marks~~: done.
2. Regenerate in place, with streaming. Medium.
3. Coalescing, then absorb. Medium, and rarely used.

## 2. Discord-native backlog (needs building)

The ranking weighs how the family uses the agents against the cost of building.

| # | Feature | Why it helps | Evidence | Effort |
|---|---|---|---|---|
| B1 ✅ | **New channels join the family chat automatically** (`discord.guilds: {guild → family chat}`). Any channel created in the server becomes a new topic of the family chat, with no config edit. | You plan to add channels. Today an unlinked channel gets a separate chat with no family memory, and each channel needs a manual link and restart. | Topics carry about 2,000 messages; 5 channels were linked by hand | S |
| B2 ✅ | **Native slash commands** with a menu and autocomplete (`/new`, `/status`, `/projects`, `/remember`, `/schedule`) | Makes the commands discoverable for non-technical family members. Today they are invisible and typed from memory. | 18 commands, no menu on either platform | M |
| B3 🧪 | **Tap-to-answer buttons** when the agent asks a question (options become buttons) | Replying on a phone becomes one tap | About 70 replies a month end in a question | M |
| B4 ✅ | **Regenerate and Remember buttons under replies** instead of hidden reaction codes | The reaction actions exist but are barely used | Reactions: 4 in 30 days | S–M |
| B5 ✅ | **Mention the person a reminder is for** (`<@user>`) | Guarantees a push notification even when the server notifies on mentions only | 31 active schedules; replies currently never ping | S |
| B6 ✅ | **Quote the replied-to message into the prompt** | Discord shows replies prominently, so the family will reply to specific answers and the agent should see what they replied to | Missing on both platforms | S |
| B7 | **Forum channel for projects** (one post per project, tags for status) | A natural home for the project workspace, with the project doc pinned per post | 5 active projects | M |
| B8 | **Discord Scheduled Events** for trips and appointments (with RSVP and a reminder) | Travel plans (Japan 2027) and appointments show in the server's event list | Travel and planning projects | M |
| B9 | **Native polls** for family decisions (restaurants, dates) | #美食 and travel choices become a vote | Restaurant topic about 2,500 messages | S |
| B10 ✅ | **Embeds** for listings and project cards (image, fields, link) | Housing listings and restaurant picks become readable cards | Housing project, restaurant topic | M |
| B11 | **Voice messages → transcription** | Discord mobile makes voice notes easy. Neither platform handles them today. | Usage unknown: it was never supported | M |
| B12 ✅ | **Localized timestamps** `<t:unix:R>` in schedules ("in 2 hours") | Clearer times, correct in any time zone while travelling | Schedules, travel | S |
| B13 ✅ | **Let the agents see each other's replies** as observation only (transcript, not triggering turns) | Fits "blind until observed" | Owner decision 2026-09-23 | S, needs an owner call |

**Suggested next build:**
1. ~~B1 (auto-join)~~ and ~~the sticker gap~~: done (PRs #53 and #55). Project areas (forum posts per
   project) are driven by the project/lanes session: docs/DESIGN-PROJECT-AREAS.md.
2. ~~B6, B5, B12~~: done (PR #57). B13 needed no code: both agents' Discord replies already land in the
   shared transcript, which is injected into the other agent's context (verified 2026-09-27: 7 rows each
   since the switch).
3. Owner's order for the rest (2026-09-27): ~~B10 cards~~ (done: ```card JSON blocks → embeds), ~~B4 reply buttons~~ (done: `discord.reply_buttons`), ~~B2 slash commands~~ (done: 9 family-facing commands), B3 answer
   buttons (shipped as an experiment: `discord.answer_buttons`; judge by whether the family taps them). Later: B8 events, B11 voice. B9 polls not picked.
