package bench

import (
	"regexp"
	"strings"
	"unicode"
)

// Retrieval metrics — mirror ghost/internal/store/eval_metrics.go semantics.
// Reimplemented here because ghost's versions live in an internal/ package.

func RecallAtK(retrieved, relevant []string, k int) float64 {
	if len(relevant) == 0 {
		return 0
	}
	if k > len(retrieved) {
		k = len(retrieved)
	}
	rel := make(map[string]bool, len(relevant))
	for _, r := range relevant {
		rel[r] = true
	}
	hits := 0
	for i := 0; i < k; i++ {
		if rel[retrieved[i]] {
			hits++
		}
	}
	return float64(hits) / float64(len(relevant))
}

func MRR(retrieved []string, relevant map[string]bool) float64 {
	for i, r := range retrieved {
		if relevant[r] {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

// Answer-content metrics — mirror ghost/internal/store/e2e_bench.go primitives.
// LLM-free. Operate on normalized strings.

var (
	wsRe    = regexp.MustCompile(`\s+`)
	tokenRe = regexp.MustCompile(`[\p{L}\p{N}]+`)
)

func Normalize(s string) string {
	s = strings.ToLower(s)
	s = wsRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// Contains returns 1 if gold substring is present in answer, else 0.
func Contains(answer, gold string) float64 {
	if strings.Contains(Normalize(answer), Normalize(gold)) {
		return 1
	}
	return 0
}

// TokenRecall: fraction of gold content tokens (>=2 chars or any CJK)
// present in the answer.
func TokenRecall(answer, gold string) float64 {
	a := tokenize(answer)
	g := tokenize(gold)
	if len(g) == 0 {
		return 0
	}
	have := make(map[string]bool, len(a))
	for _, t := range a {
		have[t] = true
	}
	hits := 0
	for _, t := range g {
		if have[t] {
			hits++
		}
	}
	return float64(hits) / float64(len(g))
}

// FlexibleContains: 1 if all "meaningful tokens" from gold appear in answer.
// "Meaningful" = >=2 chars or any character outside ASCII (covers CJK).
func FlexibleContains(answer, gold string) float64 {
	g := meaningfulTokens(gold)
	if len(g) == 0 {
		return Contains(answer, gold)
	}
	a := Normalize(answer)
	for _, t := range g {
		if !strings.Contains(a, t) {
			return 0
		}
	}
	return 1
}

func tokenize(s string) []string {
	return tokenRe.FindAllString(Normalize(s), -1)
}

// meaningfulTokens returns search-query tokens that align with FTS5's
// unicode61 indexing. CJK runs are emitted as 2-character bigrams (matching
// how unicode61 indexes adjacent CJK chars as separate tokens, queryable as
// phrase-of-2). ASCII tokens are kept whole when ≥2 chars.
func meaningfulTokens(s string) []string {
	out := []string{}
	for _, t := range tokenize(s) {
		if !containsNonASCII(t) {
			if len(t) >= 2 {
				out = append(out, t)
			}
			continue
		}
		runes := []rune(t)
		if len(runes) < 2 {
			out = append(out, t)
			continue
		}
		for i := 0; i+1 < len(runes); i++ {
			out = append(out, string(runes[i:i+2]))
		}
	}
	return out
}

func containsNonASCII(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return true
		}
	}
	return false
}
