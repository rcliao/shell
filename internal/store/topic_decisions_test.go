package store

import (
	"testing"
	"time"
)

func TestTopicDecisionLogging(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	// Log a few decisions across sources.
	rows := []TopicDecision{
		{ChatID: 1, Topic: "plants", Source: "keyword", Confidence: 3.0, LatencyMs: 1},
		{ChatID: 1, Topic: "plants", Source: "haiku", Confidence: 0.9, LatencyMs: 250, CommitmentsExtracted: 1},
		{ChatID: 1, Topic: "meals", Source: "keyword", Confidence: 2.0, LatencyMs: 1},
		{ChatID: 1, Topic: "fortune", Source: "haiku", Confidence: 0.8, LatencyMs: 180, IsNew: true},
		{ChatID: 1, Topic: "plants", Source: "cache", Confidence: 0.0, LatencyMs: 0},
	}
	for _, r := range rows {
		if err := s.LogTopicDecision(r); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := s.AggregateTopicStats(1, time.Now().Add(-1*time.Hour), time.Now().Add(1*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalDecisions != 5 {
		t.Errorf("expected 5 decisions, got %d", stats.TotalDecisions)
	}
	if stats.BySource["keyword"] != 2 {
		t.Errorf("expected 2 keyword, got %d", stats.BySource["keyword"])
	}
	if stats.BySource["haiku"] != 2 {
		t.Errorf("expected 2 haiku, got %d", stats.BySource["haiku"])
	}
	if stats.ByTopic["plants"] != 3 {
		t.Errorf("expected 3 plants, got %d", stats.ByTopic["plants"])
	}
	if stats.NewTopics != 1 {
		t.Errorf("expected 1 new topic, got %d", stats.NewTopics)
	}
	if stats.CommitmentsExtracted != 1 {
		t.Errorf("expected 1 commitment, got %d", stats.CommitmentsExtracted)
	}
	// p50 of [180,250] = 180; p95 ~ 250
	if stats.HaikuLatencyP50 == 0 || stats.HaikuLatencyP95 == 0 {
		t.Errorf("haiku latency percentiles should be populated: p50=%d p95=%d",
			stats.HaikuLatencyP50, stats.HaikuLatencyP95)
	}
}

func TestThreadHealth(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	// Active thread
	s.BumpTopicThread(99, "plants", 1)
	// Add open + overdue commitment
	s.AddCommitment(99, "plants", Commitment{
		Action: "check leaves",
		DueAt:  time.Now().Add(-24 * time.Hour), // overdue
		Status: "open",
	})
	// Add open + not-yet-due commitment
	s.AddCommitment(99, "plants", Commitment{
		Action: "repot if needed",
		DueAt:  time.Now().Add(24 * time.Hour),
		Status: "open",
	})

	// Empty-summary thread
	s.BumpTopicThread(99, "meals", 2)

	// Thread with summary
	s.BumpTopicThread(99, "fortune", 3)
	s.SetThreadSummary(99, "fortune", "saturday energy: quiet, observant")

	h, err := s.ComputeThreadHealth(99, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if h.ActiveThreads != 3 {
		t.Errorf("expected 3 active threads, got %d", h.ActiveThreads)
	}
	if h.TotalOpenCommits != 2 {
		t.Errorf("expected 2 open commitments, got %d", h.TotalOpenCommits)
	}
	if h.OverdueCommits != 1 {
		t.Errorf("expected 1 overdue commitment, got %d", h.OverdueCommits)
	}
	if h.ThreadsWithNoSummary != 2 {
		t.Errorf("expected 2 threads with no summary (plants + meals), got %d", h.ThreadsWithNoSummary)
	}
	if h.AvgSummaryLen == 0 {
		t.Errorf("expected non-zero avg summary len (fortune has one)")
	}
}
