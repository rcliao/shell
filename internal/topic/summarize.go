package topic

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ── Auto-summarizer + commitment extractor (cycle 68) ────────────────────
//
// LLM-free post-response analysis. Designed to be cheap, deterministic,
// and easy to audit. Cycle 68+ may layer Haiku-assisted enrichment on top
// for harder cases, but the regex path catches the high-signal patterns.

// ExtractedCommitment is one commitment parsed from an agent response.
// Same shape as store.Commitment but defined here to avoid a cross-package
// dependency cycle; the bridge translates.
type ExtractedCommitment struct {
	Action string
	DueAt  time.Time
	Source string // brief excerpt of the phrase that matched
}

// Commitment-style phrases to extract.
//   "I'll check leaves in 5 days"        → due in 5 days
//   "revisit by 5/14"                    → due 5/14 (year inferred = current)
//   "follow up on 2026-06-01"             → due 2026-06-01
//   "remind me to repot in a week"        → due in 7 days
//   "we'll look at roots next week"       → due in 7 days
//   "check back tomorrow"                 → due in 1 day
//
// Limitations (documented):
//   - English only; Chinese phrases caught only by literal keywords
//   - Doesn't handle conditional commitments ("if X then Y")
//   - Greedy on phrase length — extracts up to 80 chars after the trigger
//
// Future: cycle 69+ can add Haiku-assisted parsing for low-coverage cases.

var (
	// English anchors: phrases that signal "this is a commitment to do X later"
	commitTriggerRe = regexp.MustCompile(`(?i)\b(I'll|I will|let's|let me|we'll|we will|we should|we can|I can|going to|will|need to|should|remind me to|set a reminder to|followup|follow up|follow-up|revisit|circle back|check back|check in|check on)\b`)

	// Cycle 81: Chinese commitment phrases — based on real production data
	// (181 decisions, 0 extractions across English-only patterns). Real
	// agent responses use bilingual style with CN commitment cues that
	// the original regex missed entirely.
	commitTriggerCNRe = regexp.MustCompile(`(明天.{0,8}(注意|看|觀察|繼續|追蹤|檢查|記得)|今晚.{0,8}注意|稍後|過幾天|改天|下次.{0,4}(幫|看|提醒)|提醒.{0,5}(妳|你)?.{0,5}(明天|稍後|下次|過|的時候)|持續(觀察|追蹤|監控)|繼續觀察|守著(妳|你)|我會.{0,15}(的時候|時|提醒)|追蹤中|過敏觀察(持續中)?|觀察期)`)

	// Date-bound phrases (English)
	dueDateRe = regexp.MustCompile(`(?i)\bby\s+(\d{1,2}/\d{1,2}(?:/\d{2,4})?|\d{4}-\d{2}-\d{2}|tomorrow|next\s+(?:week|month|monday|tuesday|wednesday|thursday|friday|saturday|sunday))\b`)
	dueInRe   = regexp.MustCompile(`(?i)\bin\s+(\d+)\s*(day|week|month|hour|minute)s?\b`)
	dueOnRe   = regexp.MustCompile(`(?i)\bon\s+(\d{1,2}/\d{1,2}(?:/\d{2,4})?|\d{4}-\d{2}-\d{2})\b`)
	// Bare temporal references (no preposition): "tomorrow", "next week", etc.
	dueBareRe = regexp.MustCompile(`(?i)\b(tomorrow|next\s+(?:week|month|monday|tuesday|wednesday|thursday|friday|saturday|sunday))\b`)

	// Cycle 81: Chinese date / time-bound markers.
	dueDateCNRe = regexp.MustCompile(`(明天|今晚|後天|大後天|這週末|下週|過幾天|(\d{1,2})月(\d{1,2})[日號]|(\d{1,2})/(\d{1,2})|週[一二三四五六日天])`)

	// Dedupe window: if two triggers fire within this many chars, treat as
	// one commitment (the second is usually a sub-pattern of the first).
	triggerDedupeWindow = 40
)

// ExtractCommitments scans agent response text for commitment-style phrases
// and returns one ExtractedCommitment per match. Empty slice when nothing
// found — that's the common case for non-action responses.
func ExtractCommitments(response string) []ExtractedCommitment {
	if response == "" {
		return nil
	}
	var out []ExtractedCommitment
	now := time.Now()

	// Find commitment trigger positions; for each, scan a short window
	// around it for date markers + action description. Dedupe by
	// post-windowing: triggers whose processing windows end at the same
	// sentence boundary are one commitment (e.g. "let's revisit by X"
	// matches both "let's" and "revisit" but ends at the same period).
	// Cycle 81: union of English + Chinese triggers.
	allTriggers := append(
		commitTriggerRe.FindAllStringIndex(response, -1),
		commitTriggerCNRe.FindAllStringIndex(response, -1)...,
	)
	seenWindowEnds := make(map[int]bool)
	triggers := make([][]int, 0, len(allTriggers))
	for _, idx := range allTriggers {
		// Compute the would-be window-end for dedup check.
		probeEnd := idx[0] + 200
		if probeEnd > len(response) {
			probeEnd = len(response)
		}
		if dot := strings.IndexAny(response[idx[1]:probeEnd], ".!?\n"); dot >= 0 {
			probeEnd = idx[1] + dot
		}
		if seenWindowEnds[probeEnd] {
			continue
		}
		seenWindowEnds[probeEnd] = true
		triggers = append(triggers, idx)
	}
	for _, idx := range triggers {
		start := idx[0]
		// Window: from trigger to end-of-sentence or 120 chars, whichever earlier.
		end := start + 200
		if end > len(response) {
			end = len(response)
		}
		// Stop at first sentence-end after the trigger.
		if dot := strings.IndexAny(response[idx[1]:end], ".!?\n"); dot >= 0 {
			end = idx[1] + dot
		}
		window := response[start:end]

		var due time.Time
		// Try due-in (relative)
		if m := dueInRe.FindStringSubmatch(window); m != nil {
			due = parseRelativeDuration(m[1], m[2], now)
		}
		// Try due-by (absolute)
		if due.IsZero() {
			if m := dueDateRe.FindStringSubmatch(window); m != nil {
				due = parseAbsoluteDate(m[1], now)
			}
		}
		// Try due-on (absolute)
		if due.IsZero() {
			if m := dueOnRe.FindStringSubmatch(window); m != nil {
				due = parseAbsoluteDate(m[1], now)
			}
		}
		// Try bare temporal ("tomorrow", "next week") without preposition.
		if due.IsZero() {
			if m := dueBareRe.FindStringSubmatch(window); m != nil {
				due = parseAbsoluteDate(m[1], now)
			}
		}
		// Cycle 81: Chinese date markers.
		if due.IsZero() {
			if m := dueDateCNRe.FindStringSubmatch(window); m != nil {
				due = parseChineseDate(m[1], now)
			}
		}
		// Only commit if we found a due date — undated "I'll think about it"
		// is too noisy.
		if due.IsZero() {
			continue
		}

		// Action description: the window with leading/trailing whitespace trimmed.
		action := strings.TrimSpace(window)
		if len(action) > 120 {
			action = action[:120] + "…"
		}

		out = append(out, ExtractedCommitment{
			Action: action,
			DueAt:  due,
			Source: action,
		})
	}
	return out
}

// SummarizeFromTurn produces an updated rolling summary given the prior
// summary + the agent's response from this turn. v1 heuristic: append the
// most recent action-bearing sentence, capped at 400 chars.
//
// Cycle 69+ can replace this with a Haiku-assisted summarizer if the
// heuristic proves too noisy.
func SummarizeFromTurn(priorSummary, response string) string {
	if response == "" {
		return priorSummary
	}
	// Extract the most action-laden sentence as the summary kernel.
	sentences := splitSentences(response)
	var bestSentence string
	var bestScore int
	for _, s := range sentences {
		score := scoreSentence(s)
		if score > bestScore {
			bestScore = score
			bestSentence = s
		}
	}
	if bestSentence == "" && len(sentences) > 0 {
		bestSentence = sentences[0]
	}

	// Combine with prior summary if it exists.
	combined := strings.TrimSpace(bestSentence)
	if priorSummary != "" {
		combined = priorSummary + " | " + combined
	}
	if len(combined) > 400 {
		combined = combined[len(combined)-400:]
		// Trim leading partial sentence.
		if i := strings.Index(combined, " | "); i >= 0 {
			combined = combined[i+3:]
		}
	}
	return combined
}

func splitSentences(s string) []string {
	// crude; English-centric. Doesn't matter for v1.
	rep := strings.NewReplacer("\n", " ", "\r", " ")
	s = rep.Replace(s)
	parts := regexp.MustCompile(`[.!?。！？]+\s*`).Split(s, -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) >= 8 {
			out = append(out, p)
		}
	}
	return out
}

// scoreSentence: prefers sentences with action verbs / domain nouns.
func scoreSentence(s string) int {
	low := strings.ToLower(s)
	score := 0
	for _, w := range []string{
		"check", "log", "save", "next", "plan", "action", "due", "by ", "in 5", "in 3",
		"diagnose", "diagnosis", "confirmed", "soil", "leaves", "roots",
		"medication", "appointment", "schedule", "reminder",
	} {
		if strings.Contains(low, w) {
			score++
		}
	}
	return score
}

// parseRelativeDuration handles "in 5 days" / "in 2 weeks" etc.
func parseRelativeDuration(n, unit string, base time.Time) time.Time {
	var count int
	fmt.Sscanf(n, "%d", &count)
	if count <= 0 {
		return time.Time{}
	}
	switch strings.ToLower(unit) {
	case "minute":
		return base.Add(time.Duration(count) * time.Minute)
	case "hour":
		return base.Add(time.Duration(count) * time.Hour)
	case "day":
		return base.AddDate(0, 0, count)
	case "week":
		return base.AddDate(0, 0, count*7)
	case "month":
		return base.AddDate(0, count, 0)
	}
	return time.Time{}
}

// parseAbsoluteDate handles "5/14", "2026-05-14", "tomorrow", "next monday".
func parseAbsoluteDate(s string, base time.Time) time.Time {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "tomorrow" {
		return base.AddDate(0, 0, 1)
	}
	if strings.HasPrefix(s, "next ") {
		rest := strings.TrimPrefix(s, "next ")
		switch rest {
		case "week":
			return base.AddDate(0, 0, 7)
		case "month":
			return base.AddDate(0, 1, 0)
		case "monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday":
			return nextWeekday(base, rest)
		}
	}
	// ISO date
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t
	}
	// M/D or M/D/YYYY or M/D/YY
	for _, layout := range []string{"1/2/2006", "1/2/06", "1/2"} {
		if t, err := time.Parse(layout, s); err == nil {
			// If year not in layout, infer current.
			if t.Year() == 0 {
				t = time.Date(base.Year(), t.Month(), t.Day(), 0, 0, 0, 0, base.Location())
			}
			// If date is in the past for this year, bump to next year.
			if t.Before(base.AddDate(0, 0, -1)) {
				t = t.AddDate(1, 0, 0)
			}
			return t
		}
	}
	return time.Time{}
}

// parseChineseDate handles 明天/今晚/後天/大後天/下週/M月D日/M月D號/M/D/週X.
// Cycle 81 — added based on production data showing all real commitments
// from agents use Chinese temporal markers.
func parseChineseDate(s string, base time.Time) time.Time {
	s = strings.TrimSpace(s)
	switch s {
	case "明天", "今晚":
		return base.AddDate(0, 0, 1)
	case "後天":
		return base.AddDate(0, 0, 2)
	case "大後天":
		return base.AddDate(0, 0, 3)
	case "這週末":
		// next Saturday
		return nextWeekday(base, "saturday")
	case "下週", "過幾天":
		return base.AddDate(0, 0, 7)
	}
	// 週X — next occurrence of that weekday in Chinese
	cnWeekdays := map[string]string{
		"週一": "monday", "週二": "tuesday", "週三": "wednesday",
		"週四": "thursday", "週五": "friday", "週六": "saturday",
		"週日": "sunday", "週天": "sunday",
	}
	if en, ok := cnWeekdays[s]; ok {
		return nextWeekday(base, en)
	}
	// M月D日 / M月D號 / M/D
	if m := regexp.MustCompile(`(\d{1,2})月(\d{1,2})[日號]`).FindStringSubmatch(s); m != nil {
		var mo, d int
		fmt.Sscanf(m[1], "%d", &mo)
		fmt.Sscanf(m[2], "%d", &d)
		return monthDayThisOrNextYear(base, mo, d)
	}
	if m := regexp.MustCompile(`(\d{1,2})/(\d{1,2})`).FindStringSubmatch(s); m != nil {
		var mo, d int
		fmt.Sscanf(m[1], "%d", &mo)
		fmt.Sscanf(m[2], "%d", &d)
		return monthDayThisOrNextYear(base, mo, d)
	}
	return time.Time{}
}

// monthDayThisOrNextYear returns the date for month/day in current year,
// or next year if already past.
func monthDayThisOrNextYear(base time.Time, mo, d int) time.Time {
	t := time.Date(base.Year(), time.Month(mo), d, 0, 0, 0, 0, base.Location())
	if t.Before(base.AddDate(0, 0, -1)) {
		t = t.AddDate(1, 0, 0)
	}
	return t
}

func nextWeekday(base time.Time, name string) time.Time {
	targets := map[string]time.Weekday{
		"sunday":    time.Sunday,
		"monday":    time.Monday,
		"tuesday":   time.Tuesday,
		"wednesday": time.Wednesday,
		"thursday":  time.Thursday,
		"friday":    time.Friday,
		"saturday":  time.Saturday,
	}
	target, ok := targets[name]
	if !ok {
		return time.Time{}
	}
	d := (int(target) - int(base.Weekday()) + 7) % 7
	if d == 0 {
		d = 7
	}
	return base.AddDate(0, 0, d)
}
