package topic

import (
	"context"
	"strings"
)

// HaikuClient classifies a message using a small LLM (e.g. Claude Haiku),
// optionally proposing new topics not in the existing registry.
type HaikuClient interface {
	ClassifyTopic(ctx context.Context, msg string, existing []Topic) (HaikuResult, error)
}

// HaikuResult is the structured JSON the small-LLM returns.
type HaikuResult struct {
	Topic       string  `json:"topic"`
	IsNew       bool    `json:"is_new"`
	Description string  `json:"description,omitempty"`
	Confidence  float64 `json:"confidence"`
	Rationale   string  `json:"rationale,omitempty"`
}

// StubHaiku is a deterministic test HaikuClient that lets cycle 65 ship
// without a live LLM dependency. It uses a simple rule: if any existing
// topic name appears as a substring of the message (case-insensitive),
// return that topic with confidence 0.9. Otherwise propose new topic
// "discussion" with confidence 0.5.
//
// Cycle 66 replaces this with a real Claude-CLI-subprocess client.
type StubHaiku struct{}

func (StubHaiku) ClassifyTopic(ctx context.Context, msg string, existing []Topic) (HaikuResult, error) {
	low := strings.ToLower(msg)
	for _, t := range existing {
		if t.Name == "" {
			continue
		}
		if strings.Contains(low, strings.ToLower(t.Name)) {
			return HaikuResult{
				Topic:      t.Name,
				IsNew:      false,
				Confidence: 0.9,
				Rationale:  "stub: existing topic name appears in message",
			}, nil
		}
	}
	return HaikuResult{
		Topic:       "discussion",
		IsNew:       true,
		Description: "general discussion (stub)",
		Confidence:  0.5,
		Rationale:   "stub: no existing topic matched",
	}, nil
}

// PromptTemplate is the prompt-shape cycle 66 will send to Haiku. Documented
// here so the interface contract is explicit before integration.
//
// Inputs: msg (string), existing []Topic (registry list).
// Output: JSON { topic, is_new, description?, confidence, rationale? }
//
// System message:
//
//	You are a topic classifier for a family-assistant agent. Given a user
//	message and a list of existing conversation topics this user has been
//	discussing, decide which topic the message belongs to.
//
//	If one of the existing topics fits, return its exact name and is_new=false.
//	If the message starts a substantively new topic (one that the existing
//	topics don't cover), return is_new=true with a fresh name + 1-line
//	description.
//
//	Stay conservative on new topics — only create one if existing topics
//	clearly don't fit. Reuse existing names whenever reasonable.
//
//	Output ONLY JSON, no other text. Schema:
//	  { "topic": string, "is_new": bool, "description": string (only if is_new),
//	    "confidence": number 0..1, "rationale": string (optional) }
//
// User message:
//
//	Existing topics for this user:
//	{topic1}: {description1}
//	{topic2}: {description2}
//	...
//
//	New user message:
//	{msg}
//
//	Return JSON only.
const PromptTemplate = `You are a topic classifier for a family-assistant agent.
Given a user message and a list of existing conversation topics this user has been
discussing, decide which topic the message belongs to.

STRONG DEFAULT: REUSE an existing topic. A new topic is the exception, not the rule.

Reuse rule (apply liberally — production data shows over-creation is the dominant
failure mode):
- If ANY existing topic could plausibly contain this message — even a different
  facet, sub-question, or follow-up — REUSE its exact name with is_new=false.
- "Mattress encasement vs protector breathability" and "Mattress care" are the
  SAME topic, not two. Pick the broader existing name.
- A question about a previously-discussed item (plant, supplement, product,
  person) belongs to that item's existing topic, even if the angle is new.

Only mark is_new=true when the message is in a clearly different domain than
ALL existing topics — e.g., switching from mattress shopping to a meal log.
Naming variation alone (e.g., "Plant Watering Schedule" vs "Plant Watering and
Root Health") is NOT a reason to create a new topic; pick the existing one.

If the registry is empty (first topic), is_new=true is correct.

Output ONLY JSON, no other text. Schema:
  { "topic": string, "is_new": bool, "description": string (only if is_new),
    "confidence": number 0..1, "rationale": string (optional) }

Existing topics for this user:
%s

New user message:
%s

Return JSON only.`
