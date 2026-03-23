package bridge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// Usage handles the /usage command.
//   - /usage         — today's usage for this chat
//   - /usage all     — all-time usage for this chat
//   - /usage global  — usage across all chats (today)
func (b *Bridge) Usage(ctx context.Context, chatID int64, args string) (string, error) {
	args = strings.TrimSpace(args)

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	switch args {
	case "global":
		summary, err := b.store.GetUsageAllChats(todayStart)
		if err != nil {
			return "", fmt.Errorf("get global usage: %w", err)
		}
		return formatUsage("Global usage (today)", summary), nil

	case "all":
		summary, err := b.store.GetUsageSummary(chatID, time.Time{})
		if err != nil {
			return "", fmt.Errorf("get usage: %w", err)
		}
		return formatUsage("Usage (all time)", summary), nil

	default:
		summary, err := b.store.GetUsageSummary(chatID, todayStart)
		if err != nil {
			return "", fmt.Errorf("get usage: %w", err)
		}
		return formatUsage("Usage (today)", summary), nil
	}
}

func formatUsage(title string, u *store.UsageSummary) string {
	if u.ExchangeCount == 0 {
		return fmt.Sprintf("## %s\n\nNo usage recorded.", title)
	}

	totalTokens := u.TotalInputTokens + u.TotalOutputTokens
	return fmt.Sprintf(
		"## %s\n\n"+
			"**Exchanges:** %s\n"+
			"**Turns:** %s\n"+
			"**Input tokens:** %s\n"+
			"**Output tokens:** %s\n"+
			"**Cache creation:** %s\n"+
			"**Cache read:** %s\n"+
			"**Total tokens:** %s\n"+
			"**Cost:** $%.4f",
		title,
		commaInt(u.ExchangeCount),
		commaInt(u.TotalTurns),
		commaInt(u.TotalInputTokens),
		commaInt(u.TotalOutputTokens),
		commaInt(u.TotalCacheCreationTokens),
		commaInt(u.TotalCacheReadTokens),
		commaInt(totalTokens),
		u.TotalCostUSD,
	)
}

// commaInt formats an int64 with comma separators.
func commaInt(n int64) string {
	if n < 0 {
		return "-" + commaInt(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}

	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}
