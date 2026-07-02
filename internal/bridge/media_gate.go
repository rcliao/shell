package bridge

import (
	"log/slog"
	"regexp"
	"strings"
)

// mediaTriggerRe matches English requests for generated/attached media.
var mediaTriggerRe = regexp.MustCompile(`(?i)\b(image|photo|picture|pic|video|clip|draw|sketch|render|generate|animate|animation|gif|sticker|selfie|screenshot|infographic|chart|diagram|meme|wallpaper|art)\b`)

// mediaTriggerCJK are matched as substrings. Generous on purpose: a false
// negative blocks media the user actually asked for, which is worse than a
// false positive letting one slip through. Includes re-send phrasings so
// "圖片呢" / "再寄一次" count as explicit requests.
var mediaTriggerCJK = []string{
	"圖", "畫", "照片", "相片", "影片", "短片", "動畫", "貼圖", "表情包",
	"截圖", "拍", "海報", "卡片", "梗圖", "桌布", "再寄", "重寄", "再傳", "重傳",
}

// isMediaTrigger reports whether the user message plausibly asked for media.
func isMediaTrigger(userMsg string) bool {
	if mediaTriggerRe.MatchString(userMsg) {
		return true
	}
	for _, t := range mediaTriggerCJK {
		if strings.Contains(userMsg, t) {
			return true
		}
	}
	return false
}

// gateMedia enforces the no-unprompted-media rule on collected artifacts.
// Heartbeat turns never deliver media — background sends are exactly the
// failure the rule exists for. On user turns, media without a request trigger
// is logged, and dropped only when enforcement is enabled (media_gate_enforce),
// mirroring the write-verify canary rollout. Returns a short note to append to
// the response when media was withheld, so the text never claims a delivery
// that didn't happen.
func (b *Bridge) gateMedia(userMsg string, isHeartbeat bool, photos *[]Photo, videos *[]Video) string {
	n := len(*photos) + len(*videos)
	if n == 0 {
		return ""
	}

	if isHeartbeat {
		slog.Warn("media gate: blocked media on heartbeat turn",
			"photos", len(*photos), "videos", len(*videos))
		*photos = nil
		*videos = nil
		return ""
	}

	if isMediaTrigger(userMsg) {
		return ""
	}

	slog.Warn("media gate: media without request trigger",
		"photos", len(*photos), "videos", len(*videos),
		"enforced", b.claudeCfg.MediaGateEnforce, "user_msg_head", head(userMsg, 80))
	if !b.claudeCfg.MediaGateEnforce {
		return ""
	}
	*photos = nil
	*videos = nil
	return "\n(媒體未送出 — 這則訊息沒有明確要求圖片/影片)"
}

// head returns at most n bytes of s, on a rune boundary.
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
