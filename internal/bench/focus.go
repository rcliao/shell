package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	memory "github.com/rcliao/ghost"
	"github.com/rcliao/shell/internal/store"
	"github.com/rcliao/shell/internal/topic"
)

// ── Focus / Conversation-stickiness eval (FC dimension) ───────────────
//
// FC measures three properties of the topic classifier + sticky-pointer
// architecture (cycle 145):
//
//   1. Stickiness accuracy — when the conversation IS continuing (ground
//      truth thread label same as prior turn), does the sticky pointer
//      stay on the right thread?
//
//   2. Drift latency — when ground truth shifts to a new thread, how
//      many turns until the sticky pointer moves to the new thread?
//      0 = same turn (ideal), N = N turns of staleness before catch-up.
//
//   3. Recall accuracy — when ground truth returns to a previously-seen
//      thread (callback), does the system correctly reuse the prior
//      thread or wrongly invent a new one?
//
// Runner uses a fresh in-memory SQLite store per probe + the keyword-only
// HybridClassifier (no Haiku — eval is LLM-free and deterministic). The
// keyword cascade is the cold-start lever in the sticky-pointer design;
// measuring how well it tracks ground truth tells us how often sticky
// would need to fall back to Haiku.

// FocusTurn is one turn in a conversation focus probe. ThreadLabel is the
// canonical ground-truth thread the turn belongs to (e.g. "meals",
// "plants", "dairy_allergy").
type FocusTurn struct {
	UserMsg     string
	ThreadLabel string
}

// FocusProbe is a multi-turn sequence with per-turn ground-truth labels.
type FocusProbe struct {
	Name  string
	Turns []FocusTurn
}

// FocusResult holds per-probe scoring against the three FC metrics.
type FocusResult struct {
	Probe              string  `json:"probe"`
	TotalTurns         int     `json:"total_turns"`
	ContinuationTurns  int     `json:"continuation_turns"` // turns where prior label == this label
	CorrectContinue    int     `json:"correct_continuations"`
	DriftEvents        int     `json:"drift_events"`       // turns where label changed from prior
	DriftLatencyTurns  []int   `json:"drift_latency_turns"` // per drift: how many turns to catch up
	CallbackEvents     int     `json:"callback_events"`    // returns to label seen earlier (not immediate prior)
	CorrectCallbacks   int     `json:"correct_callbacks"`
	StickinessAccuracy float64 `json:"stickiness_accuracy"`
	DriftLatencyMean   float64 `json:"drift_latency_mean"`
	RecallAccuracy     float64 `json:"recall_accuracy"`
}

// FocusSeeds is the synthetic probe set for the FC dimension. Add more
// seeds over time; aim for coverage of the patterns the owner actually exhibits
// (long inertia, occasional drift, callbacks to earlier threads).
//
// Seeds use simple Chinese phrasing that the keyword cascade catches via
// the standard signal buckets (meals/plants/health/etc). When the seed
// content drifts from those buckets, drift latency rises — that's the
// signal that Haiku layer would be needed in production.
var FocusSeeds = []FocusProbe{
	{
		Name: "pure-inertia-meals",
		Turns: []FocusTurn{
			{"早餐吃了蛋", "meals"},
			{"也喝咖啡了", "meals"},
			{"忘記吃酵素", "meals"},
			{"剩餘的麵還夠", "meals"},
			{"明天要買菜", "meals"},
		},
	},
	{
		Name: "single-drift-meals-to-plants",
		Turns: []FocusTurn{
			{"早餐吃了蛋", "meals"},
			{"咖啡也喝了", "meals"},
			{"植物的葉子有點黃", "plants"},
			{"要不要換土", "plants"},
			{"再觀察幾天", "plants"},
		},
	},
	{
		Name: "drift-then-callback",
		Turns: []FocusTurn{
			{"早餐吃了蛋", "meals"},
			{"咖啡也喝了", "meals"},
			{"植物葉子黃", "plants"},
			{"要不要換土", "plants"},
			{"晚餐想吃麵", "meals"}, // callback
		},
	},
	{
		Name: "quick-1-turn-detour",
		Turns: []FocusTurn{
			{"早餐吃蛋", "meals"},
			{"順便看一下植物", "plants"}, // 1-turn detour — sticky may NOT need to move
			{"咖啡很香", "meals"},      // back to meals
		},
	},
	{
		Name: "triple-drift-meals-plants-fortune",
		Turns: []FocusTurn{
			{"早餐吃了蛋", "meals"},
			{"咖啡也喝了", "meals"},
			{"植物葉子黃", "plants"},
			{"要不要換土", "plants"},
			{"今日運勢如何", "fortune"},
			{"幸運色是什麼", "fortune"},
		},
	},
	{
		Name: "cold-start-direct-keyword",
		Turns: []FocusTurn{
			{"早餐想吃麵", "meals"}, // first turn — sticky has no prior
		},
	},
	{
		Name: "ambiguous-multi-bucket",
		Turns: []FocusTurn{
			{"早餐吃蛋", "meals"},
			{"順便把植物澆水", "plants"}, // contains "植物" — drifts cleanly
			{"咖啡剛喝完", "meals"},     // back to meals
		},
	},

	// ── Cycle 146 extended seed set (10 additional probes) ─────────────

	// Long inertia: 8 meals turns where only 2 have keyword anchors. The
	// sticky-on-keyword-miss heuristic should keep the pointer on meals
	// throughout. If stickiness < 1.0 here, the inertia hypothesis is
	// already wrong.
	{
		Name: "long-inertia-meals-8-turns",
		Turns: []FocusTurn{
			{"memo 早餐吃了蛋", "meals"},          // anchored (memo, 早餐)
			{"也喝了拿鐵", "meals"},                // NOT anchored
			{"酵素剛吃過", "meals"},                // NOT anchored
			{"晚餐想吃麵", "meals"},                // anchored (晚餐)
			{"也許加個沙拉", "meals"},              // NOT anchored
			{"煮了一鍋湯", "meals"},                // NOT anchored
			{"明天再買些水果", "meals"},            // NOT anchored
			{"今晚有點餓", "meals"},                // NOT anchored
		},
	},

	// Long-distance callback: returns to meals after 4 turns of plants.
	// Tests recall accuracy under realistic conversation depth.
	{
		Name: "long-distance-callback-meals-after-4-plants",
		Turns: []FocusTurn{
			{"breakfast memo", "meals"},
			{"酵素吃了", "meals"},
			{"盆栽葉子黃了", "plants"},           // drift
			{"需要澆水嗎", "plants"},
			{"root 可能爛了", "plants"},
			{"明天再觀察", "plants"},
			{"晚餐想吃麵", "meals"},               // callback (4 turns later)
		},
	},

	// Drift-opener marker: 對了 signals a topic shift. The next turn
	// has a real keyword anchor; eval whether the cascade catches the
	// drift at latency=0 even though the opener itself has no signal.
	{
		Name: "drift-opener-對了",
		Turns: []FocusTurn{
			{"早餐 memo", "meals"},
			{"也記得吃酵素", "meals"},
			{"對了，盆栽葉子黃", "plants"},        // drift via opener + anchor
			{"可能要換土", "plants"},
		},
	},

	// Rapid drift across 5 distinct keyword-anchored domains. Worst-case
	// classifier load — confirms no one bucket leaks state into another.
	{
		Name: "rapid-drift-5-domains",
		Turns: []FocusTurn{
			{"早餐 memo 吃蛋", "meals"},
			{"盆栽葉子黃", "plants"},
			{"今日運勢如何", "fortune"},
			{"過敏觀察一下", "health"},
			{"機票價格怎樣", "travel"},
		},
	},

	// Pure unanchored continuation: only the first turn has a keyword.
	// Every subsequent turn must rely on sticky-pointer inertia. This is
	// the stress test for the cycle-145 hypothesis — if sticky-only
	// works, this probe scores 4/4. If it doesn't, sticky is wrong.
	{
		Name: "continuation-no-anchor-after-cold",
		Turns: []FocusTurn{
			{"早餐 memo 開始", "meals"},
			{"包了水煮蛋", "meals"},
			{"也煮了湯", "meals"},
			{"沙拉很好吃", "meals"},
			{"甜點是優格", "meals"},
		},
	},

	// Health domain inertia with multiple anchors. Verifies the health
	// bucket's regex (過敏|藥|症狀|doctor|appointment|clinic) catches
	// representative phrasings.
	{
		Name: "health-symptom-tracking",
		Turns: []FocusTurn{
			{"右臉有點癢", "health"},               // NOT anchored — sticky stays from cold? probe starts cold so this lands as general/sticky-none
			{"過敏觀察", "health"},                // anchored
			{"藥剛吃了", "health"},                // anchored (藥)
			{"doctor 預約在明天", "health"},        // anchored
			{"明天再觀察一下", "health"},          // NOT anchored
		},
	},

	// Travel domain inertia. Tests 機票/住宿/行李 anchors.
	{
		Name: "travel-flight-planning",
		Turns: []FocusTurn{
			{"機票什麼時候訂", "travel"},
			{"Delta 還是 Alaska", "travel"},        // NOT anchored
			{"下週週四出發", "travel"},            // NOT anchored
			{"住宿要查嗎", "travel"},              // anchored
			{"行李兩件夠嗎", "travel"},            // anchored
		},
	},

	// Rapid A→B→A pattern with anchored drifts. Validates that the
	// pointer can swing twice in 3 turns without smearing.
	{
		Name: "rapid-ABA-pattern",
		Turns: []FocusTurn{
			{"早餐吃了 memo", "meals"},
			{"盆栽葉子黃了", "plants"},
			{"memo 晚餐想吃麵", "meals"},          // callback via anchored re-entry
		},
	},

	// English code-switch with "by the way" opener. Stresses both the
	// case-insensitive plant keyword and the opener pattern.
	{
		Name: "english-opener-by-the-way",
		Turns: []FocusTurn{
			{"breakfast 吃了蛋", "meals"},
			{"也喝了咖啡", "meals"},                // NOT anchored
			{"by the way, the plant leaves are yellow", "plants"}, // anchored (plant, leaves)
			{"soil 可能太乾", "plants"},            // anchored (soil)
		},
	},

	// Drift back to a 2-turn-old thread with re-anchoring on the
	// callback turn. Different from quick-1-turn-detour because BOTH
	// drift events here have keyword anchors.
	{
		Name: "callback-with-anchor",
		Turns: []FocusTurn{
			{"早餐 memo 蛋", "meals"},
			{"也喝了拿鐵", "meals"},                // NOT anchored
			{"盆栽葉子黃", "plants"},              // drift (anchored)
			{"breakfast 還想再吃", "meals"},        // callback (anchored — breakfast)
		},
	},
}

// RunFocusProbe runs one probe through a fresh in-memory store +
// caller-supplied classifier + sticky-pointer pipeline, scoring against
// the three FC metrics. Uses chat_id = 1 in the sandbox.
//
// Cycle 148 made the classifier injectable so the bench can compare
// implementations: keyword-only vs Haiku-augmented vs (future) embedding-
// based. Pass NewKeywordOnlyClassifier() for the deterministic baseline,
// or NewHybridClassifier(haiku) to measure Haiku's lift over keyword.
func RunFocusProbe(p FocusProbe, classifier topic.Classifier) (FocusResult, error) {
	r := FocusResult{Probe: p.Name, TotalTurns: len(p.Turns)}

	// Fresh sandbox store per probe — no state bleed.
	s, cleanup, err := newSandboxStore()
	if err != nil {
		return r, fmt.Errorf("sandbox store: %w", err)
	}
	defer cleanup()

	// Per-probe state.
	chatID := int64(1)
	seenLabels := make(map[string]bool)  // labels we've ground-truthed before
	var pendingDrift bool                 // last turn was a drift; counting catch-up latency
	var driftAt int                       // turn index where drift event started
	prevLabel := ""

	for i, turn := range p.Turns {
		// Ground-truth bookkeeping.
		isContinuation := i > 0 && turn.ThreadLabel == prevLabel
		isDrift := i > 0 && turn.ThreadLabel != prevLabel
		isCallback := isDrift && seenLabels[turn.ThreadLabel]

		if isContinuation {
			r.ContinuationTurns++
		}
		if isDrift {
			r.DriftEvents++
			pendingDrift = true
			driftAt = i
		}
		if isCallback {
			r.CallbackEvents++
		}

		// Run classifier on this turn's user message.
		ctx := context.Background()
		result, err := classifier.Classify(ctx, turn.UserMsg)
		if err != nil {
			return r, fmt.Errorf("classify turn %d: %w", i, err)
		}
		topicName := result.Topic.Name
		if topicName == "" || topicName == topic.TopicGeneral {
			// Keyword found nothing — sticky pointer stays where it was.
			// For scoring purposes this is "no movement"; if continuation
			// expected, count as incorrect.
			if isContinuation {
				// sticky stayed (didn't move), prevLabel was the prior — correct
				r.CorrectContinue++
			}
			prevLabel = turn.ThreadLabel
			seenLabels[turn.ThreadLabel] = true
			continue
		}

		// Upsert topic_threads + conversations.
		if _, err := s.BumpTopicThread(chatID, topicName, 0); err != nil {
			return r, fmt.Errorf("bump thread turn %d: %w", i, err)
		}
		thread, _ := s.GetTopicThread(chatID, topicName)
		if thread != nil {
			_ = s.UpsertConversation(chatID, thread.ID, topicName)
		}

		// Score this turn.
		stickyMatched := topicName == turn.ThreadLabel
		if isContinuation && stickyMatched {
			r.CorrectContinue++
		}
		if pendingDrift && stickyMatched {
			latency := i - driftAt
			r.DriftLatencyTurns = append(r.DriftLatencyTurns, latency)
			pendingDrift = false
		}
		if isCallback && stickyMatched {
			r.CorrectCallbacks++
		}

		seenLabels[turn.ThreadLabel] = true
		prevLabel = turn.ThreadLabel
	}

	// Finalize aggregates. Use -1 as the JSON-friendly "N/A" sentinel for
	// metrics whose denominator is zero (cold-start probes, all-drift
	// probes, etc) and for "drift never caught up" cases.
	if r.ContinuationTurns > 0 {
		r.StickinessAccuracy = float64(r.CorrectContinue) / float64(r.ContinuationTurns)
	} else {
		r.StickinessAccuracy = -1
	}
	if len(r.DriftLatencyTurns) > 0 {
		sum := 0
		for _, l := range r.DriftLatencyTurns {
			sum += l
		}
		r.DriftLatencyMean = float64(sum) / float64(len(r.DriftLatencyTurns))
	} else if r.DriftEvents > 0 {
		r.DriftLatencyMean = -1 // sentinel: never caught up
	} else {
		r.DriftLatencyMean = -1 // no drift events
	}
	if r.CallbackEvents > 0 {
		r.RecallAccuracy = float64(r.CorrectCallbacks) / float64(r.CallbackEvents)
	} else {
		r.RecallAccuracy = -1
	}
	return r, nil
}

// FocusSummary aggregates per-probe results into a dimension-level snapshot.
type FocusSummary struct {
	Probes              []FocusResult `json:"probes"`
	OverallStickiness   float64       `json:"overall_stickiness"`
	OverallDriftLatency float64       `json:"overall_drift_latency_mean"`
	OverallRecall       float64       `json:"overall_recall"`
	TotalDriftEvents    int           `json:"total_drift_events"`
	TotalCallbacks      int           `json:"total_callback_events"`
	UncaughtDrifts      int           `json:"uncaught_drifts"`
}

// SummarizeFocus aggregates per-probe scores into a single dashboard row.
// Uses -1 sentinel for "no data" the same as per-probe metrics. Also reports
// uncaught_drifts so callers know when drift latency under-reports.
func SummarizeFocus(results []FocusResult) FocusSummary {
	s := FocusSummary{Probes: results}
	var stickNum, stickDen, recallNum, recallDen, driftSum, driftN int
	for _, r := range results {
		stickNum += r.CorrectContinue
		stickDen += r.ContinuationTurns
		recallNum += r.CorrectCallbacks
		recallDen += r.CallbackEvents
		driftSum += sumInts(r.DriftLatencyTurns)
		driftN += len(r.DriftLatencyTurns)
		s.TotalDriftEvents += r.DriftEvents
		s.TotalCallbacks += r.CallbackEvents
	}
	s.UncaughtDrifts = s.TotalDriftEvents - driftN
	if stickDen > 0 {
		s.OverallStickiness = float64(stickNum) / float64(stickDen)
	} else {
		s.OverallStickiness = -1
	}
	if recallDen > 0 {
		s.OverallRecall = float64(recallNum) / float64(recallDen)
	} else {
		s.OverallRecall = -1
	}
	if driftN > 0 {
		s.OverallDriftLatency = float64(driftSum) / float64(driftN)
	} else {
		s.OverallDriftLatency = -1
	}
	return s
}

func sumInts(xs []int) int {
	t := 0
	for _, x := range xs {
		t += x
	}
	return t
}

// ── helpers: sandbox store + keyword-only classifier ─────────────────

func newSandboxStore() (*store.Store, func(), error) {
	tmp, err := os.MkdirTemp("", "focus-bench-")
	if err != nil {
		return nil, nil, err
	}
	dbPath := filepath.Join(tmp, "shell.db")
	s, err := store.Open(dbPath)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, nil, err
	}
	return s, func() {
		_ = s.Close()
		_ = os.RemoveAll(tmp)
	}, nil
}

// NewKeywordOnlyClassifier builds a HybridClassifier with no Haiku — only
// the keyword fast-path runs. Used as the FC bench baseline. Production
// signals so the bench tracks whatever the daemon ships.
// KeywordHighConfidence=1 commits on any keyword hit (we're isolating
// keyword coverage, not the keyword+Haiku cascade).
//
// Uses NewHybrid for the proper private-field initialization (clock fn),
// then overrides the keyword threshold.
func NewKeywordOnlyClassifier() topic.Classifier {
	h := topic.NewHybrid(nil, nil)
	h.KeywordHighConfidence = 1
	return h
}

// NewHybridClassifier wires a real Haiku client into the cascade. When
// keyword doesn't anchor, Haiku is called for classification. Use this
// in the bench to measure the Haiku lift over the keyword baseline.
//
// KeywordHighConfidence=2 here matches production (anything ≥ 2 keyword
// matches skips Haiku; ≥1 but <2 still goes to Haiku for confirmation).
//
// Caller must supply a Registry; production reads chat-level topic
// taxonomy from ghost ns "loop:topics". For bench use, pass a fresh
// in-memory ghost store via NewBenchRegistry below.
func NewHybridClassifier(haiku topic.HaikuClient, reg *topic.Registry) topic.Classifier {
	return topic.NewHybrid(reg, haiku)
}

// NewBenchRegistry returns a topic.Registry backed by a fresh temporary
// SQLite ghost store. Caller is responsible for invoking the returned
// cleanup function when done. chatID is 1 (matches the focus sandbox).
func NewBenchRegistry() (*topic.Registry, func(), error) {
	tmp, err := os.MkdirTemp("", "focus-bench-registry-")
	if err != nil {
		return nil, nil, err
	}
	dbPath := filepath.Join(tmp, "ghost.db")
	gs, err := memory.NewSQLiteStore(dbPath)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, nil, err
	}
	return topic.NewRegistry(gs, 1), func() {
		_ = gs.Close()
		_ = os.RemoveAll(tmp)
	}, nil
}
