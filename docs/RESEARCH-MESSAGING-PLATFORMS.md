# Research: a second messaging platform (Discord and others)

Status: research in progress (2026-09-26). No code changes yet.

## Checklist

- [x] Coordinate with the lanes/routing session (constraints below)
- [x] Harness survey: Hermes, OpenClaw, OpenHuman, Muse — platforms, gateway design, user sentiment (desktop/mobile)
- [x] Platform bot-API comparison vs Telegram (Discord, Slack, WhatsApp, Signal, iMessage, Matrix)
- [x] Map Telegram coupling in this repo
- [x] Recommendation (below)
- [x] Proposed plan (below)
- [ ] Owner confirms goal + plan → tech-design one-pager → Phase 0

## Summary and recommendation

**What users like, by surface**
- Telegram is the default channel in every harness surveyed (OpenClaw: "the fastest channel to get working";
  Hermes: "mobile-optimized defaults"), and has the most issue traffic in both big repos. Users like it for
  phone-first chat, instant setup (BotFather, no server), good desktop + mobile sync, and — in 2026 — native
  streaming drafts with a Stop button (DMs only).
- Discord is the clear second. Users like it for the desktop app, threads per project, slash commands and
  native components (buttons, select menus, modals), and rooms where several bots can see each other.
- WhatsApp is where families already are (Meta's own agent, Muse, ships in its app + WhatsApp), but the
  official API now bans general-purpose AI assistants and the unofficial route gets numbers banned. Out.
- Signal/iMessage are unofficial bridges with hard edit caps (no streaming); Slack feels like work;
  Matrix onboarding is too heavy for the family.

**Discord vs Telegram for shell specifically**
- The most-cited Discord advantage ("threads keep projects apart") shell already has via Telegram forum topics.
- Real Discord gains: bots see each other (the two-agent group), slash commands + components, richer
  desktop use, no 20 MB download cap, email sign-up (no phone number).
- Real Discord costs: 2,000-char messages (chunker or file-baton), no native streaming (edit-based,
  ~1 edit/s, undocumented), 3 s interaction deadline, no threads in DMs, every family member needs an
  account and must join a server, busier UI. Hermes' Discord adapter reached 10k lines with streaming-duplicate
  and attachment bugs — the surface is bigger than it looks.

**Architecture every harness converged on** — one gateway, many adapters; a small required interface
(connect, send, typing, image) with optional methods that degrade to plain text; an explicit capability
declaration (edit, reactions, threads, buttons, native commands, max length); session keys shaped
`platform:chat[:thread]`; bot-loop protection for multi-bot rooms.

**Recommendation** — add Discord *alongside* Telegram, not as a replacement:
1. Keep Telegram for the family (phones, DMs, drafts).
2. Add Discord first for the owner's desktop use and multi-agent rooms.
3. Keep the id change small: map each Discord (guild channel / thread / DM) to an internal (chat_id, thread_id)
   through one binding table, positive-only thread ids (negatives stay reserved for lanes), rather than adding
   a platform column to ~20 tables.
4. Pull the streaming/chunking logic out of `telegram/handler.go` into a platform-neutral sink with a
   capability struct, so Discord's 2,000-char limit and edit throttle are parameters, not a fork.

**Open question for the owner (decides scope):** what is Discord *for*? (a) owner desktop use,
(b) agent-to-agent rooms, (c) family members who won't use Telegram, or (d) a platform-neutral harness
as a goal in itself. (a)/(b) → DM + one guild with threads is enough; (c) → onboarding and a fuller
feature match matter more.

**Costs of the binding-table option (b), stated honestly**
- Synthetic internal chat ids must keep the sign convention (negative = group, positive = DM) and live in a
  range that cannot collide with real Telegram ids.
- Ghost namespaces `chat:%d` become opaque for Discord conversations (the binding table is the decoder).
- `SHELL_CHAT_ID` and `shell_relay` address chats by id: the agent must learn the synthetic id to relay into Discord.
- Person identity: `user_labels`/`user_canonical` and memory `source_user` are keyed by Telegram user id. A
  person on Discord is a new id — map it to the canonical person or their memories fork (Hermes has this
  open as #4335). Needs a decision.

## Proposed plan (default: goal = owner desktop + agent rooms; owner can redirect)

- **Phase 0** — neutral rename of `bridge.WithTelegramMsgID` → a channel-message-id context
  (welcomed by the lanes session). Finish: build + tests green, no behavior change.
- **Phase 1** — `channel_bindings` table + a text-only Discord adapter (DM + one guild, threads) behind a
  config flag; no streaming, reactions or buttons. Wire as a second intake like `shell chat`.
  Finish: a Discord message round-trips to Claude and back, session row persisted, lanes untouched.
- **Spike** (before Phase 2) — measure Discord's real edit-rate ceiling on a test channel.
- **Phase 2** — extract streaming/chunking from `telegram/handler.go` into a platform-neutral sink with a
  capability struct (max length, edit interval, reactions, buttons); Telegram behavior unchanged.
- **Phase 3** — reactions, slash commands, attachments, bot-loop protection for multi-bot rooms.

Out of scope: Slack, Matrix, WhatsApp, iMessage; cross-platform session continuity; family onboarding.

Verification per phase: baseline `make test` before any change; tests pass after; `shell chat` still works
(proves the intake seam); a live Discord round-trip with the daemon started outside the sandbox; Telegram
smoke test (DM + forum topic) after Phase 2. Before Phase 1, a tech-design one-pager (Intent, Components,
Data flow, Data model, Interfaces, Plan).

## Constraints from the lanes work (already on main, PRs #29–#40)

Source: the routing session, 2026-09-26; see docs/DESIGN-ROUTER-AND-SUGGESTIONS.md and "Lanes (R1)" in ARCHITECTURE.md.

- The routing design is channel-neutral by owner decision; the CLI (`shell chat`) is already a second source.
- Session key is `process.SessionKey{ChatID, ThreadID}`. Project lanes allocate NEGATIVE session thread ids
  (`lane_sessions`); `store.RealThread` maps back. Negative thread ids stay reserved for lanes.
- In `bridge.HandleMessageStreamingEvents`: `sessThread` keys the session; `threadID` (real) is used for
  delivery, transcript, message maps. Anything sending from a session key must map through `RealThread`.
- `bridge.WithTelegramMsgID(ctx, id)` carries the channel message id — candidate for a neutral rename.
- `DeleteSession(chat, negative)` means "all topics"; `store.DeleteSessionThread` is the exact delete.
- User text starting with `[` is treated as synthetic and skips lanes.
- Natural address shape: (platform, chat, thread) → real thread; lanes sit underneath.

## Telegram coupling in this repo (2026-09-26)

Spot-checked: `process/session.go:20`, `bridge/transport.go:9`, `scheduler/queue.go:162`, go.mod, handler.go size.

**Good news — the library is contained.** `github.com/go-telegram/bot` v1.19 is imported only in
`internal/telegram/`. Only `daemon/daemon.go` and `cmd/shell/main.go` import `internal/telegram`.
MarkdownV2 lives only in `telegram/handler.go`.

**Seams that already exist**
- Outbound: `daemon.outbound` (`daemon/outbound.go:25-50`, plus `headlessOutbound`) and `bridge.Transport`
  (`bridge/transport.go`, implemented by `telegramTransport` in daemon.go). Scheduler, RPC relay and project
  home all send through these.
- Intake: `scheduler.MessageTurn{Transport, ExternalID}` with `RegisterTransport`/Sink (Update/Finish/Fail) in
  `scheduler/messageturn.go`; the CLI (`clichat.go`) is already a second source.
- `bridge.AgentResponse` is platform-neutral; `scheduler.PartitionKey` is a string.

**What leaks: the Telegram ID shape**
- `SessionKey{ChatID, ThreadID int64}`; every `Transport` method is `(chatID, threadID int64)`, message ids `int`.
- ~20 SQLite tables key on `chat_id INTEGER` (sessions UNIQUE(chat_id, message_thread_id), message_map,
  schedules, projects, lane_sessions, pending_turns.telegram_msg_id, ...); transcript DB has `telegram_msg_id`.
- Ghost memory namespaces/tags use `chat:%d`; `SHELL_CHAT_ID` is passed to Claude children and MCP.
- Sign conventions: negative chat id = group (`bridge/a2a.go:107`, `bridge.go:1042`, `lanes.go:119`,
  `project_hook.go:243`); chat 0 = system chat; negative thread id = lane session (reserved).
- Streaming UX (placeholder, throttled edits, MarkdownV2 fallback, chunking, flood retry) is inside the
  3134-line `telegram/handler.go`, not a reusable sink.
- One bot per daemon (`daemon.go:630`); config is Telegram-shaped (`TelegramConfig`, `allowed_users []int64`,
  `owner_chat_id`, `peer_bots`, `route.lane_chats`, `chat_models`, user_labels keyed by Telegram ids).

**Implication.** Discord ids are uint64 snowflakes (fit in int64 today, but positive — they would collide
with the "positive = DM" convention and could in principle collide with Telegram ids). Two options to weigh:
(a) add a `platform` dimension to the key and tables, or (b) map each external conversation to an internal
int64 conversation id via one `channel_bindings(platform, ext_chat, ext_thread) → (chat_id, thread_id)` table
and keep the rest of the code unchanged. (b) is far smaller; decide in the plan.

## Platform bot-API comparison vs Telegram (2026-09-26)

Verification note: re-fetched core.telegram.org/bots/api — sendMessageDraft exists, can_stop/keep_on_stop in 10.3, and 10.0 "see certain messages sent by other bots in groups" confirmed. The version that introduced sendMessageDraft is inconsistent between sources (subagent: 9.3; fetch summary: 10.0) — unresolved, immaterial.

### Messaging platform comparison for shell (Telegram vs Discord, Slack, WhatsApp, Signal, iMessage, Matrix)

Research date: 2026-09-26. Status: complete. Limits verified against official docs fetched 2026-09-26 unless marked (unconfirmed). Ghost memory (`ghost_context`) was not consulted for this research.

### Checklist


### Feature matrix

Legend: numbers are from the per-platform evidence sections below (each has official URLs). (u) = unconfirmed. "Judgment" rows are my assessment, not docs.

| Dimension | Telegram (today) | Discord | Slack | WhatsApp Cloud API | Signal (signal-cli) | iMessage (imsg/BlueBubbles) | Matrix |
|---|---|---|---|---|---|---|---|
| Official bot API | Yes | Yes | Yes | Yes, but **LLM assistants banned** (§4.7) | No (unofficial client) | No (Mac automation) | Yes (bots are normal users) |
| Inbound transport | getUpdates long-poll or webhook | Gateway websocket (+ optional HTTP interactions) | Socket Mode websocket or Events webhook | Webhook only | signal-cli daemon (JSON-RPC), outbound to Signal | Local chat.db watch on a Mac | /sync long-poll (or appservice) |
| Public endpoint needed | No | No | No (Socket Mode) | **Yes** | No | No (but needs always-on Mac) | No |
| Threads/topics | Forum topics in groups; topics in bot DMs (9.3+) | Threads in text channels; forum channels (posts = threads); **none in DMs** | Reply threads; channels as topics | None (groups ≤8) | None | None (inline replies) | Rooms, Spaces, `m.thread` |
| Maps Telegram topic to | — | Forum post in a private server | Channel | — | Separate group | Separate group chat | Room in a Space |
| Native streaming | `sendMessageDraft` / `sendRichMessageDraft` (DMs only, 30 s ephemeral preview, must finalize with send) | None | `chat.startStream/appendStream/stopStream` (Tier 2) | None | None | None | None in stable spec |
| Edit for streaming | editMessageText; ~1 msg/s/chat, 20/min/group (FAQ; edits in same bucket (u)) | Undocumented; ~5 edits/5 s/channel (community) | chat.update Tier 3 (50+/min) | **No edits** | **10 edits max / 24 h** | **5 edits / 15 min**; needs SIP-off helper | m.replace, homeserver-defined; exemptable if self-hosted |
| Typing indicator | sendChatAction, ≤5 s | POST /typing, 10 s | assistant.threads.setStatus (assistant DM threads only) | 25 s, bundled with read | sendTyping | SIP-off helper only | PUT typing w/ timeout |
| Reactions read | Yes (bot must be group admin + allowed_updates) | Yes (non-privileged intents) | Yes | Yes (webhook) (u) | Yes | Yes (tapbacks via watch) | Yes |
| Reactions write | 1 per message | Unlimited | Yes | Yes (u) | Yes | Standard tapbacks | Unlimited |
| Buttons / components | Inline keyboards, callbacks | Buttons, selects, Components V2 | Block Kit | ≤3 reply buttons / lists (u); none in groups | No | No | No (text commands) |
| Slash commands | /commands via BotFather | Registered slash + context menus (3 s ack, 15 min token) | Slash commands | No | No | No | No (text only) |
| Modals / forms | Mini Apps; no native modal | Modals (text, radio, checkbox, file upload) | Modals | Flows (u) | No | No | No |
| Formatting | MarkdownV2/HTML; Rich Messages (tables, headings) 10.1+ | Markdown subset, code blocks, no tables | mrkdwn / markdown_text / Block Kit | *bold* _it_ ```mono``` | Style ranges (bold/italic/mono/spoiler) | Plain text | HTML subset (tables, code) |
| Max text length | 4096 | **2000** (embeds 4096/6000) | 4,000 text; 12,000 markdown_text; 40k truncation | 4096 (u) | long (u) | long (u) | ~64 KB event (u) |
| Bot upload limit | 50 MB (photos 10 MB) | **20 MiB/file** (since 2026-09-03), 25 MiB/request | ~1 GB (help center (u)) | img 5 MB, audio/video 16 MB, docs 100 MB | ~100 MB (u) | iMessage limits (u) | Server config (Synapse default 50M (u)) |
| Bot download limit | **20 MB via getFile** (local Bot API server lifts) | No bot-side cap (CDN URLs, signed/expiring) | per file size | per media limits | per attachment | local file | server config |
| Voice notes | sendVoice (OGG/Opus, MP3, M4A); receive voice | Send via IS_VOICE_MESSAGE (single audio, waveform+duration, not editable); receive as audio/ogg attachment | Audio files / clips (u) | Audio 16 MB | Yes | Yes (audio attachment) | m.audio (voice flag MSC) |
| PDFs | sendDocument | Attachment | File | Document 100 MB | Attachment | Attachment | m.file |
| Two bots in one group | Yes | Yes | Yes | Impractical | Yes (two numbers) | Yes (two Apple IDs/Macs) | Yes |
| Bots see other bots | Historically no; 10.0 adds "certain messages" (scope (u)); bot-to-bot DMs opt-in | **Yes** (with Message Content intent) | Yes (bot_message) | n/a | Yes | Yes | Yes |
| Message-content gating | Privacy mode (groups): off or admin to see all | MESSAGE_CONTENT privileged; toggle free <10,000 users (since 2026-06-10); DMs & mentions exempt | Scopes only | n/a | none | none | none (E2EE keys needed) |
| Client quality (judgment) | Excellent mobile+desktop, fast sync | Very good desktop, good mobile; busier UI | Excellent desktop, workplace-y mobile | Excellent mobile, weaker desktop | Good; linked desktop | Excellent on Apple only | Element X improving; weakest |
| Push notifications (judgment) | Reliable, per-chat/topic control | Reliable, noisy defaults, per-channel control | Reliable | Excellent | Reliable | Excellent | Depends on push gateway |
| Cross-device sync | Full cloud history | Full cloud history | Full (free: 90 days) | Linked devices (u) | Linked devices, limited history transfer (u) | iCloud Messages | Full (E2EE key backup needed) |
| Account for family member | Phone number | **Email + DOB, no phone** (server may require verified phone) ; age assurance rollout 2026-09 | Workspace invite (email) | Phone (already have it) | Phone number | Apple ID (iPhone) | Homeserver account |
| Cost | Free | Free | Free tier 90-day history, 10 apps; paid per seat | Per-message templates outside 24 h window | Free (+ a phone number) | Free (+ always-on Mac) | Free (self-host costs) |
| ToS risk for personal AI bot | Low | Low (private server, <10k users) | Low | **Prohibited** | Medium (unofficial client, captcha/ratelimit) | Medium (unsupported automation, SIP off for features) | Low |
| Go library | go-telegram/bot et al. | discordgo (std, slow releases), disgo (fresher) | slack-go (streaming supported) | whatsmeow (unofficial) | none native; JSON-RPC to signal-cli | HTTP/stdio to BlueBubbles/imsg | mautrix/go |

### What Discord does better than Telegram (for shell)
- **Multi-bot rooms**: bots reliably see each other's messages (with MESSAGE_CONTENT, free under 10k users). Telegram only added "certain messages" in 10.0, scope undocumented. This matters for the two-bot group setup and any agent-to-agent relay.
- **Richer interaction surface**: registered slash commands with typed options and autocomplete, context-menu commands, modals with text/radio/checkbox/file-upload, selects, Components V2 layouts. Telegram has inline keyboards and Mini Apps but no native modal.
- **Reactions**: unlimited bot reactions (Telegram: 1 per message) — useful for multi-state status (seen/working/done/error). Reading reactions needs no admin rights (Telegram needs bot admin in groups).
- **Downloads**: no 20 MB getFile ceiling; users' large PDFs/photos are fetchable directly.
- **Organization**: a private family server gives categories, forum channels with tags, per-channel permissions and notification control; threads auto-archive rather than cluttering.
- **Onboarding without a phone number** (email + DOB) — easier for kids/tablets; one invite link.
- **Desktop**: better for the technical owner (code blocks with syntax highlighting, search, multi-window).

### What Discord does worse
- **Streaming**: no native draft primitive; edit ceiling is undocumented (~5 per 5 s per channel reported). Telegram now has `sendMessageDraft`/`sendRichMessageDraft` with a stop button (10.3) in DMs — the closest thing to a native "AI streaming" UX of any platform except Slack.
- **2000-char messages** vs 4096 → roughly twice the splitting for long Claude replies (embeds allow 4096 but look different and have no reply-like UX).
- **DMs are flat**: no threads in DMs, and a bot can only DM users who share a server (or user-installed apps). Telegram's private-chat topics (9.3/9.4) give per-topic sessions in a 1:1 chat; on Discord that pattern requires a server + forum channel.
- **Upload cap** 20 MiB per file vs Telegram's 50 MB for bot sends.
- **No formatting tables**; Telegram Rich Messages (10.1) now render tables, headings, math, and "thinking" blocks.
- **Family UX**: Discord's gamer-oriented, busier UI and noisy default notifications are a heavier lift for non-technical members than Telegram's messenger feel; age-assurance rollout (2026-09-22) may filter teen DMs into Message Requests.
- **Policy surface**: privileged-intent review kicks in at 10k users (irrelevant for a family) but Discord changes platform rules frequently (upload limit 25→10→20 MiB within 20 months; channel obfuscation 2026-11-16).
- **Interactions timing**: slash commands/buttons must be acked in 3 s and follow-ups die after 15 min — long Claude turns need a normal channel message, not the interaction token, for the final answer.

### Bottom line
- **Discord** is the only candidate that matches Telegram feature-for-feature and beats it on multi-bot visibility, components and reactions; it loses on native streaming, message length, DM topics and family-friendliness.
- **Slack** is technically strong (the only other native streaming API) but workplace UX and a 90-day free history make it a poor family fit.
- **WhatsApp Cloud API** is ruled out by Meta's §4.7 AI-provider ban (and has no edits, 24 h window, 8-person groups).
- **Signal / iMessage** are unofficial, edit-capped (10 / 5 edits), no buttons/threads — viable only as a "notifications + plain chat" secondary channel.
- **Matrix** has the cleanest protocol and no edit caps if self-hosted, but the weakest family onboarding/client polish and highest ops cost.
- A Discord adapter would sit alongside Telegram rather than replace it: keep Telegram for family DMs (drafts, topics), add Discord for the owner's desktop and multi-agent rooms.

### Evidence appendix (per-platform notes, streamed during research)


### Telegram (baseline) — verified 2026-09-26 against https://core.telegram.org/bots/api and https://core.telegram.org/bots/api-changelog
- Latest Bot API: 10.3 (2026-08-24).
- `sendMessageDraft`: added Bot API 9.3 (2025-12-31), opened to all bots in 9.5 (2026-03-01), empty text = "Thinking…" placeholder in 10.0, `can_stop`/`keep_on_stop` + `stopped_message_generation` update in 10.3. Doc: "stream a partial message to a user … the streamed draft is ephemeral and acts as a temporary 30-second preview – once the output is finalized, you must call sendMessage". `chat_id` is "the target **private** chat" — drafts are DM-only; `message_thread_id` supported (private-chat topics). Groups/forum supergroups still need edit-based streaming.
- `sendRichMessageDraft` + Rich Messages (tables, headings, math, "thinking" blocks) added 10.1 (2026-06-11): "stream AI-generated replies with seamless rich formatting".
- Private-chat topics (bot DMs with forum topics) 9.3; bots can create them 9.4.
- Text: 1-4096 chars after entity parsing (sendMessage, editMessageText). Captions 0-1024.
- Files: upload 50 MB (photos 10 MB) via multipart; URL upload 5 MB photo / 20 MB other; getFile download max 20 MB. Local Bot API server lifts to 2000 MB upload / unlimited download.
- sendChatAction: "set for 5 seconds or less".
- Reactions: bots set up to 1 reaction per message; `message_reaction` updates require bot to be admin in groups + explicit allowed_updates.
- Rate limits (https://core.telegram.org/bots/faq): ~1 msg/s per chat, 20 msgs/min per group, ~30 msg/s broadcast. (Edits are believed to count against the same buckets — not stated explicitly; unconfirmed.)
- Bots seeing bots: FAQ still says "bots will not be able to see messages from other bots regardless of mode", but Bot API 10.0 (2026-05-08) "Added the ability to see certain messages sent by other bots in groups" and bot-to-bot DMs by username if both opt in. Which group messages are visible is **not specified in the changelog — unconfirmed**.
- 10.2/10.3 also added Ephemeral Messages (visible to one user in a group) and ephemeral commands.

### Discord — verified against github.com/discord/discord-api-docs (HEAD cloned 2026-09-26; rendered at https://docs.discord.com/developers/...)
- Transport: Gateway websocket (outbound only, no public endpoint) — https://docs.discord.com/developers/events/gateway. Gateway send limit 120 events / 60 s per connection. HTTP interactions endpoint (webhook) is optional; interactions also arrive over the gateway.
- HTTP rate limits: global 50 req/s; 10,000 invalid (401/403/429) per 10 min → Cloudflare ban; per-route limits are header-driven and intentionally undocumented — https://docs.discord.com/developers/topics/rate-limits
- Edit rate: **not officially documented**. Community reports ~5 edits / 5 s per channel for message create/edit (https://github.com/discord/discord-api-docs/discussions/6310, llmcord uses ~1.2 s throttle). Treat ~1 edit/s as the practical ceiling — similar to Telegram. **No native streaming/draft API** (no equivalent of sendMessageDraft).
- Typing: POST /channels/{id}/typing, "expires after 10 seconds" — https://docs.discord.com/developers/resources/channel#trigger-typing-indicator
- Message: content 2000 chars; embeds: up to 10, description 4096, 6000 chars total — https://docs.discord.com/developers/resources/message. Max request size 25 MiB.
- Uploads: default per-file limit raised 10 MiB → **20 MiB** on 2026-09-03 for users, bots, webhooks, interaction responses (changelog); was 25→10 MiB on 2025-01-16. Limits are per attachment; boosts raise it — https://docs.discord.com/developers/reference#uploading-files. Up to 10 attachments per message (long-standing; not re-verified in current docs text — unconfirmed).
- Voice messages: `IS_VOICE_MESSAGE` flag, single audio attachment with `duration_secs` + `waveform`, cannot be edited; clients use mono 48 kHz Opus/OGG. Bots can send them via flag on create (widely done; docs don't forbid).
- Threads: public/private threads in text channels; forum (type 15) and media (16) channels are thread-only ("posts") — https://docs.discord.com/developers/topics/threads. Auto-archive after inactivity (archived threads can be un-archived by sending). **No threads in DMs** — DMs are flat. Telegram forum topic ≈ Discord forum post (thread) in a private server; Telegram private-chat topics have no Discord DM equivalent.
- Replies: `message_reference`; forwards require message content access (2026-04-14).
- Reactions: read (MESSAGE_REACTION_ADD events, GUILD_MESSAGE_REACTIONS / DIRECT_MESSAGE_REACTIONS intents, non-privileged) and write (any number of reactions, unlike Telegram's 1).
- Interactions: slash commands, message/user context menus, buttons, selects, modals (text inputs, radio/checkbox groups Feb 2026, file upload in modals Oct 2025), Components V2 layouts. Must ack within **3 s**; token valid **15 min** for follow-ups/edits — https://docs.discord.com/developers/interactions/receiving-and-responding. Slash command UX improved on mobile 2026-09-11.
- Markdown: Discord subset (bold/italic/underline/strike, headers, lists, code blocks with language, block quotes, spoilers, masked links, subtext) — https://docs.discord.com/developers/reference#message-formatting. No tables.
- Message Content intent (privileged): without it `content`, `embeds`, `attachments`, `components`, `poll` are empty EXCEPT own messages, DMs with the app, messages mentioning the app. **Since 2026-06-10 review threshold is 10,000 users (was 100 servers)**; below that, toggle in portal. Annual re-application for those above. — https://docs.discord.com/developers/events/gateway#privileged-intents
- Bots see other bots: yes — MESSAGE_CREATE is dispatched for all messages incl. other bots (`author.bot=true`); with MESSAGE_CONTENT they get content. Two bots in one guild is routine. Loop-prevention is the app's job.
- Upcoming: channel obfuscation for channels the bot can't view, mandatory 2026-11-16 (irrelevant if bot only acts where it has access). Age assurance global rollout 2026-09-22: teen accounts get DMs filtered to Message Requests more often — relevant for kids in the family.

### Slack — verified via docs.slack.dev (2026-09-26)
- Transport: Socket Mode websocket, no public endpoint; up to 10 concurrent connections; Socket Mode apps can't be in the public Marketplace (irrelevant for a private app) — https://docs.slack.dev/apis/events-api/using-socket-mode. Or Events API webhook (needs public HTTPS).
- **Native streaming**: `chat.startStream` / `chat.appendStream` / `chat.stopStream` (Tier 2, 20+/min); `markdown_text` up to 12,000 chars; works in channels/threads/DMs (channels need `recipient_user_id` + `recipient_team_id`); supports task/plan update chunks — https://docs.slack.dev/reference/methods/chat.startStream
- Status/typing: `assistant.threads.setStatus` (600/min) — only in AI-assistant DM threads, errors elsewhere — https://docs.slack.dev/reference/methods/assistant.threads.setStatus. No general bot typing indicator in channels (RTM-only historically; unconfirmed for modern apps).
- Edit: `chat.update` Tier 3 (50+/min), text ≤4,000 chars — https://docs.slack.dev/reference/methods/chat.update
- Post: 1 msg/s per channel; text truncates at 40,000 (4,000 recommended); `markdown_text` 12,000; `thread_ts` for threads — https://docs.slack.dev/reference/methods/chat.postMessage
- Threads: reply threads on any message (no named topics); channels are the "topic" unit. Telegram forum topic ≈ Slack channel (or long-lived thread).
- Formatting: mrkdwn (non-standard) or `markdown_text` (standard markdown), Block Kit (50 blocks), code blocks.
- Interactivity: buttons, selects, modals, slash commands, reactions read (reaction_added) and write (reactions.add). Bots receive other bots' messages (`bot_message` / `bot_id`).
- History API: non-Marketplace commercially distributed apps limited to 1 req/min on conversations.history/replies since 2025-05-29; **internal apps exempt** (50+/min) — https://docs.slack.dev/changelog/2025/05/29/rate-limit-changes-for-non-marketplace-apps
- Files: upload via files.getUploadURLExternal; 1 GB per file per Slack Help Center (not stated in API guide — unconfirmed at API level).
- Cost: free plan = 90-day history, data >1 yr deleted, 10 app installs max — https://slack.com/help/articles/27204752526611. Paid Pro is per-seat (price not verified). Workplace-oriented UX; poor fit for non-technical family.

### WhatsApp Cloud API — verified 2026-09-26
- **ToS blocker**: Meta Terms for WhatsApp Business Platform §4.7 (current version effective 2026-09-23): "Providers and developers of artificial intelligence or machine learning technologies, including … large language models, generative artificial intelligence platforms, general-purpose artificial intelligence assistants … ("AI Providers"), are strictly prohibited from accessing or using the WhatsApp Business Platform" where that is the primary functionality — https://www.facebook.com/legal/Meta-Terms-for-WhatsApp-Business-Platform . General-purpose chatbot ban enforced since 2026-01-15 (https://techcrunch.com/2025/10/18/whatssapp-changes-its-terms-to-bar-general-purpose-chatbots-from-its-platform). A Claude Code family assistant is squarely a general-purpose assistant → **disqualifying** on the official API.
- Transport: webhooks only → needs public HTTPS endpoint (Meta Graph API).
- 24-hour customer service window: free-form messages only within 24 h of the user's last message; proactive/scheduled messages outside it require pre-approved paid templates (per-message pricing). Pricing specifics not re-verified — unconfirmed.
- Typing indicator: 25 s or until reply, bundled with mark-as-read — https://developers.facebook.com/docs/whatsapp/cloud-api/typing-indicators
- Edits: not supported for business messages (groups doc explicitly: editing/deletion not supported) → no edit-streaming at all.
- Groups API: Official Business Account only, max 8 participants, invite-link join, no interactive messages, no edit/delete — https://developers.facebook.com/documentation/business-messaging/whatsapp/groups . Two bots in one group: impractical.
- Media: images 5 MB (JPEG/PNG), audio 16 MB (AAC/AMR/MP3/OGG), video 16 MB, documents incl. PDF 100 MB — https://developers.facebook.com/docs/whatsapp/cloud-api/reference/media
- Text body 4096 chars; formatting = WhatsApp's *bold* _italic_ ~strike~ ```mono```; interactive buttons (≤3 reply buttons) and lists (widely documented; not re-verified).
- Unofficial route: whatsmeow (Go, multi-device web protocol, used by mautrix-whatsapp) on a personal/secondary number — ToS violation, ban risk. Family has WhatsApp already (zero onboarding) but that's the only upside.

### Signal via signal-cli — verified 2026-09-26
- No official bot API. signal-cli (Java, unofficial; latest v0.14.8, 2026-09-10) registers or links a device for a real phone number; exposes `daemon` with JSON-RPC (socket/TCP/HTTP) and D-Bus — https://github.com/AsamK/signal-cli/blob/master/man/signal-cli.1.adoc . No public endpoint needed (outbound to Signal servers).
- Commands present: send (with `--mention`, `--text-style` BOLD/ITALIC/… ranges, quotes/replies, `--edit-timestamp`, `--no-urgent`), sendReaction, sendTyping, sendPollCreate/Vote/Terminate, sendPinMessage, remoteDelete, sendStory, group management, getAttachment.
- **Edits capped**: "edit any message you have originally sent up to 10 times within 24 hours" — https://support.signal.org/hc/en-us/articles/6255134251546-Edit-Message → edit-streaming is effectively impossible (10 edits); must send final message or a few coarse chunks.
- No threads/topics, no buttons/inline keyboards, no slash commands, no modals. Formatting = style ranges (no markdown rendering; bridge must convert).
- Attachments: ~100 MB per attachment (Signal client limit; not re-verified — unconfirmed). Voice notes supported as attachments.
- Needs a dedicated phone number for the bot (SIM, VoIP or landline with voice verification). Family needs Signal installed + phone number. Signal servers can rate-limit/captcha-challenge automated accounts (`submitRateLimitChallenge` exists) — ToS risk moderate (bots not officially supported, but personal automation widely tolerated — unconfirmed policy). Two bots in one group: possible (two numbers), each sees all group messages (E2EE, member-level).
- Go: no mature native Go Signal client; talk to signal-cli JSON-RPC over HTTP/socket (or signal-cli-rest-api). mautrix-signal uses libsignal via Go/Rust FFI (complex).

### iMessage via imsg / BlueBubbles — verified 2026-09-26
- Requires an always-on **Mac** signed into an Apple ID. imsg (Swift CLI, macOS 14+, ~1.3k stars, active 2026-09-25) reads `chat.db`, streams via `watch`, sends via Messages.app AppleScript, JSON-RPC over stdio — https://github.com/steipete/imsg (now openclaw/imsg). Standard send + tapbacks without private APIs; **typing indicators, read receipts, edits, rich sends require an injected helper with SIP disabled**, "may be blocked … on current macOS releases".
- BlueBubbles server (REST + websocket) likewise gates reactions, typing, edit, unsend, replies behind its Private API (edit/unsend need macOS 13+) — https://docs.bluebubbles.app/private-api (SIP-disable requirement per BlueBubbles docs; not in fetched excerpt — unconfirmed text).
- Edits: "up to five times within 15 minutes" — https://support.apple.com/en-us/105083 → no edit-streaming.
- No threads/topics (inline replies only), no buttons, no commands, no markdown (plain text; iOS 18 has limited text effects/formatting — unconfirmed via automation).
- Onboarding: zero for iPhone users; Android family members fall back to SMS/RCS (degraded). Group chats with the bot's Apple ID work; two bots = two Apple IDs = two Macs (or two users).
- ToS: automating Messages.app on your own Mac with your own Apple ID is grey but low-risk personal use; Apple offers no bot API (Apple Messages for Business is enterprise-only). Fragile across macOS updates.

### Matrix — spec v1.19 (https://spec.matrix.org/latest/client-server-api/)
- Transport: bot is an ordinary client doing `/sync` long-poll (no public endpoint), or an appservice (needs homeserver→bot HTTP push; only if you run the homeserver).
- Edits: `m.replace` event replacements (#event-replacements) — no spec-level count/time limit; rate limit is homeserver-defined (`M_LIMIT_EXCEEDED`); on a self-hosted Synapse the bot can be exempted → **best edit-streaming ceiling of all options** if self-hosted. No native streaming primitive in the stable spec (MSC-level work — unconfirmed).
- Threads: `m.thread` (#threading); rooms are the topic unit; Spaces group rooms. Telegram forum = Matrix Space, topic = room (or thread).
- Reactions: `m.annotation` (#event-annotations-and-reactions), read + write, unlimited.
- Typing: `PUT /rooms/{id}/typing/{user}` with timeout (#typing-notifications).
- Formatting: `formatted_body` with `org.matrix.custom.html` (HTML subset: code blocks, tables render in Element). No length limit beyond 65,536-byte event size (spec PDU limit — not re-fetched; unconfirmed).
- Buttons/modals/slash commands: none native (client-side commands only; bots parse `!cmd` text; widgets are heavy). 
- Media: server-configured `m.upload.size` via `GET /_matrix/client/v1/media/config`; Synapse default 50M (unconfirmed); authenticated media required.
- E2EE: default-on in DMs in Element; bot must implement Olm/Megolm (mautrix-go has `crypto` package). Bots see all room events incl. other bots.
- Client quality: Element X (mobile) + Element Web/Desktop; historically weaker push/sync reliability and UX than Telegram/Discord; family needs an account on a homeserver (matrix.org or self-hosted) + app. Push requires Sygnal/ntfy or matrix.org's push gateway. Highest ops burden (homeserver) unless using matrix.org.
- Go: mautrix/go (v0.31.0, 2026-09-16, very active, powers all mautrix bridges) — most mature Go Matrix SDK.

### Go libraries (GitHub API, 2026-09-26)
| Library | Stars | Last push | Latest release | Notes |
|---|---|---|---|---|
| bwmarrin/discordgo | 5,990 | 2026-02-14 | v0.29.0 (2025-05-24) | De-facto standard; has Components V2 flag, voice-message flag, FileUpload component; no Radio/Checkbox (Feb 2026 modal components) found in source; slow release cadence |
| disgoorg/disgo | 615 | 2026-09-23 | v0.19.6 (2026-06-07) | Modern, typed, tracks new API faster (radio/checkbox/file-type filters present); pre-1.0 API churn |
| diamondburned/arikawa | 599 | 2026-05-18 | v3.6.0 (2025-09-04) | Solid, less active |
| slack-go/slack | 4,965 | 2026-09-23 | v0.29.0 (2026-08-15) | Supports chat.startStream/appendStream (MsgOptionStartStream), Socket Mode |
| mautrix/go | 661 | 2026-09-26 | v0.31.0 (2026-09-16) | Matrix incl. E2EE |
| tulir/whatsmeow | 7,417 | 2026-09-25 | — | Unofficial WhatsApp Web multi-device; ToS risk |
| AsamK/signal-cli (Java) | 4,935 | 2026-09-22 | v0.14.8 (2026-09-10) | Call via JSON-RPC; bbernhard/signal-cli-rest-api (2,852) wraps it |
| BlueBubblesApp/bluebubbles-server | 1,092 | 2026-09-19 | — | REST/WS; call from Go over HTTP |
| steipete/imsg (openclaw/imsg) | 1,342 | 2026-09-25 | — | Swift CLI; JSON-RPC over stdio |
| go-telegram/bot (reference) | 1,844 | 2026-09-11 | — | for comparison |

## Harness survey: Hermes, OpenClaw, OpenHuman, Muse, NanoClaw (2026-09-26)

Verification note: re-fetched Hermes gateway/platforms/ADDING_A_PLATFORM.md — required/optional adapter methods and "degrade gracefully to plain text" confirmed.

### Agent harnesses and messaging platforms (research, 2026-09-26)

Checklist

### Initial leads
- Hermes gateway docs: https://hermes-agent.nousresearch.com/docs/user-guide/messaging/
- OpenClaw channels: https://docs.openclaw.ai/channels
- OpenHuman: https://github.com/tinyhumansai/openhuman
- "Muse" candidates: OpenMuse (open clone of Meta's Muse) https://github.com/OpenMuseAgent/OpenMuse ; MuseBot https://github.com/yincongcyincong/MuseBot ; M.U.S.E https://github.com/A-C-I-SOFTWARE-AND-DEVELOPMENT/M.U.S.E

---
### 1. Hermes Agent (Nous Research)

Repo: https://github.com/NousResearch/hermes-agent · Docs: https://hermes-agent.nousresearch.com/docs/user-guide/messaging/

### Platforms
- Docs list 20-30+: Telegram, Discord, Slack, WhatsApp (Baileys bridge and Meta Cloud API), Signal, SMS, Email, Home Assistant, Mattermost, Matrix, DingTalk, Feishu/Lark, WeCom, Weixin, BlueBubbles (iMessage), QQ, Yuanbao, Teams, LINE, ntfy, SimpleX, IRC, plus Hermes Desktop / terminal / browser. https://hermes-agent.nousresearch.com/docs/user-guide/messaging/
- No declared default, but Telegram gets "mobile-optimized defaults" (`tool_progress` off, `busy_ack_detail` off "to keep mobile chats uncluttered"). Same page. Their flagship tutorial is "Team Telegram Assistant" https://hermes-agent.nousresearch.com/docs/guides/team-telegram-assistant
- Tagline: "one agent, one memory, every surface" (hermes-agent.nousresearch.com home page).

### Architecture
- One gateway process: "a single background process that connects to all your configured platforms, handles sessions, runs cron jobs, and delivers voice messages." Each adapter routes into a per-chat session store; cron ticks every 60s. Circuit breaker per adapter, delivery ledger with at-least-once semantics. (messaging docs)
- Adapter interface: `BasePlatformAdapter` in `gateway/platforms/base.py` (~270 KB). Required: `connect`, `disconnect`, `send(chat_id, text) -> SendResult`, `send_typing`, `send_image`, `get_chat_info`. Optional with default stubs: `send_document`, `send_voice`, `send_video`, `send_animation`, `send_image_file`. Interactive UX methods that "degrade gracefully to plain text when not overridden": `send_clarify`, `_send_exec_approval_prompt`, `send_slash_confirm`, `send_model_picker`, `send_choice_picker` (Telegram inline keyboard, Discord select menu, Matrix via reactions). Shared button callback-id convention `cl:<id>:<idx>`, `appr:<id>:<choice>`. `MAX_MESSAGE_LENGTH` per adapter. Source: https://github.com/NousResearch/hermes-agent/blob/main/gateway/platforms/ADDING_A_PLATFORM.md
- Plugin path: `~/.hermes/plugins/<x>/plugin.yaml + adapter.py`, registers with `ctx.register_platform()`, "zero changes to core". Newer platforms (IRC, Teams, Google Chat, LINE) live in `plugins/platforms/`. Platform-specific time-window quirks (LINE 60s reply token, WhatsApp 24h window) handled by overriding `_keep_typing`. (same file)
- Two transports for one platform share a behavior mixin (`whatsapp_common.py` for Baileys + Cloud API). (same file)
- Session key (`gateway/session.py::build_session_key`): `<ns>:<platform>:<chat_type>[:<slack scope>][:<chat_id>][:<thread_id>][:<user>]`. DMs isolated per chat; groups per-user by default (`group_sessions_per_user`), threads shared unless `thread_sessions_per_user`. Discord auto-thread continuity uses a "prospective_thread_id" so the channel-initiating message and in-thread follow-ups share a key. https://github.com/NousResearch/hermes-agent/blob/main/gateway/session.py . Implication: sessions are per-platform/per-chat; cross-platform continuity comes from shared memory, not a shared transcript (inferred from the key layout; `/sessions` lets you resume past sessions).
- Streaming: progressive message edits on platforms that support editing (Telegram, Discord, Slack, Matrix, Mattermost); Telegram can also use native `sendMessageDraft` in DMs. Rich messages downgrade to MarkdownV2 if rejected. https://hermes-agent.nousresearch.com/docs/user-guide/messaging/telegram
- Telegram specifics: forum topics map to separate conversations; a `/topic` "multi-session DM mode" where the root DM becomes a "lobby"; 20 MB download cap unless you run a local Bot API server ("raises the file ceiling to 2 GB"); privacy mode must be off for groups; multiple bots in one group need `exclusive_bot_mentions: true`; `observe_unmentioned_group_messages`. (telegram docs)
- Discord specifics: Message Content privileged intent required ("the bot literally cannot see what you typed" without it); auto-thread per @mention by default; per-user session inside shared channels by default; skills auto-register as slash commands (100-command cap); `clarify` as buttons; voice-channel participation; 32 MiB attachment default; reaction status 👀/✅/❌. https://hermes-agent.nousresearch.com/docs/user-guide/messaging/discord

---
### 2. OpenClaw (formerly Clawdbot / Moltbot)

Repo: https://github.com/openclaw/openclaw · Docs: https://docs.openclaw.ai/channels

### Platforms
- Bundled/core: Telegram (bundled plugin), WebChat (core), A2A, Reef. Official plugins (~27): Discord, Slack, WhatsApp (QR pairing, Baileys), Signal, iMessage (via `imsg`, JSON-RPC over stdio; tapbacks, effects, polls), Matrix, Teams, Google Chat, IRC, LINE, Mattermost, Nextcloud Talk, Nostr, Twitch, Zalo, Feishu, SMS, etc. https://docs.openclaw.ai/channels
- Default/recommended: Telegram. Docs: "It needs a bot token and no plugin install, so it is the fastest channel to get working." (https://docs.openclaw.ai/channels). Extensions dir confirms separate packages per channel: https://github.com/openclaw/openclaw/tree/main/extensions
- Docs: "Text is supported everywhere; media and reactions vary by channel." Shared "bot loop protection to prevent bot pairs from replying to each other indefinitely" (relevant to our two-bot family group).

### Architecture
- One Gateway process is "the single source of truth for sessions, routing, and channel connections" (https://docs.openclaw.ai/).
- Channel plugin contract: `ChannelPlugin` in `src/channels/plugins/types.plugin.ts` is a bag of optional adapters: `config`, `setupContract`, `pairing`, `security`, `groups`, `mentions`, `outbound`, `status`, `gateway`, `auth`, `approvalCapability`, `commands`, `lifecycle`, `allowlist`, `streaming`, `threading`, `message`, `messaging`, `agentPrompt`, `directory`, `actions`, `heartbeat`, `agentTools`. https://github.com/openclaw/openclaw/blob/main/src/channels/plugins/types.plugin.ts
- Capability declaration: `ChannelCapabilities = { chatTypes, polls, reactions, edit, unsend, reply, effects, groupManagement, threads, media, tts, nativeCommands, blockStreaming }` in `src/channels/plugins/types.core.ts`. Core degrades by checking these flags. https://github.com/openclaw/openclaw/blob/main/src/channels/plugins/types.core.ts
- Plugins must go through `openclaw/plugin-sdk/*`, not import core (`src/channels/AGENTS.md`).
- Sessions: `session.dmScope` default `main` ("All DMs share the main session" — i.e. cross-channel continuity by default); options `per-peer`, `per-channel-peer` ("recommended"), `per-account-channel-peer`. Groups: `session.groupScope` default `per-group`. Docs warn: "If multiple people can message your agent, enable DM isolation." https://docs.openclaw.ai/concepts/session
- Streaming: two layers. Block streaming (coarse chunks as separate messages, default off) and preview streaming (edit a temp message). "Discord defaults to `off` when `streaming` is unset, Telegram and Slack default to `progress`, and Mattermost and MS Teams default to `partial`." Slack can use native `chat.startStream/appendStream/stopStream`. Preview chunks default `minChars: 200, maxChars: 800`. https://docs.openclaw.ai/concepts/streaming
- Open issue (2026-09-20) asks to adopt Telegram Bot API 10.x `sendMessageDraft` / `sendRichMessageDraft` and the native Stop button (`can_stop` / `stopped_message_generation`) instead of `editMessageText`: "the client renders it as regular message edits - no generating animation, no Stop button". https://github.com/openclaw/openclaw/issues/153677
- Album handling: Telegram sends each photo of an album as a separate update (with `media_group_id`); "Discord / Slack: Messages can contain multiple attachments natively, so this is less of an issue." https://github.com/openclaw/openclaw/issues/39343
- Discord specifics (https://docs.openclaw.ai/channels/discord , https://docs.openclaw.ai/channels/discord/threads-and-sessions): threads are "channel sessions" that "inherit parent channel config unless overridden"; guild `historyLimit` default 20 (0 "disables automatic guild/thread history reads"); outbound `textChunkLimit` default 2000 and `maxLinesPerMessage` default 17; "Discord DMs default to pairing mode"; persistent thread bindings for subagents (`session.threadBindings.enabled`, `idleHours`, `maxAgeHours`; `/session unbind`); `/acp spawn codex --bind here` binds a channel to a persistent ACP session. Docs do not say what happens to a session when Discord auto-archives an inactive thread (not confirmed; checked both pages).
- Identity discussion (2026-07-27): "Most of us who deploy OpenClaw in practice don't expose the Control UI to end users at all — the bot in Feishu/Telegram/Discord *is* the product." https://github.com/openclaw/openclaw/issues/114789

---
### 3. OpenHuman (tinyhumansai)

Repo: https://github.com/tinyhumansai/openhuman (~40k stars per GitHub API, created 2026-02-18; Rust core, Tauri v2 desktop app). Channels doc: https://tinyhumans.gitbook.io/openhuman/features/channels

### Platforms
- "15 messaging channels: Telegram, Discord, Slack, WhatsApp, Signal, iMessage… plus native email (IMAP IDLE + SMTP)" (README). Full list in docs: Telegram (Bot API long-poll), Discord (Gateway), Web (in-app), iMessage (AppleScript + Full Disk Access), Lark/Feishu, DingTalk, Yuanbao, Slack, WhatsApp (Meta Cloud webhook; WhatsApp Web optional), IRC, Signal (signal-cli REST), Mattermost, QQ, Linq (SMS), Email.
- Default is the desktop app's own chat: "The default is the in-app **Web** chat until you change it." Docs also say "Telegram is the most fully featured channel". https://tinyhumans.gitbook.io/openhuman/features/channels
- Desktop-first product (Tauri app, "Desktop + CLI"); messaging is an add-on reach channel. README comparison table.

### Architecture
- "every channel implements one small Rust contract (a `send` path for outbound messages and a `listen` path for inbound ones)". Inbound normalized into `ChannelMessage` "(sender, reply target, content, optional thread id)" → dispatch loop spawns/resumes agent runs. "Capabilities are declared per channel, not assumed" (typing, progressive draft updates, reactions). Threading: Discord and Lark. (channels doc)
- The contract lives in a separate crate, TinyChannels (`Channel`, `ChannelMessage`, `SendMessage`; `ChannelBackend`/`ChannelManager`), https://github.com/tinyhumansai/tinychannels . Telegram provider is the largest (`src/providers/telegram/{approval,attachments,remote_control,session_store,...}.rs`). Harness types include `ChannelTurn { session_key, envelope, admission, lifecycle }` (`src/harness/types.rs`).
- User sentiment: I did not find OpenHuman-specific user discussion comparing channels (looked at HN Algolia and web search). Unconfirmed.

---
### 4. "Muse" (ambiguous; three candidates)

1. **Meta Muse** (most likely what's meant; launched 2026-09-08). Consumer personal agent, not open source. Surfaces: own app on iOS/Android, muse.ai web, Mac app (Sep 2026), glasses "coming soon". Chat surface: "talking to it works just like messaging another person, in the Muse app or directly in WhatsApp." It works async: "keeps working after people close the app, and comes back when something changes or when it needs approval". https://about.fb.com/news/2026/09/introducing-muse-personal-ai-agent/ ; Mac: https://9to5mac.com/2026/09/17/meta-ai-launches-muse-personal-agent-including-a-new-mobile-app-for-iphone/ ; https://techcrunch.com/2026/09/23/everything-new-coming-to-metas-ai-agent-muse/ . Takeaway: the big consumer player chose native app first + the family messenger it owns (WhatsApp); no Telegram/Discord.
2. **OpenMuse → nanoMuse**: open clone of Meta's Muse, "a personal agent with a phone app, a Sentinel gatekeeper, a credential vault, memory and goals"; "one long conversation with your agent, plus side chats for separate tasks". Tiny (4 stars), created 2026-09-22, already moved to https://github.com/nano-muse/nanoMuse . https://github.com/OpenMuseAgent/OpenMuse . Own phone app, not a messaging bridge.
3. **MuseBot** (yincongcyincong, ~1.6k stars): multi-LLM chat bot for Telegram, Discord, Slack, Lark, DingTalk, WeCom, QQ, WeChat. A bot, not an agent harness. https://github.com/yincongcyincong/MuseBot
   (Also M.U.S.E by A-C-I, 3 stars, claims "20+ messaging platform gateways" — negligible.)

---
### 5. NanoClaw (nanocoai/nanoclaw, ~31k stars, pushed 2026-09-26)

Repo: https://github.com/nanocoai/nanoclaw . Runs Claude Code via the Claude Agent SDK in per-agent containers — closest architectural cousin to shell.

- Channels: WhatsApp, Telegram, Discord, Slack, Teams, iMessage, Matrix, Google Chat, Webex, Linear, GitHub, WeChat, email (Resend). Not in trunk: "Channels ... live on a long-lived `channels` branch"; you run `/add-telegram` etc. and a Claude Code skill copies the adapter into your fork ("Skills over features"). Setup script pairs "your first channel (Slack, Telegram, Discord, WhatsApp, iMessage, or a local CLI)". Trigger word default `@Andy`. README.
- Isolation model per channel: own agent, shared agent with separate conversations, or "fold multiple channels into a single shared session so one conversation spans many surfaces" (`/manage-channels`, docs/isolation-model.md).
- Routing: "user → messaging group → agent group → session", inbound written to per-session `inbound.db`, container writes `outbound.db`, host delivers via adapter. README.
- Adapter contract `src/channels/adapter.ts` (https://github.com/nanocoai/nanoclaw/blob/main/src/channels/adapter.ts): `ChannelAdapter { name, channelType, instance?, supportsThreads, setup(ChannelSetup), teardown, isConnected, deliver(platformId, threadId, OutboundMessage) -> messageId, setTyping?, syncConversations?, resolveConversation?, ... }`. `ChannelSetup` callbacks: `onInbound(platformId, threadId, msg)`, `onMetadata`, `onAction(questionId, selectedOption, userId)` for button clicks. `ChannelDefaults` per DM/group: `engageMode: 'pattern'|'mention'|'mention-sticky'`, `threads`, `sessionMode: 'shared'|'per-thread'`, `unknownSenderPolicy`, `mentions: 'platform'|'dm-only'|'never'`.
- Notable modeling choice: `supportsThreads` true for "Discord, Slack, Linear, GitHub. One thread = one session"; false for "Telegram, WhatsApp, iMessage. Thread ids are stripped at the router". (So NanoClaw does not treat Telegram forum topics as sessions, unlike shell and Hermes.)
- Discord/Slack/etc. are wrapped via Vercel's Chat SDK (`npm i chat`): "Chat SDK bridge — wraps a Chat SDK adapter ... Used by Discord, Slack, and other Chat SDK-supported platforms." `src/channels/chat-sdk-bridge.ts`. Chat SDK: https://github.com/vercel/chat (adapters for Slack, Teams, Google Chat, Discord, Telegram, GitHub, Linear, WhatsApp; JSX cards render natively per platform).

---
### 6. Claude Code Channels (Anthropic, research preview)

Docs: https://code.claude.com/docs/en/channels . Directly relevant because shell wraps the same CLI.
- "Telegram, Discord, and iMessage are included in the research preview." Each is an MCP-server plugin (Bun) that pushes events into an already-running session; replies go out via the plugin's `reply` tool. Source: https://github.com/anthropics/claude-plugins-official/tree/main/external_plugins
- Pairing: bot replies with a code, `/telegram:access pair <code>`, then `policy allowlist`. Discord needs Message Content Intent + OAuth invite with View Channels, Send Messages, Send Messages in Threads, Read Message History, Attach Files, Add Reactions. iMessage reads `~/Library/Messages/chat.db` (Full Disk Access) and sends via AppleScript; "texting yourself bypasses the gate".
- Permission relay: channels can forward tool-permission prompts so you approve from the phone.
- One session, one process: "Events only arrive while the session is open". No per-chat session mapping (it's a single session bridge), unlike shell.
- HN (2026-04-20): "Claude Code does now have integration with Telegram, Discord, and iMessage as of a few weeks ago ... I haven't used OpenClaw since then" https://news.ycombinator.com/item?id=47833008

---
### 7. Hermes follow-ups (issues that show where the pain is)

- Issue volume by platform label in hermes-agent (GitHub search API, 2026-09-26; issues only): telegram 1136 total / 524 open; discord 469 / 235; whatsapp 262; slack 231; matrix 135; signal 44. Proxy for usage + bug surface, not satisfaction. Clickable: https://github.com/NousResearch/hermes-agent/issues?q=label%3Aplatform%2Ftelegram+is%3Aissue and https://github.com/NousResearch/hermes-agent/issues?q=label%3Aplatform%2Fdiscord+is%3Aissue
- OpenClaw issues+PRs by `channel: <x>` label (same date; e.g. https://github.com/openclaw/openclaw/issues?q=label%3A%22channel%3A+telegram%22 and https://github.com/openclaw/openclaw/issues?q=label%3A%22channel%3A+discord%22 ): telegram 5156, discord 3892, slack 3002, whatsapp-web 2500, matrix 2072, signal 1523, imessage 1509. Telegram leads in both repos; Discord is a clear #2.
- Discord adapter pain (2026-08-05 meta-issue): "136 open issues carry the `platform/discord` label"; the adapter `plugins/platforms/discord/adapter.py` is "10,138 lines on main — a god-file"; "whole Discord API families (interactions, components, forum lifecycle, voice state machine) are half-expressed"; operator pain: voice reliability, "attachment routing bugs, streaming duplicates". https://github.com/NousResearch/hermes-agent/issues/79564
- Telegram formatting bug: in forum topics, replies over 4096 chars with bold/code fences "arrive with literal `**`, backtick, and triple backtick markers" because chunking splits inside entities. https://github.com/NousResearch/hermes-agent/issues/43441 (closed 2026-06). Same class of bug in other projects: https://github.com/q15co/q15/issues/159 , https://github.com/Abilityai/trinity/issues/2277
- Cross-platform continuity is a real ask: "A user messaging `@Philgram_bot` on Telegram asking 'what did we discuss on my PC?' receives no useful answer" — sessions keyed per platform. https://github.com/NousResearch/hermes-agent/issues/4335 (open since 2026-03-31)
- Topic → profile routing for Telegram forum topics (one bot, many specialized agents), because otherwise "users must run N separate bots". https://github.com/NousResearch/hermes-agent/issues/10143

---
### 8. What users say (desktop vs mobile, per platform)

Caveat: Reddit JSON and answeroverflow.com (OpenClaw's Discord archive, e.g. "is discord better than telegram for openclaw multi agents?" https://www.answeroverflow.com/m/1479098198625620038 and "Telegram vs Discord" https://www.answeroverflow.com/m/1471621352825032704) were blocked (Reddit refused, answeroverflow returned HTTP 429 / Vercel checkpoint via WebFetch, curl, and the user's Chrome; I did not try to get past the bot check). Those threads exist but I could not read them. Quotes below are from HN, GitHub, and blogs.

### Telegram
- Mobile-first "text it from anywhere" is the core draw:
  - "I guess it's mostly the telegram interface I like. Most of the time I don't have my laptop with me, and ssh using Termux, and opening a remote claude code is a much greater hassle than sending a telegram" — https://news.ycombinator.com/item?id=47103731 (2026-02-21)
  - "Having Claude in WhatsApp/Telegram is actually life-changing for quick tasks" / "Setup is intimidating for non-technical folks" — https://news.ycombinator.com/item?id=46841127 (2026-01-31)
  - "I can text it from my phone while I'm on the toilet. Very important. OpenClaw meets this definition, but so does a 50 line Telegram wrapper around Claude Code" — https://news.ycombinator.com/item?id=46957508
  - Telegram "felt good on mobile" — Mazaika (below).
- Forum topics as the answer to threads: "telegram has this supergroup feature that enable topics, so you can have multiples chats with the bot" https://news.ycombinator.com/item?id=46837672 ; "UI mainly sits on telegram group with topics" https://news.ycombinator.com/item?id=48520435 ; "Telegram integration with multiple agents in one channel using topics" https://news.ycombinator.com/item?id=48458887
- Pain: 4096-char limit + entity-safe splitting (Hermes #43441 above); 20 MB bot download cap unless you self-host the Bot API server (Hermes docs); albums arrive as N separate updates (OpenClaw #39343); edit-based streaming lacks native "generating" UX until Bot API 10.x drafts (OpenClaw #153677). Group privacy mode must be off for bots in groups (Hermes docs).
- Trust: "Telegram has real trust baggage right now — it was pulled from the App Store for a few hours in August 2026 over a CSAM report" — Mazaika (single source; not independently verified).

### Discord
- Wins on structure: "threads contain projects while channels scatter them" — Ken Mazaika, "Discord vs Telegram for Agentic Work (Hermes & OpenClaw)", 2026-08-30, https://engineering.kenmazaika.com/blog/discord-vs-telegram-agentic-work/ . He went Discord (#general only) → Telegram (4 channels) → Discord (channels per area, thread per project). Verdict: Discord for agentic work.
- Loses on: setup (developer portal, privileged Message Content intent, OAuth invite, server); message length 2,000 chars (4,000 with Nitro) vs Telegram 4,096; voice dictations over the limit truncate. His fix: the "file-baton pattern": "write it to a file, hand the file to the thread, and the thread holds the pointer while the file holds the content." Note: his Telegram setup used separate chats/channels, not forum topics; he doesn't mention topics at all (checked).
- Richer UI: "native buttons, dropdowns, forms, threads, and presence status"; "users running multi-agent setups generally prefer Discord"; Discord "Mobile: Available but desktop-focused"; setup 20-30 min vs ~5 for Telegram; "If none of those jump out at you, pick Telegram. It's the path most people take and rarely regret." — https://www.stack-junkie.com/blog/which-chat-app-works-best-with-openclaw (SEO blog; its "community feedback on X" is uncited.)
- Community projects built on Discord threads: "Each opencode project is a Discord channel. Start sessions by creating threads. Supports voice channels" https://news.ycombinator.com/item?id=46048866
- Frustration with harnesses, not Discord per se: "I just want claude code accessible through Discord (or Telegram or anyth[ing])" https://news.ycombinator.com/item?id=48591502
- Mobile vs desktop: MindStudio claims no notification difference ("Both use standard push notifications ... No difference") https://www.mindstudio.ai/blog/claude-code-channels-vs-openclaw-mobile-agent-control — no user evidence cited. I found no first-hand user complaint specifically about the Discord mobile app for agent use (unconfirmed either way).

### WhatsApp
- Pitched as the family choice ("If you want a shared family assistant everyone already knows how to reach, WhatsApp is the frictionless choice" — installer-vendor blog https://www.openclawinstall.ai/blog/ai-agent-chat-telegram-whatsapp-discord/ ). Meta Muse also chose WhatsApp.
- But unofficial (Baileys) path risks bans: "Users have reported bans within 48 hours of connecting"; advice is a dedicated number. https://openclaw.direct/blog/whatsapp-ban-openclaw-telegram-setup , https://dev.to/thegdsks/openclaw-and-moltbook-3600month-whatsapp-bans-and-923-exposed-gateways-an-engineers-2jfn . Official Cloud API has a 24h session window (Hermes ADDING_A_PLATFORM.md). OpenClaw: "QR pairing and stores more state on disk".

### iMessage
- Works only with a Mac host (chat.db + AppleScript, Full Disk Access) — Claude Code Channels, OpenClaw (`imsg`), OpenHuman, Hermes (BlueBubbles). Sentiment: "When it's working really well, there's literally no interface needed besides iMessage and email" https://news.ycombinator.com/item?id=48859633 ; risk: "To my surprise, it sent a text message reply" (agent sent a real iMessage instead of a draft) https://news.ycombinator.com/item?id=47105650

### Skeptics of the multi-channel approach
- "It can connect to everything, ok great, so I could chat with it on telegram, discord, email, whatsapp, etc. ... Too much choice." https://news.ycombinator.com/item?id=49624047 (2026-09-09)
- "the only 'new' thing about clawdbot is that it is using discord/telegram/etc as the interface? Which isn't really new, but seems to be what people really like" https://news.ycombinator.com/item?id=46832184

---
### 9. Synthesis for shell

### Patterns every harness converged on
1. **One gateway process, N adapters** (Hermes, OpenClaw, NanoClaw host, OpenHuman). None runs a process per platform.
2. **Small required interface + optional capabilities that degrade to text.** Hermes: required `send/send_typing/send_image/connect`, optional `send_clarify`, pickers, approval buttons "degrade gracefully to plain text". OpenClaw: explicit `ChannelCapabilities { reactions, edit, threads, media, polls, nativeCommands, blockStreaming ... }`. NanoClaw: `supportsThreads` + per-DM/group `ChannelDefaults`. OpenHuman: "Capabilities are declared per channel, not assumed".
3. **Session key = platform + chat + optional thread (+ optional user).** Hermes `<platform>:<chat_type>:<chat_id>[:<thread_id>][:<user>]`. OpenClaw lets DMs share one "main" session across channels by default and isolates groups. NanoClaw lets you pick per channel (separate / shared agent / shared session). Cross-platform continuity is solved with shared memory (Hermes, and an open request #4335 for more) or an explicit "main session" (OpenClaw).
4. **Streaming = edit a preview message**, with per-platform defaults (OpenClaw: Telegram/Slack progress on, Discord off by default). Telegram Bot API 10.x native drafts (`sendMessageDraft`) are the new thing (Hermes already uses it in DMs; OpenClaw has an open issue).
5. **Button callbacks use a shared id convention** so resolvers are platform-agnostic (Hermes `appr:<id>:<choice>`; NanoClaw `onAction(questionId, option, userId)`).
6. **Bot-loop protection** for multiple bots in one group (OpenClaw shared loop protection; Hermes `exclusive_bot_mentions`). Relevant to our two-bot family group on any new platform.

### Platform ranking evidence
- Telegram is the default/recommended/fastest in OpenClaw docs, Hermes defaults, Claude Code Channels (first tab), and has the most issues in both big repos (Hermes 1136 vs Discord 469; OpenClaw 5156 vs 3892).
- Discord is consistently #2 and the preferred choice for thread-per-project / multi-agent work, at the cost of setup and a 2,000-char limit.
- Consumer products (Meta Muse) go native app + WhatsApp, not Telegram/Discord.

### Implications for a second platform (my read, not from sources)
- Discord's main advertised advantage (threads contain projects) is something shell already gets from Telegram forum topics. The one blog that crowned Discord never tried Telegram topics. So the case for Discord should rest on something else: desktop app quality, native components (select menus, buttons, modals), slash-command UX, voice channels, or family members who already live there.
- Discord costs: Message Content privileged intent; OAuth invite; everyone in the family needs a Discord account and joins a server; 2,000-char messages (need a chunker or file-baton); per-thread session keys (thread id = new channel id); Hermes' Discord adapter grew to 10k lines with "streaming duplicates" and attachment bugs, so the surface is bigger than it looks.
- If the goal is "the family can reach it without Telegram", WhatsApp is where families are, but only the official Cloud API is safe (Baileys → ban reports) and it has the 24h window. iMessage needs a macOS host (shell appears to run on macOS, darwin here) and has the "agent sent a real text" risk.
- Adapter shape worth copying: OpenClaw-style capability struct + Hermes-style "optional methods fall back to text" + NanoClaw's `supportsThreads` flag and explicit engage mode (`mention` / `mention-sticky` / `pattern`). Vercel Chat SDK (TS) can't be reused from Go.

### Not confirmed / where I looked
- answeroverflow.com threads (OpenClaw Discord archive) and Reddit: blocked (429 / refused). Discord-mobile-specific user complaints: not found.
- OpenHuman channel sentiment: none found (web search, HN Algolia).
- Telegram App Store pull (Aug 2026): single source (Mazaika).
- "Muse" is ambiguous: most likely Meta Muse (consumer, closed); open clones are tiny.

Checklist status: all sections done.
