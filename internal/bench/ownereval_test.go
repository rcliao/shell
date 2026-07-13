package bench

import "testing"

// Detector patterns must catch owner-correction phrasing SHAPES and stay
// quiet on ordinary chat. Fixtures are SYNTHETIC — no real conversation
// content may appear in this file (repo redaction rule; see Makefile
// verify-no-pii).
func TestOwnerEvalDetectors(t *testing.T) {
	factual := []string{
		"我明明是問窗戶要不要關，你怎麼在講別的",
		"這附近哪有那間餐廳",
		"我說的是這個不是那個",
		"你今天的回答都有點文不對題",
		"你的訊息又太長",
		"你有看仔細嗎",
		"你記錯了吧",
		"no, I meant the other one",
	}
	for _, s := range factual {
		if !factualCorrectionRe.MatchString(s) {
			t.Fatalf("factual detector missed: %q", s)
		}
	}

	delivery := []string{
		"你回答被截掉了",
		"我怎麼沒看到你的回覆",
		"提醒怎麼會重複兩次一樣的訊息？",
		"你的回答怎麼不見了？",
		"常卡 Analyzing 沒回覆",
	}
	for _, s := range delivery {
		if !deliveryComplaintRe.MatchString(s) {
			t.Fatalf("delivery detector missed: %q", s)
		}
	}

	nudges := []string{"pika?", "babies?", "Umbreon?"}
	for _, s := range nudges {
		if !nudgeRe.MatchString(s) {
			t.Fatalf("nudge detector missed: %q", s)
		}
	}

	leaks := []string{
		"This message is directed at the other agent, not me — I'll stay quiet",
		"API Error: 500 internal server error",
		"You've hit your session limit · resets 4:30pm",
		"There's an issue with the selected model",
	}
	for _, s := range leaks {
		if !internalLeakRe.MatchString(s) {
			t.Fatalf("leak detector missed: %q", s)
		}
	}

	// Negative controls: ordinary chat must not trip detectors.
	clean := []string{
		"早餐memo - 鬆餅 + 藍莓 + 一顆水煮蛋",
		"這雙拖鞋耐穿嗎",
		"好的謝謝你們💛",
		"今天天氣不錯，適合澆花",
	}
	for _, s := range clean {
		if factualCorrectionRe.MatchString(s) || deliveryComplaintRe.MatchString(s) ||
			internalLeakRe.MatchString(s) || photoExpiredRe.MatchString(s) {
			t.Fatalf("false positive on clean text: %q", s)
		}
	}

	expired := []string{
		"暫存檔案也已經過期了，讀不到了",
		"圖片載入失敗了，可以再傳一次嗎",
	}
	for _, s := range expired {
		if !photoExpiredRe.MatchString(s) {
			t.Fatalf("photo-expired detector missed: %q", s)
		}
	}
}
