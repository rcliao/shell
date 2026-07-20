package bridge

import (
	"testing"

	"github.com/rcliao/shell/internal/process"
)

func ghostPut() process.ToolCall {
	return process.ToolCall{Name: "mcp__ghost__ghost_put", Input: map[string]any{}}
}

func bashRemember() process.ToolCall {
	return process.ToolCall{Name: "Bash", Input: map[string]any{"command": "scripts/shell-remember --action memo"}}
}

func TestClassifyWrite(t *testing.T) {
	tests := []struct {
		name     string
		userMsg  string
		response string
		calls    []process.ToolCall
		want     string
	}{
		{
			name:     "verbal save — Chinese claim '補進 Notion ✅', no tool call",
			userMsg:  "晚餐：地瓜 + 優格",
			response: "收到 📝 晚餐記下，補進 Notion ✅",
			calls:    nil,
			want:     "verbal_save",
		},
		{
			name:     "verbal save — '記進 ... 了 ✅' phrasing, no tool call",
			userMsg:  "晚餐：地瓜 + 優格",
			response: "好的～ 記進 Notion 了 ✅",
			calls:    nil,
			want:     "verbal_save",
		},
		{
			name:     "verbal save — resultative '更新好了 ✅', no write",
			userMsg:  "幫我改一下午餐",
			response: "更新好了 ✅",
			calls:    nil,
			want:     "verbal_save",
		},
		{
			name:     "verified — Chinese claim and ghost_put succeeded",
			userMsg:  "晚餐：地瓜 + 優格",
			response: "收到 📝 補進 Notion ✅",
			calls:    []process.ToolCall{ghostPut()},
			want:     "verified",
		},
		{
			name:     "verified via bash shell-remember",
			userMsg:  "remember this meal",
			response: "Saved it for you ✅",
			calls:    []process.ToolCall{bashRemember()},
			want:     "verified",
		},
		{
			name:     "silent failure — write tool errored",
			userMsg:  "記下這餐",
			response: "好的 記下了",
			calls:    []process.ToolCall{{Name: "mcp__ghost__ghost_put", Failed: true}},
			want:     "silent_failure",
		},
		{
			name:     "unclaimed trigger — user asked, agent neither claimed nor wrote",
			userMsg:  "幫我記一下今天午餐",
			response: "好喔～ 午餐看起來不錯！",
			calls:    nil,
			want:     "unclaimed_trigger",
		},
		{
			name:     "no false positive — 記在心裡 (emotional, not persistence) is not a claim",
			userMsg:  "我有點難過",
			response: "我會把你的心情記在心裡 💛",
			calls:    nil,
			want:     "",
		},
		// Production false-positive classes (sampled from a week of ledger
		// rows — 14/15 verbal_save flags were these, all sanitized):
		{
			name:     "no false positive — future promise (澆完我再補進) is not a claim",
			userMsg:  "植物澆水要澆多少",
			response: "要澆到盆底出水才對喔。澆完跟我說，我補進 Notion 📖",
			calls:    nil,
			want:     "",
		},
		{
			name:     "no false positive — conditional future (的話我再寫進日記)",
			userMsg:  "土還有點濕",
			response: "再等兩三天，澆了的話我再寫進日記📖",
			calls:    nil,
			want:     "",
		},
		{
			name:     "no false positive — offer question (要不要我把它加進去嗎)",
			userMsg:  "這家餐廳有名嗎",
			response: "滿有名的喔！要不要我把它加進行程那格嗎？",
			calls:    nil,
			want:     "",
		},
		{
			name:     "no false positive — refusal/negation (還沒吃就不用先記錄)",
			userMsg:  "我等等想清炒加進晚餐",
			response: "聽起來很搭！這只是計畫，還沒吃就不用先記錄，等吃完我再幫忙補晚餐memo喔",
			calls:    nil,
			want:     "",
		},
		{
			name:     "no false positive — non-persistence 加到 (food, not a write)",
			userMsg:  "芝麻粉可以加到咖啡裡嗎",
			response: "芝麻粉可以加到咖啡裡面喔，口感會變濃稠一點",
			calls:    nil,
			want:     "",
		},
		{
			name:     "no false positive — non-persistence 補上 (metabolic, not a write)",
			userMsg:  "午餐沒吃到澱粉",
			response: "偶爾一餐沒吃不影響，身體會用其他熱量來源補上",
			calls:    nil,
			want:     "",
		},
		{
			name:     "no false positive — 記錄 as noun (聊天記錄)",
			userMsg:  "怎麼一直跳題",
			response: "應該是聊天記錄混在一起顯示，不是真的跳題啦",
			calls:    nil,
			want:     "",
		},
		{
			name:     "no false positive — conditional 才會記錄",
			userMsg:  "我可以吃一顆藥嗎",
			response: "真的吃了要跟我說一聲，我才會記錄「有吃」，不然只算建議",
			calls:    nil,
			want:     "",
		},
		{
			name:     "no false positive — past reference (那時就寫好結論)",
			userMsg:  "上次澆水是哪天",
			response: "日記那時就寫好結論：以後看實際土況判斷。這次不用寫，我沒動日記",
			calls:    nil,
			want:     "",
		},
		{
			name:     "no false positive — advice to user (記下當天吃了什麼)",
			userMsg:  "怎麼分辨疹子原因",
			response: "發作時拍照，記下當天曬了多久、吃了什麼，兩三次後模式就清楚了",
			calls:    nil,
			want:     "",
		},
		{
			name:     "no false positive — reference to a write hours ago (今早那筆已經寫進)",
			userMsg:  "植物看起來怎麼樣",
			response: "長得很好！今早澆透那筆我已經寫進 Notion 了，繼續維持微濕就好",
			calls:    nil,
			want:     "",
		},
		{
			name:     "no false positive — content authored in-reply (把英文信寫好)",
			userMsg:  "幫我寫封英文信給房東",
			response: "好的 ② 幫妳把英文信寫好了，妳看看：Dear landlord...",
			calls:    nil,
			want:     "",
		},
		{
			name:     "no false positive — 登錄 as noun (BSMI 登錄號)",
			userMsg:  "這個字號是什麼",
			response: "✅ 商檢字號 R56297 — 這就是我要的 BSMI 登錄號",
			calls:    nil,
			want:     "",
		},
		{
			name:     "no false positive — descriptive 登記 (vendor has registration)",
			userMsg:  "這家代購可靠嗎",
			response: "看起來正規：有登記公司 + 客服可聯絡，評價也不錯",
			calls:    nil,
			want:     "",
		},
		{
			name:     "no false positive — coincidence adverb (剛好補上一個重點 is discussion)",
			userMsg:  "這是軌道的近拍",
			response: "這張近拍剛好補上一個重點，軌道溝槽裡卡了不少水垢",
			calls:    nil,
			want:     "",
		},
		{
			name:     "no false positive — peer attribution (Umbreon 已經幫妳寫進…了)",
			userMsg:  "接駁車路線有到哪些點",
			response: "路線整理好給妳看。Umbreon 也已經幫妳寫進 Notion 行程頁了💛",
			calls:    nil,
			want:     "",
		},
		{
			name:     "genuine claim with completion marker still caught — 已經幫妳補進…了",
			userMsg:  "脖子還是癢",
			response: "已經幫妳補進 7/11 頁面的「反應/備註」欄了 💛",
			calls:    nil,
			want:     "verbal_save",
		},
		{
			name:     "skip — ordinary chat, nothing persistence-related",
			userMsg:  "今天天氣如何？",
			response: "今天晴天 ☀️ 26 度",
			calls:    nil,
			want:     "",
		},
		{
			name:     "skip — agent wrote memory but made no claim and no trigger",
			userMsg:  "我喜歡抹茶",
			response: "抹茶超讚的 🍵",
			calls:    []process.ToolCall{ghostPut()},
			want:     "", // background memory write, not a claimed/triggered persist
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyWrite(tt.userMsg, tt.response, tt.calls, []string{"umbreon", "umbreonmini", "哥哥", "小傘"})
			if got.classification != tt.want {
				t.Errorf("classifyWrite() = %q, want %q (claimed=%v triggered=%v writeOK=%v writeFailed=%v)",
					got.classification, tt.want, got.claimed, got.triggered, got.writeOK, got.writeFailed)
			}
		})
	}
}

// The notion skill script replaced the MCP server: only its write subcommands
// count as persistence; reads must not verify a save claim.
func TestNotionScriptWriteClassification(t *testing.T) {
	writes := []string{
		"~/.shell/skills/notion/scripts/notion patch-prop abc123 午餐 '▫️ item'",
		"scripts/notion append abc123 note text",
		"curl -X PATCH https://api.notion.com/v1/pages/abc",
	}
	reads := []string{
		"~/.shell/skills/notion/scripts/notion get-page abc123",
		"scripts/notion query-db dbid --date Date=2026-07-13",
		"skills/meal-memo/scripts/food-log get-day 2026-07-13",
	}
	for _, cmd := range writes {
		tc := process.ToolCall{Name: "Bash", Input: map[string]any{"command": cmd}}
		if !isPersistenceTool(tc) {
			t.Fatalf("write not classified as persistence: %q", cmd)
		}
	}
	for _, cmd := range reads {
		tc := process.ToolCall{Name: "Bash", Input: map[string]any{"command": cmd}}
		if isPersistenceTool(tc) {
			t.Fatalf("read falsely classified as persistence: %q", cmd)
		}
	}
}
