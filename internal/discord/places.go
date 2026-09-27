package discord

import (
	"fmt"
	"regexp"
	"log/slog"
	"strconv"
	"strings"
	"unicode"

	"github.com/bwmarrin/discordgo"
)

// Places: the bot creates and manages a project's own place, a post in an
// area's forum channel (docs/DESIGN-PROJECT-AREAS.md). A post is a thread,
// so its snowflake is the project's message_thread_id and it routes like any
// other thread (Addresses.Inbound / Outbound).

// Discord limits: thread names are at most 100 characters, tag names 20.
const (
	maxThreadName = 100
	maxTagName    = 20
)

// CreateForumPost starts a post in the forum channel with a first message
// and the named tags (created on the forum when missing). It returns the
// post's snowflake, which is also its thread id.
func (b *Bot) CreateForumPost(forumID, title, content string, tags []string) (int64, error) {
	forum, err := b.api.Channel(forumID)
	if err != nil {
		return 0, fmt.Errorf("discord: forum %s: %w", forumID, err)
	}
	if forum.Type != discordgo.ChannelTypeGuildForum {
		return 0, fmt.Errorf("discord: channel %s is not a forum", forumID)
	}
	// Tags are decoration: a tag the bot cannot add (no Manage Channels, or
	// the forum's 20-tag limit) must not cost the project its post. `project
	// stage` retries the tag later and reports the error there.
	ids, err := b.forumTagIDs(forumID, tags)
	if err != nil {
		slog.Warn("discord: forum post created without tags", "forum", forumID, "tags", tags, "error", err)
		ids = nil
	}
	if content == "" {
		content = title
	}
	th, err := b.api.StartForumThread(forumID,
		&discordgo.ThreadStart{Name: truncateRunes(title, maxThreadName), AppliedTags: ids},
		&discordgo.MessageSend{Content: truncateRunes(content, maxMessageLen)})
	if err != nil {
		return 0, fmt.Errorf("discord: start forum post: %w", err)
	}
	return strconv.ParseInt(th.ID, 10, 64)
}

// SetPostTags replaces a forum post's tags with the named ones.
func (b *Bot) SetPostTags(postID int64, tags []string) error {
	post, err := b.api.Channel(strconv.FormatInt(postID, 10))
	if err != nil {
		return fmt.Errorf("discord: post %d: %w", postID, err)
	}
	ids, err := b.forumTagIDs(post.ParentID, tags)
	if err != nil {
		return err
	}
	if ids == nil {
		ids = []string{} // clear: Discord wants [], not null
	}
	_, err = b.api.EditChannel(post.ID, &discordgo.ChannelEdit{AppliedTags: &ids})
	return err
}

// ArchiveThread closes a thread or forum post. It stays readable, and a new
// message in it reopens it.
func (b *Bot) ArchiveThread(threadID int64) error {
	yes := true
	_, err := b.api.EditChannel(strconv.FormatInt(threadID, 10), &discordgo.ChannelEdit{Archived: &yes})
	return err
}

// forumTagIDs maps tag names to the forum's tag ids, adding the missing ones
// to the forum (that needs Manage Channels). Names compare exactly, after the
// same truncation Discord would apply.
func (b *Bot) forumTagIDs(forumID string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	forum, err := b.api.Channel(forumID)
	if err != nil {
		return nil, fmt.Errorf("discord: forum %s: %w", forumID, err)
	}
	if forum.Type != discordgo.ChannelTypeGuildForum {
		return nil, fmt.Errorf("discord: channel %s is not a forum", forumID)
	}
	have := forum.AvailableTags
	var missing []string
	for _, n := range names {
		n = truncateRunes(n, maxTagName)
		if tagID(have, n) == "" && n != "" {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		all := append([]discordgo.ForumTag(nil), have...)
		for _, n := range missing {
			all = append(all, discordgo.ForumTag{Name: n})
		}
		updated, err := b.api.EditChannel(forumID, &discordgo.ChannelEdit{AvailableTags: &all})
		if err != nil {
			return nil, fmt.Errorf("discord: add forum tags %v: %w", missing, err)
		}
		have = updated.AvailableTags
	}
	var ids []string
	for _, n := range names {
		if id := tagID(have, truncateRunes(n, maxTagName)); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func tagID(tags []discordgo.ForumTag, name string) string {
	for _, t := range tags {
		if t.Name == name {
			return t.ID
		}
	}
	return ""
}

// Area places (docs/DESIGN-PROJECT-AREAS.md, part 2): an area gets a text
// channel for loose talk and a forum for its projects. Creation is
// find-or-create by name, so the second agent that runs the same command
// reuses the first agent's channels and nobody hands anything off.

// AreaChannels are an area's two channels.
type AreaChannels struct {
	ChannelID string // text channel: the area's own place
	ForumID   string // forum: one post per project
	Created   bool   // at least one of them was created now
}

// channelKey compares channel names however Discord stored them: letters
// and digits only, lower case. "Gaming Projects", "gaming-projects" and a
// name Discord stripped of punctuation all match.
func channelKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// oldest returns the channel of this type and name with the lowest id (the
// first one made), or nil. Both agents pick the same one.
func oldest(chans []*discordgo.Channel, typ discordgo.ChannelType, key string) *discordgo.Channel {
	var best *discordgo.Channel
	var bestID int64
	for _, c := range chans {
		if c.Type != typ || channelKey(c.Name) != key {
			continue
		}
		id, err := strconv.ParseInt(c.ID, 10, 64)
		if err != nil {
			continue
		}
		if best == nil || id < bestID {
			best, bestID = c, id
		}
	}
	return best
}

// EnsureAreaChannels finds or creates the text channel `name` and the forum
// `forumName` (with tags) in the server chatID joins, next to the chat's
// main channel. A new or topic-less text channel gets a topic that points
// at the forum.
//
// Two agents may run this at the same moment (both answer the same yes).
// After creating, it lists again and keeps the oldest channel of each name,
// deleting its own duplicate: the later creator always sees the earlier
// one, so both converge on one pair.
func (b *Bot) EnsureAreaChannels(chatID int64, name, forumName string, tags []string) (AreaChannels, error) {
	var out AreaChannels
	guild, ok := b.addr.GuildFor(chatID)
	if !ok {
		return out, fmt.Errorf("discord: chat %d has no server (discord.guilds) to create channels in", chatID)
	}
	textKey, forumKey := channelKey(name), channelKey(forumName)
	if textKey == "" || forumKey == "" || textKey == forumKey {
		return out, fmt.Errorf("discord: area needs two distinct channel names")
	}
	chans, err := b.api.GuildChannels(guild)
	if err != nil {
		return out, fmt.Errorf("discord: list channels: %w", err)
	}
	category := ""
	if t, ok := b.addr.Outbound(chatID, 0); ok {
		if main, err := b.api.Channel(t.ChannelID); err == nil {
			category = main.ParentID
		}
	}
	ensure := func(typ discordgo.ChannelType, key, display, topic string) (*discordgo.Channel, error) {
		if c := oldest(chans, typ, key); c != nil {
			return c, nil
		}
		mine, err := b.api.CreateGuildChannel(guild, discordgo.GuildChannelCreateData{
			Name: strings.ToLower(strings.Join(strings.Fields(display), "-")), Type: typ, ParentID: category, Topic: topic})
		if err != nil {
			return nil, err
		}
		out.Created = true
		again, err := b.api.GuildChannels(guild)
		if err != nil {
			return mine, nil
		}
		if first := oldest(again, typ, key); first != nil && first.ID != mine.ID {
			// Another agent made it first: use theirs, remove ours.
			if err := b.api.DeleteChannel(mine.ID); err != nil {
				slog.Warn("discord: duplicate area channel not removed", "channel", mine.ID, "error", err)
			}
			return first, nil
		}
		return mine, nil
	}

	// The text channel is the area's identity: both agents converge on it
	// by name. Its forum is then found through the channel, not by name —
	// two agents may name the forum differently (seen live: "gaming-projects"
	// vs a Chinese name), and a name match alone gave each its own forum.
	text, err := ensure(discordgo.ChannelTypeGuildText, textKey, name, "")
	if err != nil {
		return out, fmt.Errorf("discord: create channel %s: %w", name, err)
	}
	backref := "<#" + text.ID + ">"
	byID := func(list []*discordgo.Channel, id string) *discordgo.Channel {
		for _, c := range list {
			if c.ID == id && c.Type == discordgo.ChannelTypeGuildForum {
				return c
			}
		}
		return nil
	}
	// oldestFor is the forum that belongs to this channel: the oldest forum
	// whose topic points back at it. Both agents pick the same one.
	oldestFor := func(list []*discordgo.Channel) *discordgo.Channel {
		var best *discordgo.Channel
		var bestID int64
		for _, c := range list {
			if c.Type != discordgo.ChannelTypeGuildForum || !strings.Contains(c.Topic, backref) {
				continue
			}
			if id, err := strconv.ParseInt(c.ID, 10, 64); err == nil && (best == nil || id < bestID) {
				best, bestID = c, id
			}
		}
		return best
	}
	var forum *discordgo.Channel
	if id := topicForum(text.Topic); id != "" {
		forum = byID(chans, id) // the channel already names its forum
	}
	if forum == nil {
		forum = oldestFor(chans)
	}
	if forum == nil {
		forum = oldest(chans, discordgo.ChannelTypeGuildForum, forumKey)
	}
	if forum == nil {
		mine, err := b.api.CreateGuildChannel(guild, discordgo.GuildChannelCreateData{
			Name: strings.ToLower(strings.Join(strings.Fields(forumName), "-")), Type: discordgo.ChannelTypeGuildForum,
			ParentID: category, Topic: "Area: " + backref})
		if err != nil {
			return out, fmt.Errorf("discord: create forum %s: %w", forumName, err)
		}
		out.Created = true
		forum = mine
		if again, err := b.api.GuildChannels(guild); err == nil {
			if first := oldestFor(again); first != nil && first.ID != mine.ID {
				// Another agent made this area's forum first: use theirs.
				if err := b.api.DeleteChannel(mine.ID); err != nil {
					slog.Warn("discord: duplicate area forum not removed", "forum", mine.ID, "error", err)
				}
				forum = first
			}
		}
		if forum.ID == mine.ID && len(tags) > 0 {
			ft := make([]discordgo.ForumTag, 0, len(tags))
			for _, t := range tags {
				if t = truncateRunes(strings.TrimSpace(t), maxTagName); t != "" {
					ft = append(ft, discordgo.ForumTag{Name: t})
				}
			}
			if _, err := b.api.EditChannel(forum.ID, &discordgo.ChannelEdit{AvailableTags: &ft}); err != nil {
				slog.Warn("discord: area forum created without tags", "forum", forum.ID, "error", err)
			}
		}
	}
	if topicForum(text.Topic) != forum.ID {
		topic := "Projects: <#" + forum.ID + ">"
		if _, err := b.api.EditChannel(text.ID, &discordgo.ChannelEdit{Topic: topic}); err != nil {
			slog.Warn("discord: area channel topic not set", "channel", text.ID, "error", err)
		}
	}
	out.ChannelID, out.ForumID = text.ID, forum.ID
	return out, nil
}

var channelMention = regexp.MustCompile(`<#([0-9]{17,20})>`)

// topicForum returns the forum id an area channel's topic points at
// ("Projects: <#id>"), or "".
func topicForum(topic string) string {
	if m := channelMention.FindStringSubmatch(topic); m != nil {
		return m[1]
	}
	return ""
}

// postTitleKey compares post titles without their leading emoji and case:
// "🎉 日本行程 2027" and "日本行程 2027" are the same project.
func postTitleKey(s string) string {
	s = strings.TrimLeftFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// FindOpenPost returns an open post in the forum whose title matches, so a
// second agent joins the first one's post instead of opening another.
func (b *Bot) FindOpenPost(forumID, title string) (int64, bool, error) {
	forum, err := b.api.Channel(forumID)
	if err != nil {
		return 0, false, fmt.Errorf("discord: forum %s: %w", forumID, err)
	}
	threads, err := b.api.ActiveThreads(forum.GuildID)
	if err != nil {
		return 0, false, fmt.Errorf("discord: list threads: %w", err)
	}
	want := postTitleKey(title)
	for _, t := range threads {
		if t.ParentID == forumID && postTitleKey(t.Name) == want && want != "" {
			id, err := strconv.ParseInt(t.ID, 10, 64)
			return id, err == nil, err
		}
	}
	return 0, false, nil
}

// PostInfo describes a thread for `project join`: its title, the forum it is
// in ("" when its parent is not a forum), and its tags by name.
type PostInfo struct {
	Title   string
	ForumID string
	Tags    []string
}

func (b *Bot) PostInfo(threadID int64) (PostInfo, error) {
	var out PostInfo
	post, err := b.api.Channel(strconv.FormatInt(threadID, 10))
	if err != nil {
		return out, fmt.Errorf("discord: thread %d: %w", threadID, err)
	}
	out.Title = post.Name
	if post.ParentID == "" {
		return out, nil
	}
	parent, err := b.api.Channel(post.ParentID)
	if err != nil || parent.Type != discordgo.ChannelTypeGuildForum {
		return out, nil
	}
	out.ForumID = parent.ID
	for _, id := range post.AppliedTags {
		for _, t := range parent.AvailableTags {
			if t.ID == id {
				out.Tags = append(out.Tags, t.Name)
			}
		}
	}
	return out, nil
}

// EditPostOpener replaces a forum post's first message (its id is the
// post's id) with the project's live summary. Only the bot that opened the
// post can edit it; for another agent's post Discord refuses, which the
// caller treats as "not mine to keep".
func (b *Bot) EditPostOpener(postID int64, text string) error {
	id := strconv.FormatInt(postID, 10)
	text = truncateRunes(text, maxMessageLen)
	_, err := b.api.Edit(&discordgo.MessageEdit{Channel: id, ID: id, Content: &text})
	return err
}
