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
			got := classifyWrite(tt.userMsg, tt.response, tt.calls)
			if got.classification != tt.want {
				t.Errorf("classifyWrite() = %q, want %q (claimed=%v triggered=%v writeOK=%v writeFailed=%v)",
					got.classification, tt.want, got.claimed, got.triggered, got.writeOK, got.writeFailed)
			}
		})
	}
}
