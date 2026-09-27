package discord

import (
	"testing"

	"github.com/rcliao/shell/internal/config"
)

// Synthetic ids only. Snowflake-shaped ids are ≥ 1e17; Telegram-shaped ones
// are small.
const (
	familyChannel = "200000000000000001" // linked to the Telegram group
	topicThread   = "200000000000000002" // linked to a Telegram forum topic
	dmChannel     = "200000000000000003" // linked to a Telegram DM
	nativeChannel = "200000000000000004" // not linked
	nativeThread  = "200000000000000005" // a new thread, not linked
	tgGroup       = int64(-100200300)
	tgDM          = int64(42)
)

func testAddresses() *Addresses {
	return NewAddresses(config.DiscordConfig{
		Users: map[string]int64{"300000000000000001": 42, "7": 99},
		Chats: map[string]config.DiscordChatLink{
			familyChannel: {ChatID: tgGroup},
			topicThread:   {ChatID: tgGroup, ThreadID: 12},
			dmChannel:     {ChatID: tgDM},
			"123":         {ChatID: 5}, // below the floor: skipped
		},
	})
}

func TestInbound(t *testing.T) {
	a := testAddresses()
	cases := []struct {
		name             string
		channel, parent  string
		dm               bool
		wantChat, wantTh int64
	}{
		{"linked channel takes over the telegram group", familyChannel, "", false, tgGroup, 0},
		{"linked dm takes over the telegram dm", dmChannel, "", true, tgDM, 0},
		{"linked thread takes over its topic", topicThread, familyChannel, false, tgGroup, 12},
		{"new thread in linked channel stays in the family chat", nativeThread, familyChannel, false, tgGroup, 200000000000000005},
		{"unlinked guild channel is negative", nativeChannel, "", false, -200000000000000004, 0},
		{"unlinked dm is positive", nativeChannel, "", true, 200000000000000004, 0},
		{"thread in unlinked channel", nativeThread, nativeChannel, false, -200000000000000004, 200000000000000005},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := a.Inbound(c.channel, c.parent, c.dm)
			if err != nil {
				t.Fatal(err)
			}
			if got.ChatID != c.wantChat || got.ThreadID != c.wantTh {
				t.Fatalf("got (%d,%d), want (%d,%d)", got.ChatID, got.ThreadID, c.wantChat, c.wantTh)
			}
		})
	}
}

func TestInboundRejectsIDsBelowFloor(t *testing.T) {
	if _, err := testAddresses().Inbound("12345", "", true); err == nil {
		t.Fatal("a small id would collide with Telegram ids; want an error")
	}
}

func TestOutbound(t *testing.T) {
	a := testAddresses()
	cases := []struct {
		name         string
		chat, thread int64
		want         string
		wantOK       bool
	}{
		{"linked group", tgGroup, 0, familyChannel, true},
		{"linked topic", tgGroup, 12, topicThread, true},
		{"unlinked telegram topic falls back to the main channel", tgGroup, 77, familyChannel, true},
		{"native thread in linked chat", tgGroup, 200000000000000005, nativeThread, true},
		{"linked dm", tgDM, 0, dmChannel, true},
		{"native guild channel", -200000000000000004, 0, nativeChannel, true},
		{"native dm", 200000000000000004, 0, nativeChannel, true},
		{"native thread", -200000000000000004, 200000000000000005, nativeThread, true},
		{"telegram chat not linked stays on telegram", -100999, 0, "", false},
		{"system chat stays off discord", 0, 0, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := a.Outbound(c.chat, c.thread)
			if ok != c.wantOK || got.ChannelID != c.want {
				t.Fatalf("got (%q,%v), want (%q,%v)", got.ChannelID, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	// Whatever Inbound resolves, Outbound must deliver back to the same channel.
	a := testAddresses()
	for _, ch := range []struct {
		channel, parent string
		dm              bool
	}{
		{familyChannel, "", false}, {topicThread, familyChannel, false}, {dmChannel, "", true},
		{nativeChannel, "", false}, {nativeChannel, "", true}, {nativeThread, familyChannel, false},
		{nativeThread, nativeChannel, false},
	} {
		conv, err := a.Inbound(ch.channel, ch.parent, ch.dm)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := a.Outbound(conv.ChatID, conv.ThreadID)
		if !ok || got.ChannelID != ch.channel {
			t.Errorf("%s → (%d,%d) → %q, want %q", ch.channel, conv.ChatID, conv.ThreadID, got.ChannelID, ch.channel)
		}
	}
}

func TestUsersAndCounts(t *testing.T) {
	a := testAddresses()
	if id, ok := a.User("300000000000000001"); !ok || id != 42 {
		t.Fatalf("linked user: got %d,%v", id, ok)
	}
	if _, ok := a.User("7"); ok {
		t.Fatal("an id below the floor must not be linked")
	}
	chats, users := a.Counts()
	if chats != 3 || users != 1 {
		t.Fatalf("counts: chats=%d users=%d, want 3,1", chats, users)
	}
}

func TestThreadLinkedWithoutItsChatRoutesToDiscord(t *testing.T) {
	// A forum topic carried over on its own: its sends go to the thread while
	// the rest of that Telegram group stays on Telegram.
	a := NewAddresses(config.DiscordConfig{Chats: map[string]config.DiscordChatLink{
		topicThread: {ChatID: -100200300, ThreadID: 5},
	}})
	if got, ok := a.Outbound(-100200300, 5); !ok || got.ChannelID != topicThread {
		t.Fatalf("linked topic: got %q,%v", got.ChannelID, ok)
	}
	if _, ok := a.Outbound(-100200300, 0); ok {
		t.Fatal("the unlinked main chat must stay on Telegram")
	}
	if _, ok := a.Outbound(-100200300, 6); ok {
		t.Fatal("another unlinked topic must stay on Telegram")
	}
}

func TestIsMessageID(t *testing.T) {
	if IsMessageID(4812) || !IsMessageID(400000000000000001) {
		t.Fatal("telegram ids are small, discord ids are snowflakes")
	}
}
