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

// Digest handles the /digest command — comprehensive daily digest with per-source breakdown.
func (b *Bridge) Digest(ctx context.Context, chatID int64, args string) (string, error) {
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	// Per-source usage breakdown
	bySource, err := b.store.GetUsageSummaryBySource(chatID, todayStart)
	if err != nil {
		return "", fmt.Errorf("get usage by source: %w", err)
	}

	// Message count
	msgCount, _ := b.store.GetMessageCount(chatID, todayStart)

	// Session rotations
	sessCount, _ := b.store.GetSessionRotations(chatID, todayStart)

	// Active schedules
	schedCount, _ := b.store.GetActiveScheduleCount(chatID)

	// Aggregate totals
	var totalCost float64
	var totalExchanges int64
	var totalTurns int64
	var totalInput int64
	var totalOutput int64
	for _, u := range bySource {
		totalCost += u.TotalCostUSD
		totalExchanges += u.ExchangeCount
		totalTurns += u.TotalTurns
		totalInput += u.TotalInputTokens
		totalOutput += u.TotalOutputTokens
	}

	var sb strings.Builder
	sb.WriteString("## Daily Digest\n\n")
	sb.WriteString(fmt.Sprintf("**Date:** %s\n", now.Format("Monday, 2006-01-02")))
	sb.WriteString(fmt.Sprintf("**Messages:** %s\n", commaInt(int64(msgCount))))
	sb.WriteString(fmt.Sprintf("**Sessions:** %d\n", sessCount))
	sb.WriteString(fmt.Sprintf("**Active schedules:** %d\n", schedCount))
	sb.WriteString(fmt.Sprintf("**Total cost:** $%.4f\n\n", totalCost))

	if len(bySource) > 0 {
		sb.WriteString("### Usage by source\n\n")
		sb.WriteString("| Source | Exchanges | Turns | Input | Output | Cost |\n")
		sb.WriteString("|--------|-----------|-------|-------|--------|------|\n")
		for _, src := range []string{"interactive", "heartbeat", "scheduler"} {
			u, ok := bySource[src]
			if !ok {
				continue
			}
			sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | $%.4f |\n",
				src,
				commaInt(u.ExchangeCount),
				commaInt(u.TotalTurns),
				commaInt(u.TotalInputTokens),
				commaInt(u.TotalOutputTokens),
				u.TotalCostUSD,
			))
		}
		sb.WriteString(fmt.Sprintf("| **Total** | **%s** | **%s** | **%s** | **%s** | **$%.4f** |\n",
			commaInt(totalExchanges),
			commaInt(totalTurns),
			commaInt(totalInput),
			commaInt(totalOutput),
			totalCost,
		))
	} else {
		sb.WriteString("No usage recorded today.")
	}

	return sb.String(), nil
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
