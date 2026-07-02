package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// runTopicStats reads topic_decisions + topic_threads from one or more
// agent shell.db files and emits a regression-net report covering:
//   - per-source decision distribution (keyword/haiku/cache/fallback)
//   - per-topic activity
//   - Haiku latency p50/p95
//   - thread health (active, stale, overdue commitments)
//
// Cycle 69 deliverable. Future cycles can extend with auto-proposal
// triggers when thresholds breach.
func runTopicStats(args []string) {
	fs := flag.NewFlagSet("topic-stats", flag.ExitOnError)
	agent := fs.String("agent", "pikamini", "")
	home := fs.String("home", "", "")
	since := fs.String("since", "7d", "")
	chatID := fs.Int64("chat", 0, "filter by chat id; 0 = all")
	out := fs.String("out", "", "")
	fs.Parse(args)

	if *home == "" {
		*home = filepath.Join(os.Getenv("HOME"), ".shell")
	}
	dbPath := filepath.Join(*home, "agents", *agent, "shell.db")

	s, err := store.Open(dbPath)
	if err != nil {
		die(fmt.Errorf("open %s: %w", dbPath, err))
	}
	defer s.Close()

	dur := parseSince(*since)
	end := time.Now()
	begin := end.Add(-dur)

	stats, err := s.AggregateTopicStats(*chatID, begin, end)
	if err != nil {
		die(err)
	}
	health, err := s.ComputeThreadHealth(*chatID, 30*24*time.Hour)
	if err != nil {
		die(err)
	}

	report := map[string]any{
		"agent":         *agent,
		"chat_id":       *chatID,
		"window_start":  begin.Format(time.RFC3339),
		"window_end":    end.Format(time.RFC3339),
		"decisions":     stats,
		"thread_health": health,
		"proposals":     detectAnomalies(stats, health),
	}

	if *out != "" {
		buf, _ := json.MarshalIndent(report, "", "  ")
		_ = os.WriteFile(*out, buf, 0o644)
	}
	printTopicStatsDashboard(report, stats, health)
}

// detectAnomalies surfaces threshold breaches as suggested-proposal items.
// Returns a list of human-readable problems with hypotheses + target dims.
func detectAnomalies(stats *store.TopicStats, health *store.ThreadHealth) []map[string]any {
	var props []map[string]any

	if stats.TotalDecisions > 50 {
		haikuShare := float64(stats.BySource["haiku"]) / float64(stats.TotalDecisions)
		if haikuShare > 0.5 {
			props = append(props, map[string]any{
				"name":   "high-haiku-fallback-rate",
				"value":  fmt.Sprintf("%.0f%% of decisions hit Haiku", haikuShare*100),
				"hypothesis": "keyword regex too narrow — add more signals for the topics with most Haiku hits",
				"target_dim": "SI",
			})
		}
		if stats.HaikuLatencyP95 > 5000 {
			props = append(props, map[string]any{
				"name":   "haiku-latency-p95-high",
				"value":  fmt.Sprintf("p95 = %dms", stats.HaikuLatencyP95),
				"hypothesis": "Haiku CLI is slow — consider raising timeout, switching to HTTP API, or caching aggressively",
				"target_dim": "perf",
			})
		}
		newRate := float64(stats.NewTopics) / float64(stats.TotalDecisions)
		if newRate > 0.10 {
			props = append(props, map[string]any{
				"name":   "high-new-topic-rate",
				"value":  fmt.Sprintf("%.0f%% of decisions create a new topic", newRate*100),
				"hypothesis": "Haiku is over-creating topics; tighten prompt to bias toward existing-topic match",
				"target_dim": "PD",
			})
		}
	}

	if health.OverdueCommits >= 3 {
		props = append(props, map[string]any{
			"name":   "overdue-commitments-piling-up",
			"value":  fmt.Sprintf("%d overdue commitments across %d threads", health.OverdueCommits, health.ActiveThreads),
			"hypothesis": "heartbeat hook for overdue surfacing not built — agent isn't proactively flagging",
			"target_dim": "L3",
		})
	}

	if health.ActiveThreads >= 5 && health.ThreadsWithNoSummary > health.ActiveThreads/2 {
		props = append(props, map[string]any{
			"name":   "summarizer-not-firing",
			"value":  fmt.Sprintf("%d of %d threads have no summary", health.ThreadsWithNoSummary, health.ActiveThreads),
			"hypothesis": "SummarizeFromTurn heuristic skipping sentences; investigate score function on real responses",
			"target_dim": "L3",
		})
	}

	return props
}

func printTopicStatsDashboard(report map[string]any, stats *store.TopicStats, health *store.ThreadHealth) {
	w := os.Stderr
	fmt.Fprintln(w)
	fmt.Fprintln(w, "─── shell-bench topic-stats ───────────────────────────────────")
	fmt.Fprintf(w, "  agent: %s   window: %s → %s\n",
		report["agent"], stats.WindowStart.Format("01-02 15:04"), stats.WindowEnd.Format("01-02 15:04"))
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Decisions: %d total\n", stats.TotalDecisions)

	if stats.TotalDecisions == 0 {
		fmt.Fprintln(w, "  (no data yet — has classifier been enabled + agents restarted?)")
		fmt.Fprintln(w, "────────────────────────────────────────────────────────────────")
		return
	}

	fmt.Fprintln(w, "    by source:")
	for _, src := range sortedKeys(stats.BySource) {
		share := float64(stats.BySource[src]) / float64(stats.TotalDecisions) * 100
		fmt.Fprintf(w, "      %-10s  %4d  (%.0f%%)\n", src, stats.BySource[src], share)
	}
	fmt.Fprintln(w, "    by topic:")
	for _, t := range sortedKeys(stats.ByTopic) {
		fmt.Fprintf(w, "      %-12s  %4d\n", t, stats.ByTopic[t])
	}
	fmt.Fprintf(w, "    new topics this window: %d\n", stats.NewTopics)
	fmt.Fprintf(w, "    commitments extracted: %d\n", stats.CommitmentsExtracted)
	fmt.Fprintf(w, "    haiku latency p50=%dms p95=%dms\n", stats.HaikuLatencyP50, stats.HaikuLatencyP95)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Thread health:")
	fmt.Fprintf(w, "    active threads:     %d\n", health.ActiveThreads)
	fmt.Fprintf(w, "    stale (>30d):       %d\n", health.StaleThreads)
	fmt.Fprintf(w, "    open commitments:   %d\n", health.TotalOpenCommits)
	fmt.Fprintf(w, "    overdue:            %d\n", health.OverdueCommits)
	fmt.Fprintf(w, "    threads w/o summary: %d / %d\n", health.ThreadsWithNoSummary, health.ActiveThreads)
	fmt.Fprintf(w, "    avg summary length:  %d chars\n", health.AvgSummaryLen)
	fmt.Fprintln(w)

	if ps, ok := report["proposals"].([]map[string]any); ok && len(ps) > 0 {
		fmt.Fprintln(w, "  ⚠ Anomalies detected (file as proposals via /loop):")
		for _, p := range ps {
			fmt.Fprintf(w, "    - %s: %s\n", p["name"], p["value"])
			fmt.Fprintf(w, "        hypothesis: %s\n", p["hypothesis"])
			fmt.Fprintf(w, "        target_dim: %s\n", p["target_dim"])
		}
	} else {
		fmt.Fprintln(w, "  ✓ No anomalies above thresholds")
	}
	fmt.Fprintln(w, "────────────────────────────────────────────────────────────────")
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
