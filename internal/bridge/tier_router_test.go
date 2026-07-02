package bridge

import "testing"

func TestClassifyTier(t *testing.T) {
	cases := []struct {
		msg      string
		hb       bool
		source   string
		wantTier string
	}{
		// simple — acks, reactions, greetings
		{msg: "ok", wantTier: "simple"},
		{msg: "謝謝", wantTier: "simple"},
		{msg: "哈哈", wantTier: "simple"},
		{msg: "好的~", wantTier: "simple"},
		{msg: "早安", wantTier: "simple"},
		{msg: "👍", wantTier: "simple"},
		// everyday — memos, short Q&A, media asks
		{msg: "早餐memo - toast, latte and dairy", wantTier: "everyday"},
		{msg: "今天天氣如何", wantTier: "everyday"},
		{msg: "幫我畫一張貓的圖", wantTier: "everyday"},
		{msg: "what time is the game tonight?", wantTier: "everyday"},
		// short but a question → not simple
		{msg: "幾點?", wantTier: "everyday"},
		// deep — research/comparison/analysis
		{msg: "幫我研究一下這兩台筆電哪台好", wantTier: "deep"},
		{msg: "compare these two hotels and recommend one", wantTier: "deep"},
		{msg: "為什麼我的植物葉子變黃", wantTier: "deep"},
		// demanding — dev/build markers
		{msg: "help me debug the scheduler, it fires twice", wantTier: "demanding"},
		{msg: "refactor the store layer to use interfaces", wantTier: "demanding"},
		{msg: "```go\nfunc main(){}\n``` why does this not compile", wantTier: "demanding"},
		// background turns tagged, not routed
		{msg: "[Heartbeat] check in", hb: true, wantTier: "everyday"},
		{msg: "daily briefing", source: "scheduler", wantTier: "everyday"},
	}
	for _, c := range cases {
		got := classifyTier(c.msg, c.hb, c.source)
		if got.tier != c.wantTier {
			t.Errorf("classifyTier(%q) = %s (%s), want %s", c.msg, got.tier, got.reason, c.wantTier)
		}
	}
}
