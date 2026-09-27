package discord

import (
	"strconv"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/rcliao/shell/internal/config"
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
		t.Fatal("a post in a text channel should fail")
	}
}

// A tag the bot cannot add must not cost the project its post.
type noTagEditAPI struct{ *fakeAPI }

func (a noTagEditAPI) EditChannel(id string, e *discordgo.ChannelEdit) (*discordgo.Channel, error) {
	if e.AvailableTags != nil {
		return nil, discordgo.ErrUnauthorized
	}
	return a.fakeAPI.EditChannel(id, e)
}

func TestForumPostSurvivesTagFailure(t *testing.T) {
	api := newFakeAPI()
	api.channels[forumChan] = &discordgo.Channel{ID: forumChan, Type: discordgo.ChannelTypeGuildForum}
	b := newBot(noTagEditAPI{api}, Options{})
	post, err := b.CreateForumPost(forumChan, "Trip", "", []string{"new stage"})
	if err != nil || api.channels[strconv.FormatInt(post, 10)] == nil {
		t.Fatalf("post not created: %d %v", post, err)
	}
	// Clearing tags sends an empty list, not null.
	if err := b.SetPostTags(post, nil); err != nil {
		t.Fatal(err)
	}
	if pc := api.channels[strconv.FormatInt(post, 10)]; pc.AppliedTags == nil {
		t.Fatal("cleared tags sent as null")
	}
}

const testGuild = "700000000000000100"

func areaBot(t *testing.T) (*Bot, *fakeAPI) {
	api := newFakeAPI()
	api.channels[groupChan] = &discordgo.Channel{ID: groupChan, GuildID: testGuild, Type: discordgo.ChannelTypeGuildText, ParentID: "700000000000000200"}
	addr := NewAddresses(config.DiscordConfig{
		Chats:  map[string]config.DiscordChatLink{groupChan: {ChatID: -100200300}},
		Guilds: map[string]config.DiscordGuildLink{testGuild: {ChatID: -100200300}},
	})
	return newBot(api, Options{Addresses: addr}), api
}

// The second agent's identical request reuses the first one's channels.
func TestEnsureAreaChannelsFindOrCreate(t *testing.T) {
	b, api := areaBot(t)
	first, err := b.EnsureAreaChannels(-100200300, "Gaming", "gaming projects", []string{"playing", "done"})
	if err != nil || !first.Created {
		t.Fatalf("first: %+v %v", first, err)
	}
	text, forum := api.channels[first.ChannelID], api.channels[first.ForumID]
	if text.Name != "gaming" || forum.Name != "gaming-projects" || forum.Type != discordgo.ChannelTypeGuildForum {
		t.Fatalf("names/types: %q %q %v", text.Name, forum.Name, forum.Type)
	}
	if text.ParentID != "700000000000000200" || text.Topic != "Projects: <#"+forum.ID+">" || len(forum.AvailableTags) != 2 {
		t.Fatalf("category/topic/tags: %q %q %v", text.ParentID, text.Topic, forum.AvailableTags)
	}
	second, err := b.EnsureAreaChannels(-100200300, "gaming", "Gaming Projects", nil)
	if err != nil || second.Created || second != (AreaChannels{ChannelID: first.ChannelID, ForumID: first.ForumID}) {
		t.Fatalf("second: %+v %v", second, err)
	}
	if len(api.created) != 2 {
		t.Fatalf("created %d channels, want 2", len(api.created))
	}
	if _, err := b.EnsureAreaChannels(42, "x", "y", nil); err == nil {
		t.Fatal("a chat with no server must be refused")
	}
}

// A same-title post is found (emoji and case ignored); info reads tags.
func TestFindOpenPostAndInfo(t *testing.T) {
	b, api := areaBot(t)
	ac, _ := b.EnsureAreaChannels(-100200300, "travel", "trips", []string{"planning"})
	post, err := b.CreateForumPost(ac.ForumID, "🎉 Japan 2027", "", []string{"planning"})
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := b.FindOpenPost(ac.ForumID, "japan 2027")
	if err != nil || !ok || got != post {
		t.Fatalf("find: %d %v %v, want %d", got, ok, err, post)
	}
	if _, ok, _ := b.FindOpenPost(ac.ForumID, "Japan 2028"); ok {
		t.Fatal("different title matched")
	}
	info, err := b.PostInfo(post)
	if err != nil || info.Title != "🎉 Japan 2027" || info.ForumID != ac.ForumID || len(info.Tags) != 1 || info.Tags[0] != "planning" {
		t.Fatalf("info: %+v %v", info, err)
	}
	if err := b.ArchiveThread(post); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := b.FindOpenPost(ac.ForumID, "Japan 2027"); ok {
		t.Fatal("an archived post is not open")
	}
	_ = api
}

// racingAPI hides a channel the other agent made a moment earlier from the
// first listing only, as when both agents list before either creates.
type racingAPI struct {
	*fakeAPI
	hidden string
	listed int
}

func (r *racingAPI) GuildChannels(guild string) ([]*discordgo.Channel, error) {
	all, err := r.fakeAPI.GuildChannels(guild)
	r.listed++
	if r.listed > 1 {
		return all, err
	}
	var out []*discordgo.Channel
	for _, c := range all {
		if c.ID != r.hidden {
			out = append(out, c)
		}
	}
	return out, err
}

// Two agents creating at the same moment converge on the older channel,
// and the later one removes its own duplicate.
func TestEnsureAreaChannelsRace(t *testing.T) {
	_, base := areaBot(t)
	theirs := &discordgo.Channel{ID: "100000000000000001", GuildID: testGuild, Name: "gaming-projects", Type: discordgo.ChannelTypeGuildForum}
	base.channels[theirs.ID] = theirs
	api := &racingAPI{fakeAPI: base, hidden: theirs.ID}
	addr := NewAddresses(config.DiscordConfig{
		Chats:  map[string]config.DiscordChatLink{groupChan: {ChatID: -100200300}},
		Guilds: map[string]config.DiscordGuildLink{testGuild: {ChatID: -100200300}},
	})
	b := newBot(api, Options{Addresses: addr})
	ac, err := b.EnsureAreaChannels(-100200300, "gaming", "Gaming Projects", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ac.ForumID != theirs.ID {
		t.Fatalf("forum = %s, want the older %s", ac.ForumID, theirs.ID)
	}
	if len(base.deleted) != 1 || base.channels[base.deleted[0]] != nil {
		t.Fatalf("own duplicate not removed: deleted=%v", base.deleted)
	}
}
