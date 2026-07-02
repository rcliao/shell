package bridge

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Shadow model-tier router (V2-H11 phase 2). Classifies each turn into the
// tier that WOULD serve it — demanding | deep | everyday | simple — and logs
// the prediction next to the turn's realized complexity (tool calls, output
// tokens). LOG-ONLY: nothing is routed. The ledger answers, before any live
// routing exists: what's the tier distribution, how good are the heuristics,
// and what would the sidecar architecture actually save.
//
// Tiers (owner's framing, 2026-07-01):
//   demanding — dev/build/orchestration work (top model)
//   deep      — research, comparison, multi-step analysis
//   everyday  — memos, short factual Q&A, casual conversation with tools
//   simple    — acks, reactions, greetings; no tools, tiny output expected

var demandingRe = regexp.MustCompile("(?i)(```|\\b(implement|refactor|debug|deploy|rewrite|migrate)\\b|寫程式|修.?bug|部署|重構)")

var deepRe = regexp.MustCompile(`(?i)(\b(research|compare|analy[sz]e|investigate|evaluate|recommend|plan out)\b|深入|研究|調查|分析|比較|評估|規劃|方案|為什麼|why\b)`)

// simple: short, no question, no ask — acks/reactions/greetings.
var simpleAckRe = regexp.MustCompile(`(?i)^(ok(ay)?|thanks?|thank you|lol|haha+|nice|cool|great|good ?(morning|night)|hi|hey|yo)[!.~ ]*$`)
var simpleAckCJK = []string{"好", "好的", "嗯", "嗯嗯", "收到", "謝謝", "谢谢", "哈哈", "早安", "晚安", "辛苦了", "讚", "赞", "可以", "沒問題", "没问题"}

// anyWordRe: does the string contain any letter or digit (incl. CJK)?
var anyWordRe = regexp.MustCompile(`[\p{L}\p{N}]`)

// tierDecision is one shadow-routing observation.
type tierDecision struct {
	tier   string
	reason string
}

// classifyTier predicts the cheapest tier that would serve this turn.
// Heuristic v1 — deliberately rough; the shadow ledger exists to measure and
// tune it against realized complexity before anything goes live.
func classifyTier(userMsg string, isHeartbeat bool, source string) tierDecision {
	if isHeartbeat || source == "scheduler" {
		// Background turns are already model-routed via config (ModelRouting);
		// tag them so interactive analysis can exclude them cleanly.
		return tierDecision{tier: "everyday", reason: "background:" + source}
	}

	msg := strings.TrimSpace(userMsg)
	runes := utf8.RuneCountInString(msg)

	if demandingRe.MatchString(msg) {
		return tierDecision{tier: "demanding", reason: "dev-task marker"}
	}
	if deepRe.MatchString(msg) || runes > 400 {
		if runes > 400 {
			return tierDecision{tier: "deep", reason: "long-form message"}
		}
		return tierDecision{tier: "deep", reason: "research/analysis marker"}
	}

	// simple: tiny message, no question mark, ack-shaped.
	if runes <= 12 && !strings.ContainsAny(msg, "?？") {
		if simpleAckRe.MatchString(msg) {
			return tierDecision{tier: "simple", reason: "ack (en)"}
		}
		stripped := strings.Trim(msg, "!！~。.…⚡🐾💛🌙 ")
		for _, a := range simpleAckCJK {
			if stripped == a {
				return tierDecision{tier: "simple", reason: "ack (cjk)"}
			}
		}
		// No letters or digits at all → pure emoji/sticker reaction.
		if !anyWordRe.MatchString(stripped) {
			return tierDecision{tier: "simple", reason: "emoji/empty"}
		}
	}

	return tierDecision{tier: "everyday", reason: "default"}
}
