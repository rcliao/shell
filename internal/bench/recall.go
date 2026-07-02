package bench

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	memory "github.com/rcliao/ghost"
	"gopkg.in/yaml.v3"
)

// RFCase is a single recall test case loaded from yaml.
type RFCase struct {
	ID       string   `yaml:"id"`
	Question string   `yaml:"question"`
	// Gold is the canonical answer for token_recall / flexible_contains scoring.
	Gold string `yaml:"gold"`
	// EvidenceMsgIDs are shell.db message IDs that should be retrievable
	// (used for recall@K when the memory's meta links back to a source msg).
	EvidenceMsgIDs []int64 `yaml:"evidence_msg_ids,omitempty"`
	// Tags lets the test set be filtered by category (test selection).
	Tags []string `yaml:"tags,omitempty"`
	// SearchTags are passed to ghost.Search as a memory filter. Models the
	// "agent infers intent and adds a tag filter" behavior we want.
	// Empty = no filter (baseline; what the agent does today).
	SearchTags []string `yaml:"search_tags,omitempty"`
}

// LoadRFCases loads all *.yml files under dir as RFCases.
func LoadRFCases(dir string) ([]RFCase, error) {
	var cases []RFCase
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if filepath.Ext(path) != ".yml" && filepath.Ext(path) != ".yaml" {
			return nil
		}
		buf, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var c RFCase
		if err := yaml.Unmarshal(buf, &c); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if c.ID == "" {
			c.ID = filepath.Base(path)
		}
		cases = append(cases, c)
		return nil
	})
	return cases, err
}

// RecallFidelity scores the agent's ghost memory.db for answer presence.
// For each case, it queries via ghost's actual Search pipeline (FTS5 →
// vector fallback → LIKE), concatenates the top-K contents, and applies
// LLM-free scorers (contains / flexible_contains / token_recall) against
// the gold answer.
//
// Using ghost.Store.Search() rather than direct SQL means we measure what
// the agent actually experiences when retrieving — not a toy approximation.
func RecallFidelity(t AgentTarget, cases []RFCase, topK int) (*RFReport, error) {
	if topK <= 0 {
		topK = 5
	}
	store, err := memory.NewSQLiteStore(t.MemoryDB)
	if err != nil {
		return nil, fmt.Errorf("open ghost store: %w", err)
	}

	rep := &RFReport{
		Metrics: map[string]float64{
			"contains":          0,
			"flexible_contains": 0,
			"token_recall":      0,
		},
	}

	ctx := context.Background()
	for _, c := range cases {
		retrieved, snippet := ghostSearchTagged(ctx, store, t.Namespace, c.Question, c.SearchTags, topK)

		corpus := snippet
		caseMetrics := map[string]float64{
			"contains":          Contains(corpus, c.Gold),
			"flexible_contains": FlexibleContains(corpus, c.Gold),
			"token_recall":      TokenRecall(corpus, c.Gold),
		}

		for k, v := range caseMetrics {
			rep.Metrics[k] += v
		}
		rep.PerCase = append(rep.PerCase, RFCaseResult{
			CaseID:      c.ID,
			Question:    c.Question,
			Retrieved:   retrieved,
			Metrics:     caseMetrics,
			BestSnippet: truncate(snippet, 200),
		})
	}

	rep.Cases = len(cases)
	if rep.Cases > 0 {
		for k := range rep.Metrics {
			rep.Metrics[k] /= float64(rep.Cases)
		}
	}
	return rep, nil
}

// ghostSearch is the canonical retrieval path: ghost.Store.Search() with no
// tag filter (mirrors what the agent does today with a naive query).
func ghostSearch(ctx context.Context, store memory.Store, ns, query string, k int) ([]string, string) {
	return ghostSearchTagged(ctx, store, ns, query, nil, k)
}

// ghostSearchTagged adds a tag filter to the search. Used by RF cases that
// model "the agent inferred intent and passed Tags". Set tags=nil to disable.
//
// Additionally performs date-aware re-ranking: if the question contains a
// date pattern (M月D[日號], M/D, or YYYY-MM-DD), memory rows whose keys
// contain the matching ISO date are floated to the top of the result set.
// Models "agent uses temporal hint to disambiguate between same-tag memories".
func ghostSearchTagged(ctx context.Context, store memory.Store, ns, query string, tags []string, k int) ([]string, string) {
	// Over-fetch only when date-aware re-rank can use the extra rows.
	// For non-date queries, the original ranker order is what we measure.
	dates := extractISODatesFromQuery(query)
	limit := k
	if len(dates) > 0 {
		limit = k * 4
	}
	results, err := store.Search(ctx, memory.SearchParams{
		NS:         ns,
		Query:      query,
		Tags:       tags,
		Limit:      limit,
		IncludeAll: true,
	})
	if err != nil {
		return nil, ""
	}

	if len(dates) > 0 {
		dateSet := make(map[string]bool, len(dates))
		for _, d := range dates {
			dateSet[d] = true
		}
		sort.SliceStable(results, func(i, j int) bool {
			ai, aj := keyMatchesAnyDate(results[i].Memory.Key, dateSet), keyMatchesAnyDate(results[j].Memory.Key, dateSet)
			if ai != aj {
				return ai && !aj
			}
			return false
		})
	}

	if len(results) > k {
		results = results[:k]
	}

	var keys []string
	var corpus strings.Builder
	for _, r := range results {
		keys = append(keys, r.Memory.Key)
		corpus.WriteString(r.Memory.Content)
		corpus.WriteString("\n")
	}
	return keys, corpus.String()
}

var (
	// Year is optional — questions usually omit it. We assume current year
	// at runtime when no year is given. Tested forms: 5月11號, 5月11日, 5/11,
	// 2026-05-11, 2026/5/11, May 11.
	dateReChineseMonthDay = regexp.MustCompile(`(\d{1,2})月(\d{1,2})[日號号]?`)
	dateReSlash           = regexp.MustCompile(`(?:^|\D)(\d{1,2})/(\d{1,2})(?:\D|$)`)
	dateReISO             = regexp.MustCompile(`(20\d\d)-(\d{1,2})-(\d{1,2})`)
	monthNames            = map[string]int{
		"jan": 1, "january": 1, "feb": 2, "february": 2, "mar": 3, "march": 3,
		"apr": 4, "april": 4, "may": 5, "jun": 6, "june": 6, "jul": 7, "july": 7,
		"aug": 8, "august": 8, "sep": 9, "sept": 9, "september": 9,
		"oct": 10, "october": 10, "nov": 11, "november": 11, "dec": 12, "december": 12,
	}
	dateReEnglish = regexp.MustCompile(`(?i)(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\s+(\d{1,2})`)
)

// extractISODatesFromQuery scans a natural-language question for date forms
// and emits canonical ISO dates (YYYY-MM-DD). When no year is present, uses
// 2026 as default. Returns nil when no date pattern is found.
func extractISODatesFromQuery(q string) []string {
	const defaultYear = "2026"
	seen := map[string]bool{}
	out := []string{}
	add := func(y string, m, d int) {
		if y == "" {
			y = defaultYear
		}
		s := fmt.Sprintf("%s-%02d-%02d", y, m, d)
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, m := range dateReISO.FindAllStringSubmatch(q, -1) {
		var mo, d int
		fmt.Sscanf(m[2], "%d", &mo)
		fmt.Sscanf(m[3], "%d", &d)
		add(m[1], mo, d)
	}
	for _, m := range dateReChineseMonthDay.FindAllStringSubmatch(q, -1) {
		var mo, d int
		fmt.Sscanf(m[1], "%d", &mo)
		fmt.Sscanf(m[2], "%d", &d)
		add("", mo, d)
	}
	for _, m := range dateReSlash.FindAllStringSubmatch(q, -1) {
		var mo, d int
		fmt.Sscanf(m[1], "%d", &mo)
		fmt.Sscanf(m[2], "%d", &d)
		add("", mo, d)
	}
	for _, m := range dateReEnglish.FindAllStringSubmatch(q, -1) {
		mname := strings.ToLower(m[1])
		mo, ok := monthNames[mname]
		if !ok {
			continue
		}
		var d int
		fmt.Sscanf(m[2], "%d", &d)
		add("", mo, d)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func keyMatchesAnyDate(key string, dates map[string]bool) bool {
	for d := range dates {
		if strings.Contains(key, d) {
			return true
		}
	}
	return false
}

// fts5Search uses ghost's chunks_fts table to retrieve memory keys + content
// snippets for a question. Returns (keys, concatenated snippet, err).
//
// Deprecated: prefer ghostSearch — kept for use by the CV runner against
// sandbox DBs where we already have a *sql.DB handle open.
func fts5Search(db *sql.DB, ns, query string, k int) ([]string, string, error) {
	rows, err := db.Query(`
		SELECT m.key, m.content
		FROM chunks_fts f
		JOIN chunks c ON c.rowid = f.rowid
		JOIN memories m ON m.id = c.memory_id
		WHERE m.ns = ? AND m.deleted_at IS NULL
		  AND chunks_fts MATCH ?
		ORDER BY rank
		LIMIT ?`, ns, ftsEscape(query), k)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	return readKeyContent(rows)
}

func likeSearch(db *sql.DB, ns, query string, k int) ([]string, string, error) {
	rows, err := db.Query(`
		SELECT key, content FROM memories
		WHERE ns = ? AND deleted_at IS NULL AND content LIKE ?
		ORDER BY created_at DESC LIMIT ?`,
		ns, "%"+query+"%", k)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	return readKeyContent(rows)
}

func readKeyContent(rows *sql.Rows) ([]string, string, error) {
	var keys []string
	var corpus string
	for rows.Next() {
		var key, content string
		if err := rows.Scan(&key, &content); err != nil {
			return nil, "", err
		}
		keys = append(keys, key)
		corpus += content + "\n"
	}
	return keys, corpus, rows.Err()
}

// ftsEscape wraps each meaningful token in quotes joined by OR so FTS5
// returns rows matching any token. AND (default) is too strict for CJK
// questions where the whole CJK phrase tokenizes as one block but the
// memory uses individual chars.
func ftsEscape(q string) string {
	tokens := meaningfulTokens(q)
	if len(tokens) == 0 {
		return ""
	}
	parts := make([]string, 0, len(tokens))
	for _, t := range tokens {
		parts = append(parts, `"`+t+`"`)
	}
	return strings.Join(parts, " OR ")
}
