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
		{"今天奶製品總共多少", true},
		{"昨天晚餐吃什麼", true},
		{"我之前說的那個皮膚反應", true},
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
		{"早餐memo 跟昨天一樣", true}, // contains 昨天 → still a recall (must read yesterday)
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
		injected      string
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
			name:          "relevant ghost injection grounds the recall",
			msg:           "昨天晚餐吃什麼",
			injected:      "[Memory] 晚餐 6/30: quinoa salad + salmon",
			wantClass:     "grounded_recall",
			wantGrounding: "ghost_inject",
		},
		{
			name:          "IRRELEVANT ghost injection does NOT ground — presence isn't relevance",
			msg:           "今天奶製品總共多少",
			injected:      "[Memory] user prefers window seats on flights; birthday is in August",
			wantClass:     "memory_recall",
			wantGrounding: "inject_irrelevant",
		},
		{
			name:          "subject-free temporal question: cannot judge relevance → injection grounds conservatively",
			msg:           "昨天吃了什麼",
			injected:      "[Memory] some unrelated context",
			wantClass:     "grounded_recall",
			wantGrounding: "ghost_inject",
		},
		{
			name:          "active read wins even if ghost also injected",
			msg:           "昨天晚餐吃什麼",
			calls:         []process.ToolCall{readCall},
			injected:      "[Memory] anything",
			wantClass:     "grounded_recall",
			wantGrounding: "active_read",
		},
		{
			name:          "no read, no injection → ungrounded memory_recall",
			msg:           "我之前的皮膚反應是什麼",
			wantClass:     "memory_recall",
			wantGrounding: "none",
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
		{
			name:          "english recall with relevant injection",
			msg:           "how much dairy did I have today",
			injected:      "[Memory] dairy log 7/1: latte (1), cheese toast (1)",
			wantClass:     "grounded_recall",
			wantGrounding: "ghost_inject",
		},
		{
			name:          "english recall with irrelevant injection",
			msg:           "how much dairy did I have today",
			injected:      "[Memory] the strategy game uses a hex grid",
			wantClass:     "memory_recall",
			wantGrounding: "inject_irrelevant",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := classifyRecall(c.msg, "some answer", c.calls, c.injected)
			if v.classification != c.wantClass {
				t.Errorf("classification = %q, want %q", v.classification, c.wantClass)
			}
			if c.wantGrounding != "" && v.grounding != c.wantGrounding {
				t.Errorf("grounding = %q, want %q", v.grounding, c.wantGrounding)
			}
		})
	}
}

func TestSalientTokens(t *testing.T) {
	cases := []struct {
		msg  string
		want []string // tokens that MUST be present
		none bool     // expect zero salient tokens
	}{
		{msg: "今天奶製品總共多少", want: []string{"奶製品"}},
		{msg: "how much dairy did I have today", want: []string{"dairy"}},
		{msg: "昨天吃了什麼", none: true},
		{msg: "上次的 collagen 補充計畫是什麼", want: []string{"collagen"}},
	}
	for _, c := range cases {
		got := salientTokens(c.msg)
		if c.none {
			if len(got) != 0 {
				t.Errorf("salientTokens(%q) = %v, want none", c.msg, got)
			}
			continue
		}
		for _, w := range c.want {
			found := false
			for _, g := range got {
				if g == w {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("salientTokens(%q) = %v, missing %q", c.msg, got, w)
			}
		}
	}
}
