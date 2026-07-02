package topic

import (
	"testing"
	"time"
)

// TestExtractCommitmentsChinese — Chinese commitment trigger coverage.
// Cycle 81. Fixtures are synthetic stand-ins (cycle 144 PII scrub) chosen
// to exercise the same regex patterns as the original production-derived
// cases without quoting real user/assistant content.
func TestExtractCommitmentsChinese(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		input     string
		wantN     int
		wantDueIn time.Duration // approximate (+/- 1d)
	}{
		{
			name:      "明天注意",
			input:     "項目A 觀察持續中 — 明天注意有沒有變化",
			wantN:     1,
			wantDueIn: 24 * time.Hour,
		},
		{
			name:      "明天注意 (variant)",
			input:     "項目B 今日量約 N — 也注意反應 明天注意",
			wantN:     1,
			wantDueIn: 24 * time.Hour,
		},
		{
			name:  "稍後 with bare temporal",
			input: "守著你～ 好好休息 💛 稍後幫你看",
			wantN: 0, // "稍後" alone, no specific date → keep conservative
		},
		{
			name:      "過幾天",
			input:     "持續觀察狀況，過幾天再幫你檢查",
			wantN:     1,
			wantDueIn: 7 * 24 * time.Hour,
		},
		// Known gap (documented, not blocking): "提醒妳<date>" without a
		// "明天/稍後/下次/的時候" follow-up doesn't fire the CN trigger.
		// Cycle 82+ may add a date-driven shortcut. Real production hits
		// are rare; the loss is acceptable for v1.
		{
			name:  "Chinese date M月D日 (known gap)",
			input: "提醒你5月20日要回診",
			wantN: 0,
		},
		{
			name:  "no commitment (purely descriptive)",
			input: "植物的葉子捲曲通常是它的自救反應",
			wantN: 0,
		},
		{
			name:  "no commitment (English greeting)",
			input: "晚安～ 🌙",
			wantN: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractCommitments(c.input)
			if len(got) != c.wantN {
				for i, g := range got {
					t.Logf("  result[%d]: action=%q due=%v", i, g.Action, g.DueAt)
				}
				t.Errorf("got %d commitments, want %d", len(got), c.wantN)
				return
			}
			if c.wantN > 0 && c.wantDueIn > 0 {
				delta := got[0].DueAt.Sub(now.Add(c.wantDueIn))
				if delta < -36*time.Hour || delta > 36*time.Hour {
					t.Errorf("due date off: got %v, want ~%v from now (delta %v)",
						got[0].DueAt, c.wantDueIn, delta)
				}
			}
		})
	}
}
