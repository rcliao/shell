package bridge

import (
	"testing"

	"github.com/rcliao/shell/internal/process"
)

func TestIsRecallTrigger(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		// Chinese recall triggers (retrieval of a logged fact)
		{"皮卡 今天奶製品總共多少", true},
		{"昨天晚餐吃什麼", true},
		{"我之前說的那個過敏反應", true},
		{"這週喝了幾天的茶", true},
		{"全天 dairy 到目前多少", true},
		{"你還記得我上次的備註嗎", true},
		// English recall triggers
		{"how much dairy did I have today", true},
		{"what did I log for lunch yesterday", true},
		{"remember the supplement schedule we set?", true},
		{"when did I last take flonase", true},
		// NOT recall — unit conversion uses 多少/幾 but isn't retrieving a stored fact
		{"3/4\" 是多少cm", false},
		{"Diameter 3/4\" 是多少mm", false},
		{"幾点了", false},
		// NOT recall — a logging command, not a question about the past
		{"皮卡 早餐memo 跟昨天一樣", true}, // contains 昨天 → still a recall (must read yesterday)
		{"今天午餐 yellow curry chips", false},
		{"幫我查一下這個產品", false},
	}
	for _, c := range cases {
		if got := isRecallTrigger(c.msg); got != c.want {
			t.Errorf("isRecallTrigger(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

func TestIsReadTool(t *testing.T) {
	read := []process.ToolCall{
		{Name: "mcp__ghost__ghost_search"},
		{Name: "mcp__ghost__ghost_get"},
		{Name: "mcp__claude_ai_Notion__notion-fetch"},
		{Name: "Bash", Input: map[string]any{"command": "~/.shell/.../food-log get-day 2026-06-21"}},
		{Name: "Bash", Input: map[string]any{"command": "ghost search dairy"}},
	}
	for _, tc := range read {
		if !isReadTool(tc) {
			t.Errorf("isReadTool(%q / %v) = false, want true", tc.Name, tc.Input)
		}
	}

	notRead := []process.ToolCall{
		{Name: "mcp__ghost__ghost_put"},                        // a write, not a read
		{Name: "mcp__claude_ai_Notion__notion-update-page"},    // a write
		{Name: "mcp__claude_ai_Notion__notion-create-pages"},   // a write
		{Name: "Bash", Input: map[string]any{"command": "shell-remember 'x'"}}, // a write
		{Name: "Bash", Input: map[string]any{"command": "ls -la"}},
	}
	for _, tc := range notRead {
		if isReadTool(tc) {
			t.Errorf("isReadTool(%q / %v) = true, want false", tc.Name, tc.Input)
		}
	}
}

func TestClassifyRecall(t *testing.T) {
	readCall := process.ToolCall{Name: "mcp__ghost__ghost_search"}
	failedRead := process.ToolCall{Name: "mcp__ghost__ghost_search", Failed: true}
	writeCall := process.ToolCall{Name: "mcp__ghost__ghost_put"}

	cases := []struct {
		name          string
		msg           string
		calls         []process.ToolCall
		ghostInjected bool
		wantClass     string
		wantGrounding string
	}{
		{
			name:      "not a recall trigger → skip",
			msg:       "今天午餐 yellow curry chips",
			wantClass: "",
		},
		{
			name:          "active read grounds the recall",
			msg:           "今天奶製品總共多少",
			calls:         []process.ToolCall{readCall},
			wantClass:     "grounded_recall",
			wantGrounding: "active_read",
		},
		{
			name:          "ghost injection grounds the recall behind the scenes",
			msg:           "昨天晚餐吃什麼",
			ghostInjected: true,
			wantClass:     "grounded_recall",
			wantGrounding: "ghost_inject",
		},
		{
			name:          "active read wins even if ghost also injected",
			msg:           "昨天晚餐吃什麼",
			calls:         []process.ToolCall{readCall},
			ghostInjected: true,
			wantClass:     "grounded_recall",
			wantGrounding: "active_read",
		},
		{
			name:      "no read, no injection → ungrounded memory_recall",
			msg:       "我之前的過敏反應是什麼",
			wantClass: "memory_recall",
		},
		{
			name:      "a failed read does not count as grounding",
			msg:       "今天奶製品總共多少",
			calls:     []process.ToolCall{failedRead},
			wantClass: "memory_recall",
		},
		{
			name:      "a write tool is not a read → still ungrounded",
			msg:       "昨天吃了什麼",
			calls:     []process.ToolCall{writeCall},
			wantClass: "memory_recall",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := classifyRecall(c.msg, "some answer", c.calls, c.ghostInjected)
			if v.classification != c.wantClass {
				t.Errorf("classification = %q, want %q", v.classification, c.wantClass)
			}
			if c.wantGrounding != "" && v.grounding != c.wantGrounding {
				t.Errorf("grounding = %q, want %q", v.grounding, c.wantGrounding)
			}
		})
	}
}
