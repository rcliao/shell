package bench

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"unicode"

	_ "modernc.org/sqlite"
)

// ── PD (Persona Distinctness) dimension ──────────────────────────────────
//
// PD measures whether two agents sound like meaningfully different agents.
// LLM-free: builds a per-agent "signature" from token frequencies over
// recent assistant messages, then runs a holdout-split classifier and
// reports attribution accuracy.
//
// Goal-state interpretation:
//   PD.accuracy ≥ 0.85  → agents are reliably distinguishable
//   PD.accuracy 0.5     → indistinguishable (no persona)
//   PD.accuracy ≈ 0.5   → indistinguishable; suggests persona collapse
//
// Aligned with the user's stated north star: agents developing distinct
// personalities. PD is the regression net that catches persona drift /
// homogenization across loop cycles.

// PDReport holds the attribution-test outcome for one A-vs-B comparison.
type PDReport struct {
	AgentA           string       `json:"agent_a"`
	AgentB           string       `json:"agent_b"`
	TotalMessages    int          `json:"total_messages"`
	TrainSize        int          `json:"train_size"`
	TestSize         int          `json:"test_size"`
	Accuracy         float64      `json:"accuracy"`           // overall
	AccuracyA        float64      `json:"accuracy_a"`         // A-messages correctly identified as A
	AccuracyB        float64      `json:"accuracy_b"`         // B-messages correctly identified as B
	SignatureTokensA []TokenScore `json:"signature_tokens_a"` // top-K distinctively A
	SignatureTokensB []TokenScore `json:"signature_tokens_b"` // top-K distinctively B
	ConfusionMatrix  [][]int      `json:"confusion_matrix"`   // [[TPa, FNa], [FPb, TPb]]
}

// TokenScore is one distinctive token with its per-agent frequencies.
type TokenScore struct {
	Token         string  `json:"token"`
	DistinctScore float64 `json:"distinct_score"` // freqA - freqB
	FreqA         float64 `json:"freq_a"`
	FreqB         float64 `json:"freq_b"`
}

// PersonaDistinctness loads recent assistant messages for two agents,
// holds out a fraction for test, builds signatures from the training half,
// and reports the classifier's attribution accuracy.
func PersonaDistinctness(a, b AgentTarget, limit, topK int, holdoutFrac float64) (*PDReport, error) {
	if topK <= 0 {
		topK = 30
	}
	if holdoutFrac <= 0 || holdoutFrac >= 1 {
		holdoutFrac = 0.2
	}

	// Cycle 62: load (id, content) and split by msg_id parity for true
	// deterministic stability across runs. Old splitDeterministic used array
	// index which shuffled as new messages joined the window.
	idMsgsA, err := loadAgentAssistantMessagesWithID(a.ShellDB, limit)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", a.Name, err)
	}
	idMsgsB, err := loadAgentAssistantMessagesWithID(b.ShellDB, limit)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", b.Name, err)
	}

	step := int(1.0 / holdoutFrac)
	if step < 2 {
		step = 2
	}
	trainA, testA := splitByIDParity(idMsgsA, step)
	trainB, testB := splitByIDParity(idMsgsB, step)
	msgsA := allContents(idMsgsA)
	msgsB := allContents(idMsgsB)

	freqA := tokenFreqInMessages(trainA)
	freqB := tokenFreqInMessages(trainB)

	scores := distinctiveTokens(freqA, freqB)
	sigA, sigB := topKByDirection(scores, topK)

	correct, correctA, correctB := 0, 0, 0
	tpA, fnA, fpB, tpB := 0, 0, 0, 0
	classify := buildClassifier(sigA, sigB)

	for _, m := range testA {
		if classify(m) == "A" {
			correct++
			correctA++
			tpA++
		} else {
			fnA++
		}
	}
	for _, m := range testB {
		if classify(m) == "B" {
			correct++
			correctB++
			tpB++
		} else {
			fpB++
		}
	}

	totalTest := len(testA) + len(testB)
	rep := &PDReport{
		AgentA:        a.Name,
		AgentB:        b.Name,
		TotalMessages: len(msgsA) + len(msgsB),
		TrainSize:     len(trainA) + len(trainB),
		TestSize:      totalTest,
		ConfusionMatrix: [][]int{
			{tpA, fnA},
			{fpB, tpB},
		},
		SignatureTokensA: sigA,
		SignatureTokensB: sigB,
	}
	if totalTest > 0 {
		rep.Accuracy = float64(correct) / float64(totalTest)
	}
	if len(testA) > 0 {
		rep.AccuracyA = float64(correctA) / float64(len(testA))
	}
	if len(testB) > 0 {
		rep.AccuracyB = float64(correctB) / float64(len(testB))
	}
	return rep, nil
}

func loadAgentAssistantMessages(shellDB string, limit int) ([]string, error) {
	msgs, err := loadAgentAssistantMessagesWithID(shellDB, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.content
	}
	return out, nil
}

type idMsg struct {
	id      int64
	content string
}

func loadAgentAssistantMessagesWithID(shellDB string, limit int) ([]idMsg, error) {
	if limit <= 0 {
		limit = 1000
	}
	db, err := sql.Open("sqlite", shellDB+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`
		SELECT id, content
		FROM messages
		WHERE role = 'assistant'
		  AND length(content) > 10
		ORDER BY id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []idMsg
	for rows.Next() {
		var m idMsg
		if err := rows.Scan(&m.id, &m.content); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// splitByIDParity is deterministic across runs: every message with
// id%step==0 lands in test. New messages join either set based on their
// own id, not their position in the array.
func splitByIDParity(msgs []idMsg, step int) (train, test []string) {
	for _, m := range msgs {
		if m.id%int64(step) == 0 {
			test = append(test, m.content)
		} else {
			train = append(train, m.content)
		}
	}
	return
}

func allContents(msgs []idMsg) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.content
	}
	return out
}

// splitDeterministic returns train/test by index: every Kth message goes to
// test where K = round(1/holdoutFrac). Deterministic across runs.
func splitDeterministic(msgs []string, holdoutFrac float64) (train, test []string) {
	if len(msgs) == 0 {
		return nil, nil
	}
	step := int(math.Round(1.0 / holdoutFrac))
	if step < 2 {
		step = 2
	}
	for i, m := range msgs {
		if i%step == 0 {
			test = append(test, m)
		} else {
			train = append(train, m)
		}
	}
	return
}

// tokenFreqInMessages returns per-token presence-frequency: fraction of
// messages that contain the token at least once. Uses pdTokens.
func tokenFreqInMessages(msgs []string) map[string]float64 {
	if len(msgs) == 0 {
		return map[string]float64{}
	}
	counts := make(map[string]int)
	for _, m := range msgs {
		seen := make(map[string]bool)
		for _, t := range pdTokens(m) {
			if !seen[t] {
				seen[t] = true
				counts[t]++
			}
		}
	}
	total := float64(len(msgs))
	freq := make(map[string]float64, len(counts))
	for t, c := range counts {
		freq[t] = float64(c) / total
	}
	return freq
}

// pdTokens extracts persona-signature tokens from a message:
//   - ASCII words (≥3 chars) from meaningfulTokens
//   - Single emoji characters (Symbol_Other category)
//   - Skips raw numbers (they're noisy across both agents)
func pdTokens(s string) []string {
	out := []string{}
	// ASCII words ≥3 (drop "is", "to", "a", etc. as too generic)
	for _, t := range meaningfulTokens(s) {
		if len(t) < 3 {
			continue
		}
		if isAllDigits(t) {
			continue
		}
		// Skip CJK bigrams for PD (they're retrieval-shaped, noisy for persona).
		// meaningfulTokens emits CJK 2-char bigrams; ASCII tokens stay whole.
		// Use containsNonASCII to detect CJK bigram.
		if containsNonASCII(t) && len([]rune(t)) <= 2 {
			continue
		}
		out = append(out, t)
	}
	// Emoji
	for _, r := range s {
		if unicode.Is(unicode.So, r) || unicode.Is(unicode.Sk, r) {
			out = append(out, string(r))
		}
	}
	return out
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// distinctiveTokens computes the per-token frequency difference between
// agent A and agent B, filtering out tokens too rare in both.
func distinctiveTokens(freqA, freqB map[string]float64) []TokenScore {
	allKeys := make(map[string]bool, len(freqA)+len(freqB))
	for k := range freqA {
		allKeys[k] = true
	}
	for k := range freqB {
		allKeys[k] = true
	}
	var scores []TokenScore
	for k := range allKeys {
		fa := freqA[k]
		fb := freqB[k]
		if fa < 0.02 && fb < 0.02 {
			// Token appears in <2% of either agent's messages; too rare.
			continue
		}
		scores = append(scores, TokenScore{
			Token:         k,
			DistinctScore: fa - fb,
			FreqA:         fa, FreqB: fb,
		})
	}
	sort.Slice(scores, func(i, j int) bool {
		return math.Abs(scores[i].DistinctScore) > math.Abs(scores[j].DistinctScore)
	})
	return scores
}

// topKByDirection returns the top-K most distinctive tokens for each agent.
func topKByDirection(scores []TokenScore, k int) (sigA, sigB []TokenScore) {
	for _, s := range scores {
		if s.DistinctScore > 0 && len(sigA) < k {
			sigA = append(sigA, s)
		}
		if s.DistinctScore < 0 && len(sigB) < k {
			sigB = append(sigB, s)
		}
		if len(sigA) >= k && len(sigB) >= k {
			break
		}
	}
	return
}

// buildClassifier returns a function that attributes a message to A or B
// by summing the distinctiveness of its tokens against each signature.
func buildClassifier(sigA, sigB []TokenScore) func(string) string {
	scoreA := make(map[string]float64, len(sigA))
	scoreB := make(map[string]float64, len(sigB))
	for _, t := range sigA {
		scoreA[t.Token] = t.DistinctScore
	}
	for _, t := range sigB {
		scoreB[t.Token] = -t.DistinctScore // make positive for "B-ness"
	}
	return func(msg string) string {
		seen := make(map[string]bool)
		var sumA, sumB float64
		for _, t := range pdTokens(msg) {
			if seen[t] {
				continue
			}
			seen[t] = true
			if s, ok := scoreA[t]; ok {
				sumA += s
			}
			if s, ok := scoreB[t]; ok {
				sumB += s
			}
		}
		if sumA > sumB {
			return "A"
		}
		if sumB > sumA {
			return "B"
		}
		// Tie → predict the agent whose signature contains more matched tokens.
		return "A"
	}
}
