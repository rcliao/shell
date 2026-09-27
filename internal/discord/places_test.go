package discord

import (
	"strconv"
	"testing"

	"github.com/bwmarrin/discordgo"
)

const forumChan = "700000000000000003"

func TestForumPostTagsAndArchive(t *testing.T) {
	api := newFakeAPI()
	api.channels[forumChan] = &discordgo.Channel{ID: forumChan, Type: discordgo.ChannelTypeGuildForum,
		AvailableTags: []discordgo.ForumTag{{ID: "1", Name: "planning"}}}
	b := newBot(api, Options{})

	post, err := b.CreateForumPost(forumChan, "✈️ Trip", "", []string{"planning"})
	if err != nil {
		t.Fatal(err)
	}
	pc := api.channels[strconv.FormatInt(post, 10)]
	if pc == nil || pc.ParentID != forumChan || len(pc.AppliedTags) != 1 || pc.AppliedTags[0] != "1" {
		t.Fatalf("post = %+v", pc)
	}
	if last := api.sends[len(api.sends)-1]; last.content != "✈️ Trip" {
		t.Fatalf("first message = %q, want the title when content is empty", last.content)
	}

	// A new stage becomes a new forum tag, then the post's only tag.
	if err := b.SetPostTags(post, []string{"booked"}); err != nil {
		t.Fatal(err)
	}
	if n := len(api.channels[forumChan].AvailableTags); n != 2 {
		t.Fatalf("forum tags = %d, want 2", n)
	}
	booked := tagID(api.channels[forumChan].AvailableTags, "booked")
	if booked == "" || len(pc.AppliedTags) != 1 || pc.AppliedTags[0] != booked {
		t.Fatalf("applied = %v, booked id %q", pc.AppliedTags, booked)
	}

	if err := b.ArchiveThread(post); err != nil {
		t.Fatal(err)
	}
	if pc.ThreadMetadata == nil || !pc.ThreadMetadata.Archived {
		t.Fatal("post not archived")
	}
}

func TestForumPostRejectsNonForum(t *testing.T) {
	api := newFakeAPI()
	api.channels[groupChan] = &discordgo.Channel{ID: groupChan, Type: discordgo.ChannelTypeGuildText}
	b := newBot(api, Options{})
	if _, err := b.CreateForumPost(groupChan, "x", "", []string{"planning"}); err == nil {
		t.Fatal("tagging in a text channel should fail")
	}
}
