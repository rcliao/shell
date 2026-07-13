package bench

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// OwnerEval (V2-H31) — the owner-fitness scorecard.
//
// Measures the qualities the owners actually correct the agents about, from
// the agent's own ledgers and transcript, fully LLM-free and deterministic.
// Two attribution buckets keep the harness honest:
//
//   - HARNESS dimensions (delivery complaints, latency, dup suppression,
//     internal-text leaks, photo persistence): the shell layer owns these
//     outcomes end-to-end.
//   - AGENT dimensions (factual corrections, brevity, recall grounding,
//     write hygiene): the model/prompt layer owns these; the harness
//     measures and enforces.
//
// REDACTION RULE: results carry NUMBERS ONLY — no message content, no names,
// no quoted text ever leaves this struct into repo-committed artifacts. The
// pattern lists below are generic correction phrases, not conversation data.

// Detector patterns — grounded in the 2026-07-12 transcript mining
// (generic owner-correction phrases; no conversation content). Conservative on purpose: a
// missed correction costs less than a noisy metric.
var (
	factualCorrectionRe = regexp.MustCompile(
		`不對|錯了|記錯|我說的是|我是問|文不對題|你忘記|哪有|明明就?是|又太長|你有看仔細嗎|no,? I meant|that'?s wrong|I told you`)
	deliveryComplaintRe = regexp.MustCompile(
		`被截掉|沒回|沒看到.{0,6}回覆|沒反應|重複兩次|怎麼會重複|卡住|卡 ?Analyzing|回答呢|回答怎麼不見`)
	// Base nudge aliases are the public bot names; deployment-specific
	// nicknames load from a PRIVATE file at runtime (never committed) —
	// see ownerAliasPattern.
	nudgeRe = regexp.MustCompile(
		`(?i)^(pika|umbreon|babies)[?？!！]$`)
	internalLeakRe = regexp.MustCompile(
		`(?i)^this message is directed|i'?ll stay quiet|api error|session limit|issue with the selected model|context deadline exceeded|stale background`)
	photoExpiredRe = regexp.MustCompile(
		`過期.{0,6}讀不到|讀不到了|檔案.{0,4}過期|圖片載入失敗`)
)

// Dimension is one measured quality. Value and Goal share Unit; Better says
// which direction is improvement. Count/Total give the raw fraction so a
// reader can judge sample size (an honest 0/3 beats a fake 100%).
type Dimension struct {
	Name   string  `json:"name"`
	Layer  string  `json:"layer"` // "harness" | "agent"
	Value  float64 `json:"value"`
	Goal   float64 `json:"goal"`
	Unit   string  `json:"unit"`
	Better string  `json:"better"` // "lower" | "higher"
	Count  int64   `json:"count"`
	Total  int64   `json:"total"`
}

// OwnerEvalReport is one agent's scorecard over a window. Numbers only.
type OwnerEvalReport struct {
	Agent      string      `json:"agent"`
	Since      time.Time   `json:"since"`
	Until      time.Time   `json:"until"`
	Dimensions []Dimension `json:"dimensions"`
}

// loadPrivateNudgeRe extends the nudge detector with deployment-specific
// nicknames from ~/.shell/eval-aliases.txt (one alias per line, PRIVATE —
// gitignored territory, never enters this repo). Returns nudgeRe when the
// file is absent.
func loadPrivateNudgeRe() *regexp.Regexp {
	home, err := os.UserHomeDir()
	if err != nil {
		return nudgeRe
	}
	data, err := os.ReadFile(filepath.Join(home, ".shell", "eval-aliases.txt"))
	if err != nil {
		return nudgeRe
	}
	var aliases []string
	for _, line := range strings.Split(string(data), "\n") {
		if a := strings.TrimSpace(line); a != "" {
			aliases = append(aliases, regexp.QuoteMeta(a))
		}
	}
	if len(aliases) == 0 {
		return nudgeRe
	}
	re, err := regexp.Compile(`(?i)^(pika|umbreon|babies|` + strings.Join(aliases, "|") + `)[?？!！]$`)
	if err != nil {
		return nudgeRe
	}
	return re
}

// OwnerEval computes the scorecard from the agent's shell.db.
func OwnerEval(shellDBPath, agentName string, since, until time.Time) (*OwnerEvalReport, error) {
	db, err := sql.Open("sqlite", shellDBPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rep := &OwnerEvalReport{Agent: agentName, Since: since, Until: until}

	// ---- transcript-derived dimensions ----
	type msg struct {
		id      int64
		session int64
		role    string
		content string
	}
	rows, err := db.Query(`
		SELECT id, session_id, role, content FROM messages
		WHERE created_at >= ? AND created_at < ? ORDER BY id`,
		since.UTC(), until.UTC())
	if err != nil {
		return nil, err
	}
	var msgs []msg
	for rows.Next() {
		var m msg
		if err := rows.Scan(&m.id, &m.session, &m.role, &m.content); err != nil {
			rows.Close()
			return nil, err
		}
		msgs = append(msgs, m)
	}
	rows.Close()

	nudgeDetector := loadPrivateNudgeRe()
	var userTurns, factualCorr, deliveryCompl, nudges int64
	var verboseCasual, casualTurns int64
	var leaks, expiredApologies, assistantTurns int64
	lastUserBySession := map[int64]string{}
	for _, m := range msgs {
		switch m.role {
		case "user":
			userTurns++
			if factualCorrectionRe.MatchString(m.content) {
				factualCorr++
			}
			if deliveryComplaintRe.MatchString(m.content) {
				deliveryCompl++
			}
			if nudgeDetector.MatchString(strings.TrimSpace(strings.ToLower(m.content))) {
				nudges++
			}
			lastUserBySession[m.session] = m.content
		case "assistant":
			assistantTurns++
			if internalLeakRe.MatchString(m.content) {
				leaks++
			}
			if photoExpiredRe.MatchString(m.content) {
				expiredApologies++
			}
			if prev, ok := lastUserBySession[m.session]; ok {
				// Brevity contract: casual user turn (<30 runes, not a photo
				// placeholder) answered with a wall (>400 runes).
				if utf8.RuneCountInString(prev) < 30 && !strings.Contains(prev, "(photo)") {
					casualTurns++
					if utf8.RuneCountInString(m.content) > 400 {
						verboseCasual++
					}
				}
			}
		}
	}

	addRate := func(name, layer string, count, total int64, goal float64, better string) {
		var v float64
		if total > 0 {
			v = float64(count) / float64(total)
		}
		rep.Dimensions = append(rep.Dimensions, Dimension{
			Name: name, Layer: layer, Value: v, Goal: goal, Unit: "rate",
			Better: better, Count: count, Total: total,
		})
	}

	addRate("factual_corrections", "agent", factualCorr, userTurns, 0.02, "lower")
	addRate("delivery_complaints", "harness", deliveryCompl, userTurns, 0.0, "lower")
	addRate("nudges_unanswered", "harness", nudges, userTurns, 0.0, "lower")
	addRate("verbose_to_casual", "agent", verboseCasual, casualTurns, 0.08, "lower")
	addRate("internal_text_leaks", "harness", leaks, assistantTurns, 0.0, "lower")
	addRate("photo_expired_apologies", "harness", expiredApologies, assistantTurns, 0.0, "lower")

	// ---- ledger-derived dimensions (tables may predate this build) ----
	countQ := func(q string, args ...any) (int64, bool) {
		var n int64
		if err := db.QueryRow(q, args...).Scan(&n); err != nil {
			return 0, false
		}
		return n, true
	}

	// grounded_recall is the ledger's own verdict; memory_recall/inject_irrelevant
	// rows are the misses (V2-H2 miss path).
	if grounded, ok := countQ(`SELECT COUNT(*) FROM recall_verifications WHERE created_at >= ? AND created_at < ? AND classification = 'grounded_recall'`, since.UTC(), until.UTC()); ok {
		total, _ := countQ(`SELECT COUNT(*) FROM recall_verifications WHERE created_at >= ? AND created_at < ?`, since.UTC(), until.UTC())
		addRate("recall_grounded", "agent", grounded, total, 0.90, "higher")
	}
	if confab, ok := countQ(`SELECT COUNT(*) FROM write_verifications WHERE created_at >= ? AND created_at < ? AND classification = 'verbal_save'`, since.UTC(), until.UTC()); ok {
		claimed, _ := countQ(`SELECT COUNT(*) FROM write_verifications WHERE created_at >= ? AND created_at < ? AND claimed = 1`, since.UTC(), until.UTC())
		addRate("write_confabulation", "agent", confab, claimed, 0.05, "lower")
	}
	if suppressed, ok := countQ(`SELECT COUNT(*) FROM outbound_sends WHERE sent_at >= ? AND sent_at < ? AND suppressed = 1`, since.UTC(), until.UTC()); ok {
		rep.Dimensions = append(rep.Dimensions, Dimension{
			Name: "dup_suppressions", Layer: "harness", Value: float64(suppressed),
			Goal: 0, Unit: "count", Better: "lower", Count: suppressed, Total: 0,
		})
	}
	if described, ok := countQ(`SELECT COUNT(*) FROM media WHERE created_at >= ? AND created_at < ? AND description != ''`, since.UTC(), until.UTC()); ok {
		total, _ := countQ(`SELECT COUNT(*) FROM media WHERE created_at >= ? AND created_at < ?`, since.UTC(), until.UTC())
		addRate("photo_described", "harness", described, total, 0.80, "higher")
	}

	// Latency long tail from the timing columns (interactive turns only).
	var durs []int64
	if drows, err := db.Query(`SELECT duration_ms FROM usage WHERE created_at >= ? AND created_at < ? AND source = 'interactive' AND duration_ms > 0`, since.UTC(), until.UTC()); err == nil {
		for drows.Next() {
			var d int64
			drows.Scan(&d)
			durs = append(durs, d)
		}
		drows.Close()
	}
	if len(durs) > 0 {
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		var over60 int64
		for _, d := range durs {
			if d > 60_000 {
				over60++
			}
		}
		p95 := durs[len(durs)*95/100]
		rep.Dimensions = append(rep.Dimensions, Dimension{
			Name: "latency_p95_seconds", Layer: "harness", Value: float64(p95) / 1000,
			Goal: 60, Unit: "seconds", Better: "lower", Count: over60, Total: int64(len(durs)),
		})
		addRate("turns_over_60s", "harness", over60, int64(len(durs)), 0.05, "lower")
	}

	return rep, nil
}

// FormatOwnerEval renders the human table (numbers only — redaction-safe).
func FormatOwnerEval(r *OwnerEvalReport) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Owner-eval scorecard — %s (%s → %s)\n\n", r.Agent,
		r.Since.Format("2006-01-02"), r.Until.Format("2006-01-02"))
	fmt.Fprintf(&sb, "  %-26s %-8s %10s %8s %6s  %s\n", "DIMENSION", "LAYER", "VALUE", "GOAL", "DIR", "N")
	for _, d := range r.Dimensions {
		val := fmt.Sprintf("%.1f%%", d.Value*100)
		goal := fmt.Sprintf("%.0f%%", d.Goal*100)
		if d.Unit != "rate" {
			val = fmt.Sprintf("%.1f", d.Value)
			goal = fmt.Sprintf("%.0f", d.Goal)
		}
		n := fmt.Sprintf("%d/%d", d.Count, d.Total)
		if d.Total == 0 {
			n = fmt.Sprintf("%d", d.Count)
		}
		fmt.Fprintf(&sb, "  %-26s %-8s %10s %8s %6s  %s\n", d.Name, d.Layer, val, goal, d.Better, n)
	}
	return sb.String()
}

// OwnerEvalJSON renders the redaction-safe JSON snapshot.
func OwnerEvalJSON(r *OwnerEvalReport) ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}
