package bench

import (
	"regexp"
	"strings"
)

// ── Topic classifier (LLM-free) ──────────────────────────────────────────
//
// Cycle 63 foundation for L3 thread-state architecture. Classifies user
// messages into one of ~8 topics using keyword+regex matching. Designed
// to be cheap, deterministic, and explainable. Confidence is a count of
// matched signals; fall through to "general" when no topic scores high
// enough.
//
// Topics chosen from observed owner traffic patterns:
//   plants    — plant care, watering, soil, leaves, specific species
//   meals     — meal logs, food, dairy tracking, recipes
//   fortune   — daily fortune readings, mood, reflection
//   health    — medications, symptoms, allergies, sensitivities
//   work      — coding, deploys, on-call, incidents
//   family    — relationships, kids, household members
//   travel    — trips, flights, hotels, packing
//   general   — catch-all fallback (zero confidence in any topic)

// Topic represents a classified conversation topic.
type Topic string

const (
	TopicPlants  Topic = "plants"
	TopicMeals   Topic = "meals"
	TopicFortune Topic = "fortune"
	TopicHealth  Topic = "health"
	TopicWork    Topic = "work"
	TopicFamily  Topic = "family"
	TopicTravel  Topic = "travel"
	TopicGeneral Topic = "general"
)

// TopicConfidence is the count of matched signal-tokens for a topic.
// Higher = more confident. Confidence < confidenceFloor falls back to general.
type TopicConfidence struct {
	Topic      Topic   `json:"topic"`
	Confidence int     `json:"confidence"` // raw signal-match count
	Score      float64 `json:"score"`      // normalized 0..1
	Matched    []string `json:"matched,omitempty"` // tokens that matched
}

const confidenceFloor = 1 // any matched topic signal commits, with general as zero-match fallback

// topicSignals are keyword regexes per topic. Each match contributes 1 to
// the confidence count. Word boundaries used to avoid partial matches
// (e.g. "plant" doesn't match "implant").
var topicSignals = map[Topic][]*regexp.Regexp{
	TopicPlants: {
		regexp.MustCompile(`(?i)\bplant\b`),
		regexp.MustCompile(`(?i)\b(leaves|leaf)\b`),
		regexp.MustCompile(`(?i)\b(soil|water|watering|overwatered)\b`),
		regexp.MustCompile(`(?i)\b(root|roots|repot|repotted)\b`),
		regexp.MustCompile(`(?i)(巴西木|盆栽|葉子|澆水|根腐)`),
		regexp.MustCompile(`(?i)\b(droopy|wilting|perked)\b`),
	},
	TopicMeals: {
		regexp.MustCompile(`(?i)\b(memo|breakfast|lunch|dinner|snack|meal)\b`),
		regexp.MustCompile(`(?i)\b(eat|ate|eating)\b`),
		regexp.MustCompile(`(早餐|午餐|晚餐|memo)`),
		regexp.MustCompile(`(?i)\bdairy\b`),
		regexp.MustCompile(`(?i)\b(latte|coffee|tea|americano)\b`),
	},
	TopicFortune: {
		regexp.MustCompile(`(?i)\b(fortune|reading|horoscope)\b`),
		regexp.MustCompile(`(運勢|占卜)`),
		regexp.MustCompile(`(?i)\b(today's energy|the day ahead)\b`),
	},
	TopicHealth: {
		regexp.MustCompile(`(?i)\b(med|medication|prescription|pill)\b`),
		regexp.MustCompile(`(?i)\b(symptom|symptoms|pain|fever|cough)\b`),
		regexp.MustCompile(`(?i)\b(allergy|allergic|sensitivity|reaction)\b`),
		regexp.MustCompile(`(過敏|藥|症狀|生病)`),
		regexp.MustCompile(`(?i)\b(doctor|doc|appointment|clinic|hospital)\b`),
	},
	TopicWork: {
		regexp.MustCompile(`(?i)\b(deploy|deployment|prod|production|pr|pull request)\b`),
		regexp.MustCompile(`(?i)\b(code|coding|bug|incident|on-call|oncall)\b`),
		regexp.MustCompile(`(?i)\b(meeting|standup|sync)\b`),
	},
	TopicFamily: {
		regexp.MustCompile(`(?i)\b(papi|mami|eric|maya|umbreon|pikamini|moochi|chonky)\b`),
		regexp.MustCompile(`(老公|老婆|爸爸|媽媽|哥哥|妹妹)`),
		regexp.MustCompile(`(?i)\b(kid|kids|son|daughter|family)\b`),
	},
	TopicTravel: {
		regexp.MustCompile(`(?i)\b(flight|trip|hotel|airline|airport)\b`),
		regexp.MustCompile(`(?i)\b(packing|luggage|passport|visa)\b`),
		regexp.MustCompile(`(機票|住宿|旅館|行李)`),
	},
}

// Classify returns the highest-confidence topic for a message, falling
// back to general when no topic scores ≥ confidenceFloor.
// Confidence counts ALL match instances (not just distinct regex hits) so
// multi-word matches contribute proportionally.
func Classify(message string) TopicConfidence {
	best := TopicConfidence{Topic: TopicGeneral, Confidence: 0, Score: 0}
	for topic, signals := range topicSignals {
		var matched []string
		for _, sig := range signals {
			ms := sig.FindAllString(message, -1)
			matched = append(matched, ms...)
		}
		conf := len(matched)
		if conf > best.Confidence {
			best = TopicConfidence{Topic: topic, Confidence: conf, Score: float64(conf) / float64(len(signals)), Matched: matched}
		}
	}
	if best.Confidence < confidenceFloor {
		return TopicConfidence{Topic: TopicGeneral, Confidence: 0, Score: 0}
	}
	return best
}

// ClassifyBatch returns topic distribution across a sequence of messages —
// useful for thread-state reconstruction in cycles 64+.
func ClassifyBatch(messages []string) map[Topic]int {
	counts := make(map[Topic]int)
	for _, m := range messages {
		c := Classify(m)
		counts[c.Topic]++
	}
	return counts
}

// MajorityTopic returns the topic with the most signals across a batch.
// Returns general when no topic dominates.
func MajorityTopic(messages []string) Topic {
	counts := ClassifyBatch(messages)
	delete(counts, TopicGeneral) // don't let general win by default
	var winner Topic = TopicGeneral
	var max int
	for t, c := range counts {
		if c > max {
			max = c
			winner = t
		}
	}
	if max < 1 {
		return TopicGeneral
	}
	return winner
}

// pluckSnippet returns the first 200 chars with whitespace collapsed —
// helper for thread-summary construction.
func pluckSnippet(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
