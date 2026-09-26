package bridge

import (
	"strings"
	"testing"
)

func TestUserFacingText(t *testing.T) {
	cases := []struct {
		name        string
		segments    []string
		want        string
		wantDropped int
	}{
		{
			name:     "no tools: single segment untouched",
			segments: []string{"Yes, poppy seeds are safe to eat."},
			want:     "Yes, poppy seeds are safe to eat.",
		},
		{
			name: "short asides before a tool are dropped",
			segments: []string{
				"Probably sandbox network. Retry unsandboxed.",
				"Use the browser skill.",
				"It's pretty, but it's more of a spring or summer dress 🌿 It's lightweight chiffon.",
			},
			want:        "It's pretty, but it's more of a spring or summer dress 🌿 It's lightweight chiffon.",
			wantDropped: 2,
		},
		{
			name: "verbalised self-check before the answer is dropped",
			segments: []string{
				"That's a status report — reply ≤2 sentences, no reopening.",
				"好的 👍 袋子外面寫上「GLASS」，收垃圾那天放出去就好，早點休息 🌙",
			},
			want:        "好的 👍 袋子外面寫上「GLASS」，收垃圾那天放出去就好，早點休息 🌙",
			wantDropped: 1,
		},
		{
			name: "long answer before a memory save survives (the allText regression)",
			segments: []string{
				"Here is the full answer to your question, with all the detail you asked for: the trolley boards at the Welcome Plaza, runs every ten minutes, and the last one back leaves at six. Park at the south entrance lot and bring water.",
				"Memory saved",
			},
			want: "Here is the full answer to your question, with all the detail you asked for: the trolley boards at the Welcome Plaza, runs every ten minutes, and the last one back leaves at six. Park at the south entrance lot and bring water.\n\nMemory saved",
		},
		{
			name:     "short answer before a memory save survives (confirmation is the shorter one)",
			segments: []string{"Yes, you can freeze it. Two weeks is fine.", "Memory saved"},
			want:     "Yes, you can freeze it. Two weeks is fine.\n\nMemory saved",
		},
		{
			name:     "turn that ends on a tool call keeps what it said",
			segments: []string{"Let me check the schedule."},
			want:     "Let me check the schedule.",
		},
		{
			name: "Chinese: short aside dropped, a ~50-character answer kept, short confirmation kept",
			segments: []string{
				"要先看一下這個連結指向哪一家。",
				"**這家是在購物中心裡的火鍋店 🍲** 不吃辣的話選豆腐鍋或人蔘雞鍋都可以；麻辣那兩鍋是真的辣，跳過。點餐的時候不要加起司，其他配料都沒問題。停車場消費蓋章有折抵，記得帶收據。",
				"記進今天的午餐紀錄了 🖤",
			},
			want:        "**這家是在購物中心裡的火鍋店 🍲** 不吃辣的話選豆腐鍋或人蔘雞鍋都可以；麻辣那兩鍋是真的辣，跳過。點餐的時候不要加起司，其他配料都沒問題。停車場消費蓋章有折抵，記得帶收據。\n\n記進今天的午餐紀錄了 🖤",
			wantDropped: 1,
		},
		{
			name: "empty",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, dropped := userFacingText(c.segments)
			if got != c.want {
				t.Errorf("text:\n got %q\nwant %q", got, c.want)
			}
			if len(dropped) != c.wantDropped {
				t.Errorf("dropped %d segments, want %d: %q", len(dropped), c.wantDropped, dropped)
			}
		})
	}
}

// Journal turns keep their full narrative: a deep-beat journal is read by the
// owner, not a chat member, and the asides are the audit trail. A turn bound
// for a real chat is filtered whoever started it.
func TestApplyUserFacingText_JournalKeepsFullText(t *testing.T) {
	segs := []string{"Let me check the schedule.", "[noop] nothing due, nothing to send."}
	full := "Let me check the schedule.\n\n[noop] nothing due, nothing to send."
	if got := applyUserFacingText(0, true, "heartbeat", segs, full); got != full {
		t.Errorf("journal: got %q, want full text", got)
	}
	for _, source := range []string{"interactive", "scheduler", "followup"} {
		if got := applyUserFacingText(42, false, source, segs, full); got != "[noop] nothing due, nothing to send." {
			t.Errorf("%s: got %q", source, got)
		}
	}
}

func TestReviewRepliesAreFilteredLikeUserText(t *testing.T) {
	if !isJournalTurn(true, 42, "heartbeat") || !isJournalTurn(false, 0, "scheduler") {
		t.Error("heartbeats and system-chat turns keep the journal's full text")
	}
	if isJournalTurn(false, 0, ReviewTurnSender) {
		t.Error("the weekly review reaches the owner: it must be filtered")
	}
	if isJournalTurn(false, 42, "a family member") {
		t.Error("a user turn is never a journal")
	}
	if !isSystemSender(ReviewTurnSender) {
		t.Error("the review is still a system turn (no router shadow, no transcript)")
	}
	// The first live review's shape: a short pre-tool narration, then the summary.
	segs := []string{"Filed. Now the DO step — reinforcing my own behavior.",
		"Reinforced the message-length rule for myself and filed 3 suggestions that had never reached you; everything else is at goal."}
	got := applyUserFacingText(0, isJournalTurn(false, 0, ReviewTurnSender), "scheduler", segs, strings.Join(segs, "\n\n"))
	if strings.Contains(got, "Filed. Now the DO step") || !strings.Contains(got, "Reinforced") {
		t.Errorf("review reply = %q", got)
	}
}
