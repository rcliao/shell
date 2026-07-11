package bridge

import (
	"strings"
	"testing"

	"github.com/rcliao/shell/internal/config"
)

func TestA2AMarkerRoundTrip(t *testing.T) {
	prompt := A2ADeliveryPrompt("Pika", 2, "哥哥 can you take the plant part?")
	depth, framed, isA2A := parseA2AMarker(prompt)
	if !isA2A {
		t.Fatal("expected A2A marker to parse")
	}
	if depth != 2 {
		t.Errorf("depth = %d, want 2", depth)
	}
	if !strings.Contains(framed, "Pika") || !strings.Contains(framed, "plant part") {
		t.Errorf("framed message lost content: %q", framed)
	}
	if strings.Contains(framed, "[A2A from=") {
		t.Errorf("framed message should not still contain the raw marker: %q", framed)
	}
}

func TestA2AMarkerAbsent(t *testing.T) {
	depth, framed, isA2A := parseA2AMarker("just a normal human message")
	if isA2A {
		t.Error("plain message should not parse as A2A")
	}
	if depth != 0 {
		t.Errorf("plain message depth = %d, want 0", depth)
	}
	if framed != "just a normal human message" {
		t.Errorf("plain message should pass through unchanged, got %q", framed)
	}
}

func TestPeerAddressedInReply(t *testing.T) {
	b := &Bridge{
		agentBotUsername: "Pikamini_bot",
		peerAgents: []config.PeerAgent{
			{Name: "Umbreon", BotUsername: "umbreon_mini_bot", Aliases: []string{"umbreon", "哥哥", "小傘"}},
		},
	}
	cases := []struct {
		reply string
		want  string // expected peer bot username, "" for none
	}{
		{"哥哥 你覺得這個要怎麼分工？", "umbreon_mini_bot"},
		{"Umbreon, can you take the plant question?", "umbreon_mini_bot"},
		{"@小傘 幫我看一下", "umbreon_mini_bot"},
		{"好的，我來處理這個", ""},                  // addresses a human, not the peer
		{"這個問題我覺得...", ""},                  // no address
		{"the umbreon evolution line is cool", ""}, // mention mid-sentence, not leading
	}
	for _, c := range cases {
		got := b.peerAddressedInReply(c.reply)
		gotUser := ""
		if got != nil {
			gotUser = got.BotUsername
		}
		if gotUser != c.want {
			t.Errorf("peerAddressedInReply(%q) = %q, want %q", c.reply, gotUser, c.want)
		}
	}
}
