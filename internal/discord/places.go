package discord

import (
	"fmt"
	"strconv"

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
	ids, err := b.forumTagIDs(forumID, tags)
	if err != nil {
		return 0, err
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
