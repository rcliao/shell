package topic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"regexp"
	"sync"
	"time"
)

// Classifier classifies a user message into a topic.
type Classifier interface {
	Classify(ctx context.Context, msg string) (ClassificationResult, error)
}

// HybridClassifier composes keyword fast-path + Haiku fallback + msg-hash cache.
// Cost model: keyword is free, Haiku is ~$0.0002 per ambiguous turn, cache makes
// repeat queries free across both layers.
type HybridClassifier struct {
	Registry          *Registry
	Haiku             HaikuClient
	KeywordSignals    map[string][]*regexp.Regexp // topic_name → signal regexes
	KeywordHighConfidence int                    // confidence ≥ this skips Haiku
	Cache             *Cache
	clock             func() time.Time
}

// NewHybrid returns a default-configured HybridClassifier.
// Haiku may be nil — cascade then falls through to keyword-or-general.
func NewHybrid(reg *Registry, haiku HaikuClient) *HybridClassifier {
	return &HybridClassifier{
		Registry:              reg,
		Haiku:                 haiku,
		KeywordSignals:        DefaultKeywordSignals(),
		KeywordHighConfidence: 2,
		Cache:                 NewCache(),
		clock:                 time.Now,
	}
}

// Classify implements the cascade: keyword → cache → Haiku → fallback.
func (h *HybridClassifier) Classify(ctx context.Context, msg string) (ClassificationResult, error) {
	// Cache: hashed by msg only (chat-agnostic; if a topic only exists in
	// one chat's registry, that's handled at upsert time).
	key := hashMsg(msg)
	if h.Cache != nil {
		if c, ok := h.Cache.Get(key); ok {
			c.Source = "cache"
			return c, nil
		}
	}

	// Keyword fast-path.
	kw := h.classifyKeyword(msg)
	if kw.Confidence >= float64(h.KeywordHighConfidence) {
		// High-confidence keyword hit — commit, skip Haiku.
		if h.Cache != nil {
			h.Cache.Put(key, kw)
		}
		return kw, nil
	}

	// Haiku fallback — skip on tiny no-signal messages (greetings/acks).
	// Cost discipline: don't burn Haiku tokens on "hey", "ok", "👍".
	// Cycle 77: raised threshold 30 → 50 chars based on production latency
	// observation. Short ambiguous messages rarely need Haiku and the 8s
	// cost isn't worth it on filler turns.
	skipHaiku := h.Haiku == nil || h.Registry == nil ||
		(kw.Confidence == 0 && len(msg) < 50)

	// Capture existing topics once for both Haiku call and post-process
	// normalization (cycle 76).
	var existing []Topic
	if !skipHaiku || h.Registry != nil {
		existing, _ = h.Registry.List(ctx)
	}

	haikuErrored := false
	if !skipHaiku {
		hk, err := h.Haiku.ClassifyTopic(ctx, msg, existing)
		if err != nil {
			haikuErrored = true
			slog.Warn("haiku classify failed", "error", err.Error(), "msg_len", len(msg))
		}
		if err == nil {
			// Cycle 76: normalize against existing registry — Haiku often
			// proposes slight variants ("Collagen Supplement Comparison"
			// vs "Collagen supplements") for the same conversational thread.
			canonName := hk.Topic
			isNew := hk.IsNew
			if hk.IsNew {
				if c, matched := NormalizeName(hk.Topic, existing); matched {
					canonName = c
					isNew = false
				}
			}
			t := Topic{
				Name:        canonName,
				ChatID:      h.Registry.chatID,
				Description: hk.Description,
				LastUsed:    h.clock(),
			}
			result := ClassificationResult{
				Topic:      t,
				IsNew:      isNew,
				Confidence: hk.Confidence,
				Source:     "haiku",
			}
			if h.Cache != nil {
				h.Cache.Put(key, result)
			}
			return result, nil
		}
	}

	// Fallback source detail (cycle 105): distinguish why we're here.
	//   "fallback-haiku-error" — Haiku attempted, errored (timeout/parse/etc)
	//   "fallback-haiku-skip"  — short msg + no keyword signal, Haiku not called
	//   "fallback-keyword-low" — keyword scored 1+ but below high-conf threshold
	fbSource := "fallback"
	switch {
	case haikuErrored:
		fbSource = "fallback-haiku-error"
	case skipHaiku && kw.Confidence == 0:
		fbSource = "fallback-haiku-skip"
	}

	// Fallback: trust the (possibly low-confidence) keyword result.
	// Below high threshold but still has some signal → commit anyway.
	if kw.Confidence >= 1 {
		kw.Source = fbSource
		if kw.Source == "fallback" {
			kw.Source = "fallback-keyword-low"
		}
		if h.Cache != nil {
			h.Cache.Put(key, kw)
		}
		return kw, nil
	}

	// No signal anywhere → general.
	general := ClassificationResult{
		Topic:      Topic{Name: TopicGeneral, ChatID: h.registryChatID()},
		Confidence: 0,
		Source:     fbSource,
	}
	if h.Cache != nil {
		h.Cache.Put(key, general)
	}
	return general, nil
}

func (h *HybridClassifier) registryChatID() int64 {
	if h.Registry == nil {
		return 0
	}
	return h.Registry.chatID
}

// classifyKeyword runs all keyword regexes and returns the highest-confidence
// topic. Confidence = total regex matches across all signals for that topic.
func (h *HybridClassifier) classifyKeyword(msg string) ClassificationResult {
	best := ClassificationResult{
		Topic:      Topic{Name: TopicGeneral, ChatID: h.registryChatID()},
		Confidence: 0,
		Source:     "keyword",
	}
	for name, signals := range h.KeywordSignals {
		var matched []string
		for _, sig := range signals {
			ms := sig.FindAllString(msg, -1)
			matched = append(matched, ms...)
		}
		if len(matched) > int(best.Confidence) {
			best = ClassificationResult{
				Topic:      Topic{Name: name, ChatID: h.registryChatID()},
				Confidence: float64(len(matched)),
				Source:     "keyword",
				MatchedKWs: matched,
			}
		}
	}
	return best
}

func hashMsg(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:12])
}

// Cache is a simple bounded in-memory map of msg-hash → result.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]ClassificationResult
	max     int
}

func NewCache() *Cache {
	return &Cache{entries: make(map[string]ClassificationResult), max: 1000}
}

func (c *Cache) Get(k string) (ClassificationResult, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.entries[k]
	return r, ok
}

func (c *Cache) Put(k string, r ClassificationResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		// crude eviction: drop one arbitrary entry
		for k := range c.entries {
			delete(c.entries, k)
			break
		}
	}
	c.entries[k] = r
}

// DefaultKeywordSignals returns the curated keyword map matching topic.go
// in internal/bench. Kept in sync until we promote to a shared yaml.
func DefaultKeywordSignals() map[string][]*regexp.Regexp {
	return map[string][]*regexp.Regexp{
		"plants": {
			regexp.MustCompile(`(?i)\bplant\b`),
			regexp.MustCompile(`(?i)\b(leaves|leaf)\b`),
			regexp.MustCompile(`(?i)\b(soil|water|watering|overwatered)\b`),
			regexp.MustCompile(`(?i)\b(root|roots|repot|repotted)\b`),
			regexp.MustCompile(`(?i)(巴西木|盆栽|葉子|澆水|根腐)`),
			regexp.MustCompile(`(?i)\b(droopy|wilting|perked)\b`),
		},
		"meals": {
			regexp.MustCompile(`(?i)\b(memo|breakfast|lunch|dinner|snack|meal)\b`),
			regexp.MustCompile(`(?i)\b(eat|ate|eating)\b`),
			regexp.MustCompile(`(早餐|午餐|晚餐)`),
			regexp.MustCompile(`(?i)\bdairy\b`),
		},
		"fortune": {
			regexp.MustCompile(`(?i)\b(fortune|reading|horoscope)\b`),
			regexp.MustCompile(`(運勢|占卜)`),
		},
		"health": {
			regexp.MustCompile(`(?i)\b(med|medication|prescription|pill)\b`),
			regexp.MustCompile(`(?i)\b(symptom|allergy|allergic|sensitivity|reaction)\b`),
			regexp.MustCompile(`(過敏|藥|症狀)`),
			regexp.MustCompile(`(?i)\b(doctor|appointment|clinic)\b`),
		},
		"work": {
			regexp.MustCompile(`(?i)\b(deploy|deployment|prod|production|pr|incident|on-call|oncall)\b`),
			regexp.MustCompile(`(?i)\b(code|coding|bug)\b`),
		},
		"family": {
			regexp.MustCompile(`(?i)\b(papi|mami|eric|maya|umbreon|pikamini|moochi|chonky)\b`),
			regexp.MustCompile(`(老公|老婆|爸爸|媽媽|哥哥|妹妹)`),
		},
		"travel": {
			regexp.MustCompile(`(?i)\b(flight|trip|hotel|airline|airport)\b`),
			regexp.MustCompile(`(機票|住宿|旅館|行李)`),
		},
	}
}
