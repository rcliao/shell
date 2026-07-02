package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/store"
	"github.com/rcliao/shell/internal/topic"
)

// toStoreCommitment translates a topic.ExtractedCommitment into the
// store.Commitment shape used by shell.db.topic_threads.
func (b *Bridge) toStoreCommitment(e topic.ExtractedCommitment) store.Commitment {
	return store.Commitment{
		Action:    e.Action,
		DueAt:     e.DueAt,
		Status:    "open",
		Source:    e.Source,
		CreatedAt: time.Now(),
	}
}

// updateThreadStateFromResponse runs the cycle-68 post-turn write path:
// extract commitments + update rolling summary based on the agent's
// response. Best-effort, swallows errors, NEVER affects response delivery.
func (b *Bridge) updateThreadStateFromResponse(ctx context.Context, chatID int64, userMsg, response string) {
	if !b.topicClassifierEnabled() || b.store == nil {
		return
	}
	if response == "" {
		return
	}
	result, err := b.classifier.Classify(ctx, userMsg)
	if err != nil || result.Topic.Name == "" || result.Topic.Name == topic.TopicGeneral {
		return
	}
	topicName := result.Topic.Name

	// Update rolling summary.
	existing, _ := b.store.GetTopicThread(chatID, topicName)
	priorSummary := ""
	if existing != nil {
		priorSummary = existing.Summary
	}
	newSummary := topic.SummarizeFromTurn(priorSummary, response)
	if newSummary != priorSummary && newSummary != "" {
		if err := b.store.SetThreadSummary(chatID, topicName, newSummary); err != nil {
			slog.Warn("set thread summary failed",
				"chat_id", chatID, "topic", topicName, "error", err)
		}
	}

	// Extract + persist commitments.
	commits := topic.ExtractCommitments(response)
	for _, c := range commits {
		if err := b.store.AddCommitment(chatID, topicName, b.toStoreCommitment(c)); err != nil {
			slog.Warn("add commitment failed",
				"chat_id", chatID, "topic", topicName, "action", c.Action, "error", err)
		}
	}
	if len(commits) > 0 {
		slog.Info("thread state updated",
			"chat_id", chatID, "topic", topicName,
			"commitments_added", len(commits))
	}
}

// topicClassifierEnabled reports whether topic classification is wired up.
// Two conditions: a model is configured + memory is available for registry.
func (b *Bridge) topicClassifierEnabled() bool {
	if b.classifier == nil {
		return false
	}
	if b.memory == nil {
		return false
	}
	return b.claudeCfg.ResolveModel("topic_classifier") != ""
}

// readPriorSticky returns the sticky-pointer thread state BEFORE this
// turn's classifier runs. Cycle 145: this is what the foreground would
// see if we deprecated sync classification on the user-response path.
// Returned thread may be nil (cold start / no row yet).
func (b *Bridge) readPriorSticky(chatID int64) (*store.TopicThread, *store.Conversation) {
	if b.store == nil {
		return nil, nil
	}
	conv, _ := b.store.GetConversation(chatID)
	if conv == nil || !conv.CurrentThreadID.Valid {
		return nil, conv
	}
	// Look up the thread by ID (no helper yet — inline single-row query).
	thread, _ := b.store.GetTopicThread(chatID, conv.CurrentTopic)
	return thread, conv
}

// renderStickyBlock returns a Channel B prefix block describing the
// sticky-pointer continuation hypothesis. Cycle 145: this block runs
// ALONGSIDE renderTopicBlock so we can audit whether the agent leans
// on continuity vs fresh-classify when both are present.
//
// Format: [Continuing: <topic> (X turns) | Summary: <first 80 chars>]
//
// Empty string when there's no prior thread (cold start) or when this
// turn's classifier picked the same thread (would be redundant).
func (b *Bridge) renderStickyBlock(stickyThread *store.TopicThread, stickyConv *store.Conversation, classifierTopic string) string {
	if stickyThread == nil {
		return ""
	}
	// Don't render the same block twice — if classifier agrees, the topic
	// block already covers it.
	if stickyThread.Topic == classifierTopic {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[Continuing: ")
	sb.WriteString(stickyThread.Topic)
	if stickyConv != nil && stickyConv.TurnsSinceCheck > 0 {
		fmt.Fprintf(&sb, " (%d turns)", stickyConv.TurnsSinceCheck)
	}
	if stickyThread.Summary != "" {
		s := stickyThread.Summary
		if len(s) > 80 {
			s = s[:77] + "..."
		}
		sb.WriteString(" | Summary: ")
		sb.WriteString(s)
	}
	sb.WriteString("]")
	return sb.String()
}

// classifyTurnTopic classifies the user message and upserts the topic
// to the per-chat registry + bumps the shell.db topic_thread row.
// Returns the result or a "general" fallback on any error. Designed to
// NEVER block the calling turn — failures are logged and swallowed.
//
// Cycle 66: classifier wired + Channel B one-liner.
// Cycle 67: also bumps shell.db.topic_threads (running thread state).
// Cycle 145: also writes per-chat sticky pointer; logs sticky-vs-classify
// agreement for audit (foundation for future async-drift architecture).
func (b *Bridge) classifyTurnTopic(ctx context.Context, chatID int64, msg string) topic.ClassificationResult {
	if !b.topicClassifierEnabled() {
		return topic.ClassificationResult{Topic: topic.Topic{Name: topic.TopicGeneral, ChatID: chatID}, Source: "disabled"}
	}

	cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	start := time.Now()
	result, err := b.classifier.Classify(cctx, msg)
	dur := time.Since(start)
	if err != nil {
		slog.Warn("topic classification failed",
			"chat_id", chatID, "error", err, "duration_ms", dur.Milliseconds())
		return topic.ClassificationResult{Topic: topic.Topic{Name: topic.TopicGeneral, ChatID: chatID}, Source: "error"}
	}

	// Upsert ghost registry (catalog).
	if b.memory != nil && result.Topic.Name != "" && result.Topic.Name != topic.TopicGeneral {
		reg := topic.NewRegistry(b.memory.Store(), chatID)
		if existing, _ := reg.Get(ctx, result.Topic.Name); existing != nil {
			_ = reg.IncrementTurnCount(ctx, result.Topic.Name)
		} else {
			t := result.Topic
			t.ChatID = chatID
			t.TurnCount = 1
			if t.Description == "" && result.Source == "keyword" {
				t.Description = "(keyword-classified, no description)"
			}
			_ = reg.Upsert(ctx, t)
		}
	}

	// Cycle 67: bump shell.db.topic_threads (operational state).
	// Cycle 145: capture the resulting thread_id so we can update the
	// sticky pointer below and emit the audit signal.
	var threadID int64
	stickyMatched := false
	hadPriorSticky := false
	if b.store != nil && result.Topic.Name != "" && result.Topic.Name != topic.TopicGeneral {
		if _, err := b.store.BumpTopicThread(chatID, result.Topic.Name, 0); err != nil {
			slog.Warn("bump topic thread failed",
				"chat_id", chatID, "topic", result.Topic.Name, "error", err)
		}
		if t, _ := b.store.GetTopicThread(chatID, result.Topic.Name); t != nil {
			threadID = t.ID
			// Audit BEFORE upsert so we compare against the prior pointer.
			m, h, _ := b.store.StickyMatched(chatID, threadID)
			stickyMatched = m
			hadPriorSticky = h
			if err := b.store.UpsertConversation(chatID, threadID, result.Topic.Name); err != nil {
				slog.Warn("upsert conversation failed",
					"chat_id", chatID, "topic", result.Topic.Name, "error", err)
			}
		}
	}

	// Cycle 69: persist per-turn decision for feedback analysis.
	if b.store != nil {
		_ = b.store.LogTopicDecision(store.TopicDecision{
			ChatID:     chatID,
			Topic:      result.Topic.Name,
			Source:     result.Source,
			Confidence: result.Confidence,
			LatencyMs:  dur.Milliseconds(),
			IsNew:      result.IsNew,
		})
	}

	slog.Info("topic classified",
		"chat_id", chatID,
		"topic", result.Topic.Name,
		"source", result.Source,
		"is_new", result.IsNew,
		"confidence", fmt.Sprintf("%.2f", result.Confidence),
		"duration_ms", dur.Milliseconds(),
		"sticky_had_prior", hadPriorSticky,
		"sticky_matched", stickyMatched)

	return result
}

// renderTopicBlock returns the Channel B prefix block for the classified
// topic. Cycle 67: now includes full thread state (running summary + open
// commitments + last-turn staleness) read from shell.db.topic_threads.
//
// Format:
//
//	[Topic: plants — last turn 7d ago, 6 prior turns
//	 Summary: brazilian wood overwatered; soil check 5/9 still wet
//	 Open commitments:
//	   - check leaves by 5/14 (OVERDUE 2d)
//	   - repot if root rot found]
func (b *Bridge) renderTopicBlock(ctx context.Context, chatID int64, result topic.ClassificationResult) string {
	if result.Topic.Name == "" || result.Topic.Name == topic.TopicGeneral {
		return ""
	}
	if b.memory == nil || b.store == nil {
		return ""
	}

	reg := topic.NewRegistry(b.memory.Store(), chatID)
	regTopic, _ := reg.Get(ctx, result.Topic.Name)
	thread, _ := b.store.GetTopicThread(chatID, result.Topic.Name)

	var sb strings.Builder
	sb.WriteString("[Topic: ")
	sb.WriteString(result.Topic.Name)

	// Staleness hint based on thread's last-turn timestamp (before we just bumped it).
	if thread != nil && thread.LastTurnAt != nil && thread.TurnCount > 0 {
		age := time.Since(*thread.LastTurnAt)
		switch {
		case age < 30*time.Minute:
			sb.WriteString(" — active conversation")
		case age < 24*time.Hour:
			fmt.Fprintf(&sb, " — last turn %dh ago", int(age.Hours()))
		default:
			fmt.Fprintf(&sb, " — last turn %dd ago", int(age.Hours()/24))
		}
		if thread.TurnCount > 0 {
			fmt.Fprintf(&sb, ", %d prior turns", thread.TurnCount)
		}
	} else if regTopic != nil && regTopic.TurnCount > 0 {
		fmt.Fprintf(&sb, " — %d prior turns", regTopic.TurnCount)
	}

	if regTopic != nil && regTopic.Description != "" && regTopic.Description != "(keyword-classified, no description)" {
		sb.WriteString(" — ")
		sb.WriteString(regTopic.Description)
	}

	if thread != nil && thread.Summary != "" {
		sb.WriteString("\nSummary: ")
		sb.WriteString(thread.Summary)
	}

	if thread != nil && len(thread.OpenCommitments) > 0 {
		hasOpen := false
		for _, c := range thread.OpenCommitments {
			if c.Status == "open" {
				hasOpen = true
				break
			}
		}
		if hasOpen {
			sb.WriteString("\nOpen commitments:")
			for _, c := range thread.OpenCommitments {
				if c.Status != "open" {
					continue
				}
				sb.WriteString("\n  - ")
				sb.WriteString(c.Action)
				if !c.DueAt.IsZero() {
					if c.IsOverdue() {
						fmt.Fprintf(&sb, " (OVERDUE %dd)", int(time.Since(c.DueAt).Hours()/24))
					} else {
						fmt.Fprintf(&sb, " (due %s)", c.DueAt.Format("2006-01-02"))
					}
				}
			}
		}
	}

	sb.WriteString("]")
	return sb.String()
}
