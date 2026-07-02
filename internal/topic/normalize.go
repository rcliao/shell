package topic

import (
	"strings"
	"unicode"
)

// ── Topic-name normalization (cycle 76) ──────────────────────────────────
//
// Haiku can propose slight variants of similar topics across turns:
//   "Collagen Supplement Comparison" (msg 1)
//   "Collagen supplements"           (msg 2)
//   "collagen"                       (msg 3)
//
// All refer to the same conversational thread, but the registry treats them
// as 3 distinct topics → thread state fragments. NormalizeName resolves a
// proposed new topic against the existing registry; if a fuzzy-match exists,
// returns the canonical name + true. Caller should then set is_new=false.
//
// LLM-free: lowercase-prefix + word-overlap heuristics. Conservative — we
// only match when the proposed and existing topics share their first
// meaningful word (e.g. "collagen") and have ≥50% token overlap.

// NormalizeName returns (canonical, matched). When matched=true, prefer
// the canonical existing topic name over the proposed one.
//
// Cycle 144 relaxation (drives from cycle 101's "A2 Dairy Tolerance" +
// "Dairy Allergy Management" production split):
//   - Cycle 76 required eTokens[0] == pTokens[0] (first-word match). That
//     misses positionally-divergent shared anchors ("dairy" in pos 1 vs 0).
//   - New rule: match if proposed and existing share ANY substantive
//     token (length ≥ 4, not a stop-word). This catches the dairy case
//     and similar (Plant Watering / Watering Schedule).
//   - The expanded stop-word list (added below) prevents false-positive
//     merges on generic suffix nouns ("allergy", "management", "routine"),
//     so "Cashew Allergy" and "Dairy Allergy" still split correctly —
//     "allergy" is filtered out, leaving no shared substantive token.
func NormalizeName(proposed string, existing []Topic) (string, bool) {
	if proposed == "" || len(existing) == 0 {
		return proposed, false
	}
	pTokens := nameTokens(proposed)
	if len(pTokens) == 0 {
		return proposed, false
	}

	for _, e := range existing {
		if e.Name == "" || e.Status == "pruned" {
			continue
		}
		// Exact-match shortcut (case-insensitive).
		if strings.EqualFold(e.Name, proposed) {
			return e.Name, true
		}
		eTokens := nameTokens(e.Name)
		if len(eTokens) == 0 {
			continue
		}
		// Require at least one shared substantive token (length ≥ 4)
		// after stop-word filtering. Substantive = topical anchor like
		// "dairy", "plant", "mattress" — not generic suffix nouns.
		if !hasSubstantiveOverlap(pTokens, eTokens) {
			continue
		}
		return e.Name, true
	}
	return proposed, false
}

// hasSubstantiveOverlap reports whether a and b share at least one
// token of length ≥ 4. Stop-words are already filtered out by
// nameTokens, so any shared 4+-char token is treated as topical.
func hasSubstantiveOverlap(a, b []string) bool {
	set := make(map[string]bool, len(b))
	for _, t := range b {
		if len(t) >= 4 {
			set[t] = true
		}
	}
	for _, t := range a {
		if len(t) >= 4 && set[t] {
			return true
		}
	}
	return false
}

// nameTokens splits a topic name into lowercased meaningful tokens.
// Stopwords filtered to avoid false-positive matches on filler words.
// Cycle 144 expanded the list with generic suffix nouns ("allergy",
// "management", "routine", etc.) so that names sharing only a generic
// suffix don't trigger merges. "Cashew Allergy" vs "Dairy Allergy"
// should split — they share "allergy" but no substantive anchor.
var nameStop = map[string]bool{
	"and": true, "of": true, "the": true, "for": true, "to": true,
	"a": true, "an": true, "vs": true, "comparison": true, "discussion": true,
	// Generic suffix nouns (cycle 144):
	"allergy": true, "management": true, "routine": true, "schedule": true,
	"care": true, "tracking": true, "issue": true, "issues": true,
	"problem": true, "problems": true, "topics": true, "thing": true,
	"things": true, "stuff": true, "session": true, "sessions": true,
}

func nameTokens(s string) []string {
	var out []string
	var cur strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			cur.WriteRune(unicode.ToLower(r))
			continue
		}
		if cur.Len() > 0 {
			t := cur.String()
			cur.Reset()
			if !nameStop[t] && len(t) >= 2 {
				out = append(out, t)
			}
		}
	}
	if cur.Len() > 0 {
		t := cur.String()
		if !nameStop[t] && len(t) >= 2 {
			out = append(out, t)
		}
	}
	return out
}

func overlapCount(a, b []string) int {
	set := make(map[string]bool, len(b))
	for _, t := range b {
		set[t] = true
	}
	c := 0
	for _, t := range a {
		if set[t] {
			c++
		}
	}
	return c
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
