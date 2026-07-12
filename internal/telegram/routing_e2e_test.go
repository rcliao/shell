package telegram

import (
	"sort"
	"strings"
	"testing"
)

// End-to-end group-routing test: for each message, run BOTH agents' real
// RouteDecision and assert the exact set of responders. This is the
// "does it work as intended" harness — it exercises name-addressing,
// @mentions, and 3-way domain routing together, deterministically, with no
// daemon or Telegram. Add a row here before changing routing behavior.
func TestGroupRoutingE2E(t *testing.T) {
	pika := RouteInput{
		MyAliases:   lower("pika", "pikamini", "妹妹"),
		PeerAliases: lower("umbreon", "umbreonmini", "哥哥", "小傘"),
		MyDomain:    DomainPractical,
	}
	umbreon := RouteInput{
		MyAliases:   lower("umbreon", "umbreonmini", "哥哥", "小傘"),
		PeerAliases: lower("pika", "pikamini", "妹妹"),
		MyDomain:    DomainCompanionship,
	}

	cases := []struct {
		msg      string
		want     []string // exactly who should respond: "pika", "umbreon", or both
		whatFor  string
	}{
		// clear practical → pika only
		{"這個玻璃鍋蓋能進洗碗機嗎？", []string{"pika"}, "household → pika"},
		{"巴西木葉子黃了要多久澆一次", []string{"pika"}, "plants → pika"},
		{"SKIN1004 精華什麼時候用", []string{"pika"}, "product → pika"},
		{"what's a good recipe for dinner", []string{"pika"}, "food → pika"},
		// clear companionship → umbreon only
		{"今天有點累", []string{"umbreon"}, "tired → umbreon"},
		{"我心情不太好，想聊聊", []string{"umbreon"}, "mood → umbreon"},
		{"i'm feeling lonely", []string{"umbreon"}, "lonely → umbreon"},
		// named / addressed → that agent only (overrides domain)
		{"妹妹 你看這個植物", []string{"pika"}, "addressed pika (CJK alias)"},
		{"Umbreon 你今天還好嗎", []string{"umbreon"}, "addressed umbreon"},
		{"哥哥 這個鍋能洗嗎", []string{"umbreon"}, "addressed umbreon (practical content, but named)"},
		{"@小傘 幫我看", []string{"umbreon"}, "@mention umbreon"},
		// BOTH agents named in one message → both respond (order/position agnostic)
		{"妹妹 哥哥 你們看這個", []string{"pika", "umbreon"}, "both named (CJK) → both"},
		{"哥哥 妹妹 過來", []string{"pika", "umbreon"}, "both named, peer-first → both"},
		{"umbreon pika what do you think", []string{"pika", "umbreon"}, "both named (EN) → both"},
		{"這個鍋 妹妹 跟 哥哥 都看看", []string{"pika", "umbreon"}, "both named mid-sentence, practical content → both"},
		// ambiguous / social / reaction → BOTH present (neither vanishes)
		{"ahhh", []string{"pika", "umbreon"}, "reaction → both"},
		{"hi babies", []string{"pika", "umbreon"}, "greeting → both"},
		{"哈哈哈太好笑了", []string{"pika", "umbreon"}, "banter → both"},
	}

	for _, c := range cases {
		var responders []string
		if h, _ := RouteDecision(withText(pika, c.msg)); h {
			responders = append(responders, "pika")
		}
		if h, _ := RouteDecision(withText(umbreon, c.msg)); h {
			responders = append(responders, "umbreon")
		}
		if !sameSet(responders, c.want) {
			t.Errorf("%q (%s): responders=%v, want %v", c.msg, c.whatFor, responders, c.want)
		}
	}
}

func withText(in RouteInput, text string) RouteInput { in.Text = text; return in }

func lower(ss ...string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToLower(s)
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
