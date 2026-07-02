// Package topic is the L3 thread-state foundation: per-chat topic registry
// + hybrid (keyword + Haiku) classifier. Designed so the bridge can route
// per-turn context into the right topic-thread without spinning separate
// Claude sessions per topic (L3 not L4 — same Claude session, smarter
// Channel B injection).
//
// Architecture:
//
//	user message
//	   ↓
//	HybridClassifier (keyword fast-path → Haiku fallback → cache)
//	   ↓
//	Topic (existing or new)
//	   ↓
//	Registry.Upsert (persists to ghost ns loop:topics)
//	   ↓
//	(future: ThreadState lookup → Channel B injection)
//
// Cycle 65 ships: registry + classifier interface + keyword cascade.
// Cycle 66 wires: bridge integration + real Haiku client.
package topic

import (
	"time"
)

// Topic represents one conversation topic for a specific chat.
// Topics evolve per-chat — one user's "plants" and another user's "plants"
// are independent. Stored in ghost ns "loop:topics" with key shape
// "topic-<chat_id>-<name>".
type Topic struct {
	Name           string    `json:"name"`
	ChatID         int64     `json:"chat_id"`
	Description    string    `json:"description"`              // human-readable; Haiku writes this on new-topic creation
	SignalExamples []string  `json:"signal_examples,omitempty"`// distinctive phrases that mark this topic
	FirstSeen      time.Time `json:"first_seen"`
	LastUsed       time.Time `json:"last_used"`
	TurnCount      int       `json:"turn_count"`
	Status         string    `json:"status"`                   // "active" | "pruned"
}

// ClassificationResult is the output of one classify call.
type ClassificationResult struct {
	Topic       Topic
	IsNew       bool    // true if Haiku proposed a topic not in the registry
	Confidence  float64 // 0..1; keyword=raw_score/len(signals), haiku=its own confidence
	Source      string  // "keyword" | "haiku" | "cache" | "fallback"
	MatchedKWs  []string // for explainability
}

// IsGeneral returns true when no topic was confidently classified.
func (r ClassificationResult) IsGeneral() bool {
	return r.Topic.Name == "general" || r.Topic.Name == ""
}

// Reserved topic name for messages that don't fit any topic.
const TopicGeneral = "general"
