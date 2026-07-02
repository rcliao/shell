package bench

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// WriteHygiene scores whether memo-style user messages from owner chats
// resulted in an actual ghost memory row in the agent's expected namespace.
//
// Methodology (LLM-free, post-hoc):
//   1. Pull user messages from owner chats in [since, until) whose content
//      matches a memo trigger (Chinese/English memo cues).
//   2. For each trigger, look for memories created in the agent's memory.db
//      within ±matchWindow of the message time.
//   3. Classify: verified (row in expected NS) / wrong-ns (row exists in some
//      other NS — shadow-ns bug) / missing (no row at all — verbal-save bug).
//
// Score = verified / claimed_triggers.  Goal-state = 1.0.
//
// Window default = 30min (was 5min in cycle 47-prior). Cycle 48 diagnostic
// showed that of the 7 "misses" at 5min, 3 were late landings (38-114 min)
// from current pikamini behavior, while 4 were true never-lands all from
// pre-cycle-36 (before receipt-first protocol shipped). Widening to 30min
// captures "agent persisted at all within reasonable batch window" — which
// is what the owner's complaint actually measured.
func WriteHygiene(t AgentTarget, since, until time.Time) (*WHReport, error) {
	const matchWindow = 2 * time.Hour

	shellDB, err := sql.Open("sqlite", t.ShellDB+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open shell.db: %w", err)
	}
	defer shellDB.Close()

	memDB, err := sql.Open("sqlite", t.MemoryDB+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open memory.db: %w", err)
	}
	defer memDB.Close()

	rep := &WHReport{WindowStart: since, WindowEnd: until}

	chatList := joinInts(t.OwnerChats)
	if chatList == "" {
		chatList = "0" // never match
	}
	q := fmt.Sprintf(`
		SELECT m.id, s.chat_id, m.content,
		       strftime('%%Y-%%m-%%dT%%H:%%M:%%SZ', m.created_at) AS ts
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE m.role = 'user'
		  AND s.chat_id IN (%s)
		  AND m.created_at >= ?
		  AND m.created_at < ?
		ORDER BY m.id ASC`, chatList)
	rows, err := shellDB.Query(q, since.UTC().Format("2006-01-02 15:04:05"), until.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id      int64
			chatID  int64
			content string
			ts      string
		)
		if err := rows.Scan(&id, &chatID, &content, &ts); err != nil {
			return nil, err
		}
		if !isMemoTrigger(content) {
			continue
		}
		when, err := time.Parse("2006-01-02T15:04:05Z", ts)
		if err != nil {
			continue
		}
		rep.ClaimedWrites++

		verdict, sampleNS := classifyMemo(memDB, t.Namespace, when, matchWindow)
		switch verdict {
		case "verified":
			rep.VerifiedWrites++
		case "wrong_ns":
			rep.WrongNamespace++
			rep.Samples = append(rep.Samples, WHViolation{
				MessageID: id, ChatID: chatID, When: when,
				Reason:  "row exists in " + sampleNS,
				Excerpt: truncate(content, 80),
			})
		case "missing":
			rep.MissingRow++
			rep.Samples = append(rep.Samples, WHViolation{
				MessageID: id, ChatID: chatID, When: when,
				Reason:  "no memory row within ±" + matchWindow.String(),
				Excerpt: truncate(content, 80),
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if rep.ClaimedWrites > 0 {
		rep.Score = float64(rep.VerifiedWrites) / float64(rep.ClaimedWrites)
	}
	return rep, nil
}

// isMemoTrigger detects user-facing memo/recall request patterns.
// Tuned for the owner's actual phrasing — extend as we observe new ones.
//
// Uses word-boundary matching on ASCII triggers so "memo" doesn't match
// "memory" / "memoir" / "memorable" (which appear in the daily AI
// research briefings). CJK triggers don't need boundaries; they're already
// distinctive enough.
var memoTriggerRe = regexp.MustCompile(`(?i)(\b|[^\p{L}\p{N}])(memo|remember|save this)([^\p{L}\p{N}]|\b)`)

func isMemoTrigger(s string) bool {
	if memoTriggerRe.MatchString(s) {
		return true
	}
	// CJK fragments — direct substring is fine.
	for _, k := range []string{"記下", "記錄"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// classifyMemo decides if a memo trigger at time `t` was honored.
// Requires a memo-shaped key (meal-memo-* / memo-* / *-memo) — heartbeat
// or unrelated writes in the window don't count.
// Returns one of: "verified", "wrong_ns", "missing".
func classifyMemo(memDB *sql.DB, expectedNS string, t time.Time, window time.Duration) (string, string) {
	lo := t.Add(-window).UTC().Format("2006-01-02T15:04:05Z")
	hi := t.Add(window).UTC().Format("2006-01-02T15:04:05Z")

	const keyFilter = `(key LIKE 'meal-memo-%' OR key LIKE 'memo-%' OR key LIKE '%-memo-%' OR key LIKE 'snack-memo-%')`

	// Include soft-deleted versions: we want to know if a memo was EVER
	// written near the user's trigger, even if a later edit superseded it.
	var ns string
	err := memDB.QueryRow(`
		SELECT ns FROM memories
		WHERE created_at >= ? AND created_at <= ?
		  AND ns = ?
		  AND `+keyFilter+`
		LIMIT 1`, lo, hi, expectedNS).Scan(&ns)
	if err == nil {
		return "verified", ns
	}
	if err != sql.ErrNoRows {
		return "missing", ""
	}

	// Check any other namespace — shadow-ns smell.
	err = memDB.QueryRow(`
		SELECT ns FROM memories
		WHERE created_at >= ? AND created_at <= ?
		  AND ns != ?
		  AND `+keyFilter+`
		LIMIT 1`, lo, hi, expectedNS).Scan(&ns)
	if err == nil {
		return "wrong_ns", ns
	}
	return "missing", ""
}

func joinInts(xs []int64) string {
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		parts = append(parts, fmt.Sprintf("%d", x))
	}
	return strings.Join(parts, ",")
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
