# Setting up Discord

How to move an agent's conversations from Telegram to Discord. The design is in `docs/DESIGN-DISCORD.md`.

Each agent needs its own Discord bot, because one token shared by two daemons would answer every message
twice, and the daemon refuses to start in that case. Telegram keeps running until you remove its token.
Conversations move one at a time: linking a channel is the switch.

## 1. Create a bot for each agent

In the Discord Developer Portal (discord.com/developers/applications):

1. **New Application**, named after the agent.
2. **Bot** tab:
   - Reset Token and copy it.
   - Turn on **Message Content Intent**. It's free under 10,000 users; without it the bot sees empty messages.
   - Leave Presence and Server Members off.
3. **OAuth2 → URL Generator**:
   - Scope: `bot`.
   - Permissions:
     - View Channels
     - Send Messages
     - Send Messages in Threads
     - Read Message History
     - Add Reactions
     - Attach Files
     - Embed Links
     - Manage Messages, which is needed to pin the project home and to remove the bot's own status reactions.
   - Open the URL and add the bot to the family server.

Repeat for the second agent.

## 2. Store the token

Store the token in the shell-secrets store, under a name ending in `_BOT_TOKEN` (for example
`PIKAMINI_DISCORD_BOT_TOKEN`), the same way the Telegram token is stored. Names ending in `_BOT_TOKEN` are always
stripped from Claude child processes. `shell secrets doctor --config <agent config>` shows whether the reference
resolves.

## 3. Collect ids

In Discord, go to User Settings → Advanced and turn on **Developer Mode**. Right-click → Copy ID now works on
users, channels and threads.

You need:
- each family member's Discord user id, paired with the Telegram user id they already have (the keys of
  `telegram.user_labels`);
- the Discord channel that replaces each Telegram chat: the family channel for the family group, and the DM
  channel with each person for their Telegram DM.

How to get a DM channel id: each bot has its own DM channel with each person, so a family member has one DM
channel per agent. The bot can open the channel without sending anything:

```bash
curl -X POST -H "Authorization: Bot $TOKEN" -H "Content-Type: application/json" \
  -d '{"recipient_id":"<discord user id>"}' https://discord.com/api/v10/users/@me/channels
```

The returned `id` is the DM channel. Run it once per bot for each person.

## 4. Link them in each agent's config

Put the same `users` and the same group-channel links in **both** agents' `config.json`. The shared transcript
and agent-to-agent hand-offs rely on both agents agreeing on those ids. DM links differ per agent, because each
bot has its own DM channel with each person. Both entries map to the same Telegram DM chat id.

```json
"discord": {
  "enabled": true,
  "token_env": "PIKAMINI_DISCORD_BOT_TOKEN",
  "users": {
    "<discord user id>": <telegram user id>
  },
  "chats": {
    "<family channel id>": { "chat_id": <telegram family group id>, "thread_id": 0 },
    "<DM channel id>":     { "chat_id": <telegram user id of that person>, "thread_id": 0 },
    "<thread id>":         { "chat_id": <telegram family group id>, "thread_id": <telegram topic id> }
  }
}
```

- A linked channel takes over that Telegram chat's sessions, memory, schedules, projects and lanes. From then
  on, every reminder and proactive message for that chat goes to Discord.
- **A Telegram forum topic can become its own channel.** Link a channel to `{ "chat_id": <group id>, "thread_id": <topic id> }`
  and the topic's session, lane and project carry over. A thread works the same way. The bot needs no extra
  permission for this; you create the channel.
- A thread entry is optional. Link a thread only to carry over a Telegram forum topic's session. New threads
  in a linked channel stay in that chat automatically.
- **New channels join automatically** when the server is mapped:
  `"guilds": { "<server id>": { "chat_id": <telegram family group id> } }`. Any channel, forum, thread or
  forum post you create later becomes its own topic of the family chat, with the family's memory and projects.
  No config edit or restart is needed. Explicit `chats` links still win.
- Channels in a server that isn't mapped still work, with ids derived from Discord. They start fresh.
- A person who isn't linked gets no answer. In a DM they are told the id to link.

## 5. Restart and check

A config change needs a restart (`./shell multi restart`). Then check `daemon.log`:

- `discord: enabled linked_chats=N linked_users=M`. Both agents should show the same N and M.
- `discord: connected bot=... guilds=1`.

Then DM the agent and watch the reply stream in. After that, check that a scheduled reminder for a linked chat
arrives on Discord.

Start the daemon outside the Claude Code sandbox, as with Telegram. The sandbox's proxy breaks the TLS
connection to Discord's gateway.

## 6. Turn Telegram off (after the move)

Remove `telegram.token_env`, or its secret, from each agent's config and restart. The agent then runs on
Discord alone. A send for a Telegram chat that was never linked is logged as dropped rather than lost silently.

## What differs from Telegram today

- **Images:** several images attached to one Discord message arrive as one turn, like a Telegram album.
- **Tables:** Markdown tables are shown in a code block, because Discord does not render tables.
- **Replies:** they stream by editing one message about every 1.5 s. Anything over 2,000 characters arrives as
  several messages, and code blocks are closed and reopened across the split.
- **Status reactions:** 👀 → ⏳ → ✅ or 🤔 on your message, one at a time.
- **Mentions:** the agent writes `@<canonical name>` (from `telegram.user_canonical`) to notify someone. It becomes
  a real mention, and only new messages ping: a reply that names you doesn't notify you again.
- **Replies:** replying to an earlier message quotes it to the agent, so "yes, that one" makes sense.
- **Reply buttons** (`"reply_buttons": true`): 🔄 Regenerate and 📌 Remember appear under each reply. A click does
  exactly what that reaction does. A button shows only when the agent's reaction map has the action.
- **Cards:** the agent can show a place, listing or project as a rich card by writing a fenced block with
  language `card` holding JSON (title, url, description, image, thumbnail, fields, footer). A block that doesn't
  parse is shown as written. On Telegram the block shows as plain JSON.
- **Times:** the agent may write Discord timestamps (`<t:UNIX:R>`), shown in each reader's local time.
- **Commands:** typing `/` shows a menu: `/new`, `/status`, `/help`, `/remember`, `/forget`, `/memories`,
  `/projects`, `/schedule` and `/reactions`, one entry per agent. Each bot registers them when it connects.
  Operator commands (`/plan`, `/usage` and so on) still work typed as text.
- **Not yet supported:**
  - voice notes and stickers;
  - merging several quick messages into one turn;
  - buttons other than links.
- **Bots:** messages from bots, including the other agent, are ignored. Agent-to-agent hand-offs still go
  through relay.
