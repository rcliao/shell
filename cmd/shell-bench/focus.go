package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/rcliao/shell/internal/bench"
	"github.com/rcliao/shell/internal/topic"
)

// runFocus evaluates conversation-stickiness against the synthetic seed
// set. Three metrics emitted per probe + aggregate:
//
//   - stickiness_accuracy: fraction of "continuation" turns where the
//     sticky pointer stayed on the correct thread
//   - drift_latency_mean: mean turns from a ground-truth drift event to
//     the sticky pointer catching up (0 = same turn)
//   - recall_accuracy: fraction of callbacks where the sticky pointer
//     correctly returned to a previously-seen thread
//
// Eval is LLM-free — uses the keyword classifier only. Real-world
// production data will be auditable separately via cycle-145's
// sticky_matched slog flags.
func runFocus(args []string) {
	fs := flag.NewFlagSet("focus", flag.ExitOnError)
	out := fs.String("out", "", "JSON output path (default: stdout)")
	verbose := fs.Bool("v", false, "print per-probe scores")
	withHaiku := fs.Bool("with-haiku", false,
		"augment the keyword cascade with real Haiku calls "+
			"(slow — ~12s per uncached keyword miss; ~5 min for full bench)")
	haikuModel := fs.String("haiku-model", "claude-haiku-4-5",
		"model id when --with-haiku is set")
	haikuBinary := fs.String("haiku-binary", "claude",
		"claude CLI binary when --with-haiku is set")
	fs.Parse(args)

	// Construct the classifier. Default = keyword-only (deterministic,
	// fast). --with-haiku swaps in the production-shape cascade for
	// measuring real-Haiku lift over the keyword baseline.
	var classifier topic.Classifier
	mode := "keyword-only"
	var registryCleanup func()
	if *withHaiku {
		mode = "haiku-augmented"
		haiku := topic.NewClaudeCLIHaiku(*haikuBinary, *haikuModel, 12*time.Second)
		reg, cleanup, err := bench.NewBenchRegistry()
		if err != nil {
			die(err)
		}
		registryCleanup = cleanup
		classifier = bench.NewHybridClassifier(haiku, reg)
	} else {
		classifier = bench.NewKeywordOnlyClassifier()
	}
	if registryCleanup != nil {
		defer registryCleanup()
	}

	results := make([]bench.FocusResult, 0, len(bench.FocusSeeds))
	start := time.Now()
	for _, p := range bench.FocusSeeds {
		r, err := bench.RunFocusProbe(p, classifier)
		if err != nil {
			fmt.Fprintf(os.Stderr, "probe %q failed: %v\n", p.Name, err)
			continue
		}
		results = append(results, r)
		if *verbose {
			fmt.Printf("  %-44s  stick=%.2f  drift=%s  recall=%.2f  (drifts=%d, callbacks=%d)\n",
				r.Probe,
				r.StickinessAccuracy,
				latStr(r.DriftLatencyMean),
				r.RecallAccuracy,
				r.DriftEvents, r.CallbackEvents)
		}
	}
	summary := bench.SummarizeFocus(results)
	elapsed := time.Since(start)

	report := map[string]interface{}{
		"dimension":   "FC",
		"timestamp":   time.Now().Format(time.RFC3339),
		"mode":        mode,
		"elapsed_ms":  elapsed.Milliseconds(),
		"summary":     summary,
		"thresholds": map[string]float64{
			"stickiness_min":    0.80,
			"drift_latency_max": 1.0,
			"recall_min":        0.50,
		},
	}

	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		die(err)
	}
	if *out == "" {
		fmt.Println(string(payload))
	} else {
		if err := os.WriteFile(*out, payload, 0644); err != nil {
			die(err)
		}
		fmt.Printf("wrote %s\n", *out)
	}

	// Top-line summary always to stderr so make targets can grep.
	fmt.Fprintf(os.Stderr,
		"FC [%s]  stickiness=%.2f  drift_mean=%s  recall=%.2f  uncaught=%d/%d  (probes=%d, %.1fs)\n",
		mode,
		summary.OverallStickiness, latStr(summary.OverallDriftLatency),
		summary.OverallRecall, summary.UncaughtDrifts, summary.TotalDriftEvents,
		len(summary.Probes), elapsed.Seconds())
}

// latStr formats a latency / accuracy float with -1 as N/A sentinel.
func latStr(f float64) string {
	if f < 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", f)
}
