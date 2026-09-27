// Package discord connects the bridge to Discord: a gateway session for
// inbound messages and reactions, and the daemon's outbound interface for
// everything the agent sends on its own (docs/DESIGN-DISCORD.md).
package discord

import (
	"fmt"
	"log/slog"
	"strconv"

	"github.com/rcliao/shell/internal/config"
)

// snowflakeFloor is the smallest Discord id this package accepts.
//
// The whole id scheme rests on Discord and Telegram ids never meeting. Telegram
// ids stay below 10^16 in magnitude (a -100 prefix on at most 13 digits);
// Discord snowflakes are milliseconds since 2015 shifted left 22 bits, which
// passes 10^17 about nine months into Discord's life. Anything below the floor
// is rejected rather than trusted — a collision would put a Discord channel's
// turns into a family member's Telegram session.
const snowflakeFloor = int64(1e17)

// Conv is an internal conversation: the (chat, thread) every session, memory
// and schedule is keyed by.
type Conv struct {
	ChatID   int64
	ThreadID int64
}

// Target is where a Discord send goes: the channel (or thread, which Discord
// models as a channel) to post in.
type Target struct {
	ChannelID string
}

// Addresses maps Discord ids to internal ids and back.
//
// Two sources, in order. A configured link wins: the channel takes over the
// internal conversation of the Telegram chat it replaces. Otherwise the id is
// derived from the snowflake itself — +id for a DM, −id for a guild channel,
// so the "negative = group" convention the bridge relies on holds — and both
// agents derive the same id without talking to each other.
type Addresses struct {
	chats      map[string]Conv // discord channel/thread id → linked conv
	reverse    map[Conv]string // linked conv → discord channel/thread id
	users      map[string]int64
	guilds     map[string]int64 // discord server id → the chat its channels join
	guildChats map[int64]bool
}

// NewAddresses builds the maps from config. Links with unparsable or
// colliding ids are logged and skipped rather than failing startup: one bad
// line should not take the family's agent offline.
func NewAddresses(cfg config.DiscordConfig) *Addresses {
	a := &Addresses{
		chats:   map[string]Conv{},
		reverse: map[Conv]string{},
		users:      map[string]int64{},
		guilds:     map[string]int64{},
		guildChats: map[int64]bool{},
	}
	for id, link := range cfg.Chats {
		if _, err := parseSnowflake(id); err != nil {
			slog.Warn("discord: chat link skipped", "discord_id", id, "error", err)
			continue
		}
		c := Conv{ChatID: link.ChatID, ThreadID: link.ThreadID}
		if prev, dup := a.reverse[c]; dup {
			slog.Warn("discord: two channels linked to one conversation; keeping the first",
				"chat_id", c.ChatID, "thread_id", c.ThreadID, "kept", prev, "skipped", id)
			continue
		}
		a.chats[id] = c
		a.reverse[c] = id
	}
	for id, g := range cfg.Guilds {
		if _, err := parseSnowflake(id); err != nil || g.ChatID == 0 {
			slog.Warn("discord: guild link skipped", "guild", id, "error", err)
			continue
		}
		a.guilds[id] = g.ChatID
		a.guildChats[g.ChatID] = true
	}
	for id, tg := range cfg.Users {
		if _, err := parseSnowflake(id); err != nil {
			slog.Warn("discord: user link skipped", "discord_id", id, "error", err)
			continue
		}
		a.users[id] = tg
	}
	return a
}

// Counts reports how many links are loaded, for the startup log line that
// makes a mismatch between the two agents' configs visible.
func (a *Addresses) Counts() (chats, users, guilds int) {
	return len(a.chats), len(a.users), len(a.guilds)
}

// User returns the Telegram user id linked to a Discord user.
func (a *Addresses) User(discordUserID string) (int64, bool) {
	id, ok := a.users[discordUserID]
	return id, ok
}

// Inbound resolves where a Discord message lives.
//
// channelID is the channel the message was posted in; parentID is that
// channel's parent when it is a thread or forum post, "" otherwise; guildID is
// the server ("" for a DM); isDM marks a direct message channel. In order:
//
//  1. A linked channel or thread takes over its configured conversation.
//  2. A DM is +channel.
//  3. In a server mapped in Guilds, the channel — or a thread's linked parent,
//     so a thread in the family channel stays in the family chat — gives the
//     chat, and the channel or thread's own snowflake is the topic. This is
//     what lets a channel or forum created later join the family chat with no
//     config edit.
//  4. Otherwise the id is derived: −channel for a guild channel, and a thread
//     sits in its parent's chat.
func (a *Addresses) Inbound(channelID, parentID, guildID string, isDM bool) (Conv, error) {
	if c, ok := a.chats[channelID]; ok {
		return c, nil
	}
	if isDM {
		chat, err := derivedChat(channelID, true)
		if err != nil {
			return Conv{}, err
		}
		return Conv{ChatID: chat}, nil
	}
	own, err := parseSnowflake(channelID)
	if err != nil {
		return Conv{}, err
	}
	if parentID != "" {
		if parent, ok := a.chats[parentID]; ok {
			return Conv{ChatID: parent.ChatID, ThreadID: own}, nil
		}
	}
	if chat, ok := a.guilds[guildID]; ok {
		return Conv{ChatID: chat, ThreadID: own}, nil
	}
	if parentID == "" {
		return Conv{ChatID: -own}, nil
	}
	parentChat, err := derivedChat(parentID, false)
	if err != nil {
		return Conv{}, err
	}
	return Conv{ChatID: parentChat, ThreadID: own}, nil
}

// Outbound resolves where a send for an internal conversation goes. ok is
// false when the conversation is not on Discord — the caller then hands it to
// Telegram.
func (a *Addresses) Outbound(chatID, threadID int64) (Target, bool) {
	if id, ok := a.reverse[Conv{ChatID: chatID, ThreadID: threadID}]; ok {
		return Target{ChannelID: id}, true
	}
	// A Discord-native thread is its own channel, whichever chat it is in.
	if threadID >= snowflakeFloor && a.OnDiscord(chatID) {
		return Target{ChannelID: strconv.FormatInt(threadID, 10)}, true
	}
	// Otherwise the chat's main channel. That includes a Telegram topic that
	// was never linked: the send lands in the main channel rather than being
	// lost. (Lane session threads are negative and never reach delivery —
	// the bridge maps them back with RealThread first.)
	if id, ok := a.reverse[Conv{ChatID: chatID}]; ok {
		return Target{ChannelID: id}, true
	}
	if abs(chatID) >= snowflakeFloor {
		return Target{ChannelID: strconv.FormatInt(abs(chatID), 10)}, true
	}
	return Target{}, false
}

// OnDiscord reports whether a chat is delivered on Discord: linked, joined by
// a server, or in the derived range.
func (a *Addresses) OnDiscord(chatID int64) bool {
	if abs(chatID) >= snowflakeFloor || a.guildChats[chatID] {
		return true
	}
	_, ok := a.reverse[Conv{ChatID: chatID}]
	return ok
}

// IsMessageID reports whether a message id is a Discord one. Discord message
// ids are snowflakes (≥ the floor); Telegram numbers messages per chat from 1.
// Edits and pins carry only chat + message id, and this is what routes them —
// it holds across restarts with no lookup table.
func IsMessageID(id int) bool { return int64(id) >= snowflakeFloor }

func derivedChat(channelID string, isDM bool) (int64, error) {
	id, err := parseSnowflake(channelID)
	if err != nil {
		return 0, err
	}
	if isDM {
		return id, nil
	}
	return -id, nil
}

func parseSnowflake(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not a discord id: %q", s)
	}
	if id < snowflakeFloor {
		return 0, fmt.Errorf("discord id %d below the %d floor would collide with Telegram ids", id, snowflakeFloor)
	}
	return id, nil
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
