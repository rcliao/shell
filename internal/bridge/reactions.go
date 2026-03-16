package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/store"
)

// ReactionContext holds the message context for a reaction: the original
// user message and bot response that the reaction was placed on.
type ReactionContext struct {
	UserMessage string // original user message text
	BotResponse string // bot response text
	MessageMap  *store.MessageMap
}

// SaveMessageMap persists a mapping between a user's Telegram message and the
// bot's response message for the current session, including message content.
func (b *Bridge) SaveMessageMap(chatID int64, userMessageID, botMessageID int, userMessage, botResponse string) error {
	sess, err := b.store.GetSession(chatID)
	if err != nil || sess == nil {
		return err
	}
	return b.store.SaveMessageMap(chatID, userMessageID, botMessageID, sess.ID, userMessage, botResponse)
}

// GetMessageMapByBotMsg looks up a message mapping by bot response message ID.
func (b *Bridge) GetMessageMapByBotMsg(chatID int64, botMessageID int) (*store.MessageMap, error) {
	return b.store.GetMessageMapByBotMsg(chatID, botMessageID)
}

// HandleReaction processes an emoji reaction as a user action.
// The emoji→action mapping is controlled by Config.Telegram.ReactionMap.
// messageID is the Telegram message ID that was reacted to, used to look up
// which exchange the reaction targets.
// Returns a response message if the reaction triggered an action, or empty string if ignored.
func (b *Bridge) HandleReaction(ctx context.Context, chatID int64, messageID int, emoji string) (string, error) {
	action, ok := b.reactionMap[emoji]
	if !ok || action == "" {
		return b.unmappedReactionHint(emoji), nil
	}

	// Look up which exchange the reaction targets (if any).
	var rc *ReactionContext
	if msgMap, err := b.store.GetMessageMapByBotMsg(chatID, messageID); err == nil && msgMap != nil {
		rc = &ReactionContext{
			UserMessage: msgMap.UserMessage,
			BotResponse: msgMap.BotResponse,
			MessageMap:  msgMap,
		}
		slog.Info("reaction targets mapped exchange",
			"chat_id", chatID, "emoji", emoji, "action", action,
			"bot_message_id", msgMap.BotMessageID,
			"user_message_id", msgMap.UserMessageID,
			"session_id", msgMap.SessionID,
			"user_message_len", len(msgMap.UserMessage),
			"bot_response_len", len(msgMap.BotResponse),
		)
	}

	// Actions that work regardless of plan state.
	switch action {
	case "status":
		return b.Status(chatID)
	case "cancel":
		return b.PlanStop(chatID)
	case "retry":
		return b.PlanRetry(ctx, chatID)
	case "regenerate":
		resp, err := b.Regenerate(ctx, chatID, rc)
		return resp.Text, err
	case "remember":
		return b.RememberResponse(ctx, chatID, rc)
	case "forget":
		return b.ForgetExchange(ctx, chatID, rc)
	}

	// Log context availability for plan actions.
	if rc != nil {
		slog.Debug("reaction context available for plan action",
			"action", action, "user_message", rc.UserMessage)
	}

	// Remaining actions ("go", "stop", or custom) require an interactive plan.
	b.planMu.Lock()
	run, hasPlan := b.planRuns[chatID]
	b.planMu.Unlock()

	if !hasPlan {
		return "", nil
	}

	switch run.state {
	case planStateDrafting:
		return b.handlePlanDraft(ctx, chatID, action)
	case planStateBlocked:
		return b.handlePlanBlocked(ctx, chatID, action)
	default:
		return "", nil
	}
}

// ReactionAction returns the action name mapped to the given emoji, or "".
func (b *Bridge) ReactionAction(emoji string) string {
	return b.reactionMap[emoji]
}

// Reactions returns a formatted list of the current emoji→action mappings.
func (b *Bridge) Reactions() string {
	if len(b.reactionMap) == 0 {
		return "No emoji reactions configured."
	}
	msg := "## Reactions\n\nReact to any message with these emoji:\n\n"
	for emoji, action := range b.reactionMap {
		msg += fmt.Sprintf("- %s → `%s`\n", emoji, action)
	}
	return msg
}

// unmappedReactionHint returns a short message listing available emoji reactions.
func (b *Bridge) unmappedReactionHint(emoji string) string {
	if len(b.reactionMap) == 0 {
		return fmt.Sprintf("%s is not a recognized reaction.", emoji)
	}
	pairs := make([]string, 0, len(b.reactionMap))
	for e, action := range b.reactionMap {
		pairs = append(pairs, fmt.Sprintf("%s %s", e, action))
	}
	sort.Strings(pairs)
	return fmt.Sprintf("%s is not mapped. Available reactions: %s", emoji, strings.Join(pairs, ", "))
}

// Regenerate re-sends the original user message to get a fresh response from Claude.
func (b *Bridge) Regenerate(ctx context.Context, chatID int64, rc *ReactionContext) (AgentResponse, error) {
	if rc == nil || rc.UserMessage == "" {
		return AgentResponse{Text: "Cannot regenerate: message not found."}, nil
	}
	// Don't regenerate during an active plan.
	b.planMu.Lock()
	run, hasPlan := b.planRuns[chatID]
	b.planMu.Unlock()
	if hasPlan && run.state != planStateDone {
		return AgentResponse{Text: "Cannot regenerate while a plan is active."}, nil
	}
	return b.HandleMessageStreaming(ctx, chatID, rc.UserMessage, "", nil, nil, nil)
}

// RegenerateStreaming re-sends the original user message with streaming support.
// It looks up the exchange by botMessageID, checks plan state, and streams the
// new response via onUpdate. On success it updates the stored message map entry.
func (b *Bridge) RegenerateStreaming(ctx context.Context, chatID int64, botMessageID int, onUpdate process.StreamFunc) (AgentResponse, error) {
	msgMap, err := b.store.GetMessageMapByBotMsg(chatID, botMessageID)
	if err != nil || msgMap == nil || msgMap.UserMessage == "" {
		return AgentResponse{Text: "Cannot regenerate: message not found."}, nil
	}

	// Don't regenerate during an active plan.
	b.planMu.Lock()
	run, hasPlan := b.planRuns[chatID]
	b.planMu.Unlock()
	if hasPlan && run.state != planStateDone {
		return AgentResponse{Text: "Cannot regenerate while a plan is active."}, nil
	}

	resp, err := b.HandleMessageStreaming(ctx, chatID, msgMap.UserMessage, "", nil, nil, onUpdate)
	if err != nil {
		return AgentResponse{}, err
	}

	// Update the stored bot response so subsequent reactions see the new text.
	if err := b.store.UpdateMessageMapResponse(msgMap.ID, resp.Text); err != nil {
		slog.Warn("failed to update message map response", "error", err)
	}

	return resp, nil
}

// RememberResponse saves the bot response (with user question context) to long-term memory.
func (b *Bridge) RememberResponse(ctx context.Context, chatID int64, rc *ReactionContext) (string, error) {
	if b.memory == nil {
		return "Memory is not enabled.", nil
	}
	if rc == nil || rc.BotResponse == "" {
		return "Cannot remember: message not found.", nil
	}
	// Build a memory entry with both question and answer for context.
	userPart := rc.UserMessage
	if len(userPart) > 200 {
		userPart = userPart[:200] + "..."
	}
	botPart := rc.BotResponse
	if len(botPart) > 500 {
		botPart = botPart[:500] + "..."
	}
	content := fmt.Sprintf("Q: %s\nA: %s", userPart, botPart)
	if err := b.memory.Remember(ctx, chatID, content); err != nil {
		return "", fmt.Errorf("remember: %w", err)
	}
	return "Response saved to memory.", nil
}

// ForgetExchange removes a specific exchange from the message log and message map.
func (b *Bridge) ForgetExchange(ctx context.Context, chatID int64, rc *ReactionContext) (string, error) {
	if rc == nil || rc.MessageMap == nil {
		return "Cannot forget: message not found.", nil
	}
	mm := rc.MessageMap
	// Delete the message log entries for this exchange.
	if err := b.store.DeleteExchangeMessages(mm.SessionID, mm.UserMessage, mm.BotResponse); err != nil {
		slog.Warn("failed to delete exchange messages", "error", err)
	}
	// Delete the message_map entry.
	if err := b.store.DeleteMessageMap(mm.ID); err != nil {
		return "", fmt.Errorf("forget: %w", err)
	}
	return "Exchange forgotten.", nil
}

// Remember handles the /remember command.
func (b *Bridge) Remember(ctx context.Context, chatID int64, content string) (string, error) {
	if b.memory == nil {
		return "Memory is not enabled.", nil
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return "Usage: /remember <text to remember>", nil
	}
	if err := b.memory.Remember(ctx, chatID, content); err != nil {
		return "", fmt.Errorf("remember: %w", err)
	}
	return fmt.Sprintf("Remembered: %s", content), nil
}

// Forget handles the /forget command.
func (b *Bridge) Forget(ctx context.Context, chatID int64, key string) (string, error) {
	if b.memory == nil {
		return "Memory is not enabled.", nil
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "Usage: /forget <memory key>\nUse /memories to see keys.", nil
	}
	if err := b.memory.Forget(ctx, chatID, key); err != nil {
		return "", fmt.Errorf("forget: %w", err)
	}
	return fmt.Sprintf("Forgot memory: %s", key), nil
}

// ListMemories handles the /memories command.
func (b *Bridge) ListMemories(ctx context.Context, chatID int64) (string, error) {
	if b.memory == nil {
		return "Memory is not enabled.", nil
	}
	return b.memory.ListMemories(ctx, chatID)
}

// Review handles the /review command — shows all memories with correction indices.
func (b *Bridge) Review(ctx context.Context, chatID int64) (string, error) {
	if b.memory == nil {
		return "Memory is not enabled.", nil
	}
	text, entries, err := b.memory.ReviewMemories(ctx, chatID)
	if err != nil {
		return "", err
	}
	if entries != nil {
		b.reviewMu.Lock()
		b.reviewCache[chatID] = entries
		b.reviewMu.Unlock()
	}
	return text, nil
}

// Correct handles the /correct command — updates a memory by review index.
func (b *Bridge) Correct(ctx context.Context, chatID int64, args string) (string, error) {
	if b.memory == nil {
		return "Memory is not enabled.", nil
	}
	args = strings.TrimSpace(args)
	if args == "" {
		return "Usage: /correct <number> <new content>\nRun /review first to see numbered memories.", nil
	}

	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		return "Usage: /correct <number> <new content>", nil
	}
	idx, err := strconv.Atoi(parts[0])
	if err != nil || idx < 1 {
		return "Invalid number. Run /review to see the list.", nil
	}
	newContent := strings.TrimSpace(parts[1])
	if newContent == "" {
		return "New content cannot be empty.", nil
	}

	b.reviewMu.Lock()
	entries := b.reviewCache[chatID]
	b.reviewMu.Unlock()

	if len(entries) == 0 {
		return "No review data cached. Run /review first.", nil
	}
	if idx > len(entries) {
		return fmt.Sprintf("Number %d out of range (1–%d). Run /review to refresh.", idx, len(entries)), nil
	}

	entry := entries[idx-1]
	if err := b.memory.CorrectMemory(ctx, entry.NS, entry.Key, newContent); err != nil {
		return "", fmt.Errorf("correct memory: %w", err)
	}
	return fmt.Sprintf("Updated memory #%d (**%s**): %s", idx, entry.Key, newContent), nil
}

