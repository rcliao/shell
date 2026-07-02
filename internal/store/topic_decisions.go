package store

import (
	"time"
)

// TopicDecision is one per-turn classifier observation. Cycle 69's
// feedback foundation — every classify call writes one row so future
// cycles can analyze classification quality, cost, and drift.
type TopicDecision struct {
	ID                   int64
	ChatID               int64
	MsgID                int64
	Topic                string
	Source               string // keyword | haiku | cache | fallback | error | disabled
	Confidence           float64
	LatencyMs            int64
	IsNew                bool
	CommitmentsExtracted int
	SummaryChanged       bool
	CreatedAt            time.Time
}

// LogTopicDecision persists one observation. Best-effort — caller swallows
// errors so a logging failure never disturbs the bridge's hot path.
func (s *Store) LogTopicDecision(d TopicDecision) error {
	_, err := s.db.Exec(`
		INSERT INTO topic_decisions
		  (chat_id, msg_id, topic, source, confidence, latency_ms,
		   is_new, commitments_extracted, summary_changed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ChatID, d.MsgID, d.Topic, d.Source, d.Confidence, d.LatencyMs,
		boolInt(d.IsNew), d.CommitmentsExtracted, boolInt(d.SummaryChanged))
	return err
}

// TopicStats is aggregate per-source / per-topic data for a time window.
type TopicStats struct {
	TotalDecisions int
	BySource       map[string]int                 // source → count
	ByTopic        map[string]int                 // topic → count
	NewTopics      int                            // is_new = true count
	CommitmentsExtracted int                      // sum
	HaikuLatencyP50 int64                         // median ms for source=haiku
	HaikuLatencyP95 int64
	WindowStart    time.Time
	WindowEnd      time.Time
}

// AggregateTopicStats reads decisions for a chat (or all if chatID=0)
// within [since, until] and returns aggregates.
func (s *Store) AggregateTopicStats(chatID int64, since, until time.Time) (*TopicStats, error) {
	stats := &TopicStats{
		BySource:    map[string]int{},
		ByTopic:     map[string]int{},
		WindowStart: since,
		WindowEnd:   until,
	}

	// SQLite stores CURRENT_TIMESTAMP as "YYYY-MM-DD HH:MM:SS" UTC text.
	// Go time.Time params get serialized differently by some drivers, so
	// compare against UTC-formatted strings for portability.
	chatClause := ""
	args := []any{since.UTC().Format("2006-01-02 15:04:05"), until.UTC().Format("2006-01-02 15:04:05")}
	if chatID != 0 {
		chatClause = "AND chat_id = ?"
		args = append(args, chatID)
	}

	rows, err := s.db.Query(`
		SELECT topic, source, is_new, commitments_extracted, latency_ms
		FROM topic_decisions
		WHERE created_at >= ? AND created_at <= ? `+chatClause+`
		ORDER BY created_at ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var haikuLatencies []int64
	for rows.Next() {
		var topic, source string
		var isNew, commits int
		var latency int64
		if err := rows.Scan(&topic, &source, &isNew, &commits, &latency); err != nil {
			return nil, err
		}
		stats.TotalDecisions++
		stats.BySource[source]++
		stats.ByTopic[topic]++
		if isNew == 1 {
			stats.NewTopics++
		}
		stats.CommitmentsExtracted += commits
		if source == "haiku" && latency > 0 {
			haikuLatencies = append(haikuLatencies, latency)
		}
	}
	stats.HaikuLatencyP50 = percentile(haikuLatencies, 0.50)
	stats.HaikuLatencyP95 = percentile(haikuLatencies, 0.95)
	return stats, rows.Err()
}

// ThreadHealth is a per-chat snapshot of thread-state quality.
type ThreadHealth struct {
	ChatID            int64
	ActiveThreads     int
	StaleThreads      int // last_turn_at > 30d
	TotalOpenCommits  int
	OverdueCommits    int
	AvgSummaryLen     int
	ThreadsWithNoSummary int
}

func (s *Store) ComputeThreadHealth(chatID int64, staleThreshold time.Duration) (*ThreadHealth, error) {
	threads, err := s.ListTopicThreads(chatID)
	if err != nil {
		return nil, err
	}
	h := &ThreadHealth{ChatID: chatID}
	staleCutoff := time.Now().Add(-staleThreshold)
	totalSummaryLen := 0
	for _, t := range threads {
		h.ActiveThreads++
		if t.LastTurnAt != nil && t.LastTurnAt.Before(staleCutoff) {
			h.StaleThreads++
		}
		if t.Summary == "" {
			h.ThreadsWithNoSummary++
		} else {
			totalSummaryLen += len(t.Summary)
		}
		for _, c := range t.OpenCommitments {
			if c.Status != "open" {
				continue
			}
			h.TotalOpenCommits++
			if c.IsOverdue() {
				h.OverdueCommits++
			}
		}
	}
	withSummary := h.ActiveThreads - h.ThreadsWithNoSummary
	if withSummary > 0 {
		h.AvgSummaryLen = totalSummaryLen / withSummary
	}
	return h, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// percentile picks the value at percentile p (0..1) from xs.
// Returns 0 when empty. Uses naive sort + index — fine for typical N<1000.
func percentile(xs []int64, p float64) int64 {
	if len(xs) == 0 {
		return 0
	}
	// in-place sort (mutates caller's slice, but xs is local to caller path)
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
	idx := int(float64(len(xs)-1) * p)
	return xs[idx]
}
