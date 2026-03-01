package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rcliao/teeny-relay/internal/config"
	"github.com/rcliao/teeny-relay/internal/memory"
	"github.com/rcliao/teeny-relay/internal/planner"
	"github.com/rcliao/teeny-relay/internal/process"
	"github.com/rcliao/teeny-relay/internal/store"
	"github.com/rcliao/teeny-relay/internal/worktree"
)

// relayDirective represents a message to send to another chat.
type relayDirective struct {
	ChatID  int64
	Message string
}

// relayRe matches [relay to=CHAT_ID]message[/relay] blocks in Claude's response.
var relayRe = regexp.MustCompile(`(?s)\[relay to=(\d+)\]\s*(.*?)\s*\[/relay\]`)

// parseRelayDirectives extracts relay blocks from response text.
// Returns the cleaned response (relays stripped) and the list of relay messages.
func parseRelayDirectives(response string) (string, []relayDirective) {
	matches := relayRe.FindAllStringSubmatchIndex(response, -1)
	if len(matches) == 0 {
		return response, nil
	}

	var relays []relayDirective
	clean := response
	// Process in reverse so indices stay valid
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		chatIDStr := response[m[2]:m[3]]
		msg := strings.TrimSpace(response[m[4]:m[5]])
		chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
		if err != nil {
			continue
		}
		relays = append(relays, relayDirective{ChatID: chatID, Message: msg})
		clean = clean[:m[0]] + clean[m[1]:]
	}
	return strings.TrimSpace(clean), relays
}

// sendRelays dispatches relay messages via the notify function.
func (b *Bridge) sendRelays(relays []relayDirective) {
	if b.notify == nil {
		return
	}
	for _, r := range relays {
		slog.Info("relaying message", "to_chat_id", r.ChatID, "len", len(r.Message))
		b.notify(r.ChatID, r.Message)
	}
}

// NotifyFunc sends a message to a chat. Used for async plan progress reporting.
type NotifyFunc func(chatID int64, msg string)

// planState represents where a plan is in its lifecycle.
type planState string

const (
	planStateIdle      planState = "idle"
	planStateDrafting  planState = "drafting"
	planStateExecuting planState = "executing"
	planStateBlocked   planState = "blocked"
	planStateDone      planState = "done"
)

// planRun tracks the state of an active or completed plan execution.
type planRun struct {
	cancel    context.CancelFunc
	results   []planner.TaskResult
	progress  []string
	done      bool
	startedAt time.Time

	// Drafting state
	state     planState
	draftPlan string
	intent    string

	// Blocked state: index of the task that needs human guidance
	failedTaskIdx int

	// Worktree isolation
	worktreeRepoDir string           // resolved git repo directory (source of the worktree)
	worktreePath    string           // filesystem path to the worktree checkout
	worktreeBranch  string           // git branch name for the worktree
	execPlanner     *planner.Planner // planner configured with worktree WorkDir (nil = use bridge default)
}

type Bridge struct {
	proc    *process.Manager
	store   *store.Store
	memory  *memory.Memory   // nil if disabled
	plan    *planner.Planner // nil if not configured
	notify  NotifyFunc       // optional: push progress to user

	// Worktree isolation for plan execution
	useWorktree  bool   // whether to create worktrees for plans
	repoDir      string // main repository working directory
	worktreeDir  string // base directory for worktree checkouts

	reactionMap map[string]string // emoji → action (e.g. "👍":"go")

	planMu   sync.Mutex
	planRuns map[int64]*planRun
}

func New(proc *process.Manager, store *store.Store, mem *memory.Memory, pl *planner.Planner, useWorktree bool, repoDir string, reactionMap map[string]string) *Bridge {
	wtDir := ""
	if useWorktree {
		wtDir = filepath.Join(config.DefaultConfigDir(), "worktrees")
	}
	if reactionMap == nil {
		reactionMap = config.DefaultReactionMap()
	}
	return &Bridge{
		proc:        proc,
		store:       store,
		memory:      mem,
		plan:        pl,
		useWorktree: useWorktree,
		repoDir:     repoDir,
		worktreeDir: wtDir,
		reactionMap: reactionMap,
		planRuns:    make(map[int64]*planRun),
	}
}

// SetNotifier sets the function used to push async messages (plan progress) to users.
func (b *Bridge) SetNotifier(fn NotifyFunc) {
	b.notify = fn
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

// ReactionContext holds the message context for a reaction: the original
// user message and bot response that the reaction was placed on.
type ReactionContext struct {
	UserMessage string // original user message text
	BotResponse string // bot response text
	MessageMap  *store.MessageMap
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
		return b.Regenerate(ctx, chatID, rc)
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

// HandleMessage processes an incoming user message and returns Claude's response.
// senderName identifies who sent the message (e.g. Telegram first name).
func (b *Bridge) HandleMessage(ctx context.Context, chatID int64, userMsg, senderName string) (string, error) {
	// Check for active plan draft — intercept the message.
	b.planMu.Lock()
	run, hasPlan := b.planRuns[chatID]
	b.planMu.Unlock()
	if hasPlan && run.state == planStateDrafting {
		return b.handlePlanDraft(ctx, chatID, userMsg)
	}
	if hasPlan && run.state == planStateBlocked {
		return b.handlePlanBlocked(ctx, chatID, userMsg)
	}

	sess, err := b.ensureSession(ctx, chatID)
	if err != nil {
		return "", fmt.Errorf("ensure session: %w", err)
	}

	// Log user message
	if err := b.store.LogMessage(sess.ID, "user", userMsg); err != nil {
		slog.Warn("failed to log user message", "error", err)
	}

	// Inject memory context if available
	augmentedMsg := userMsg
	if b.memory != nil {
		augmentedMsg = b.memory.InjectContext(ctx, chatID, userMsg)
	}

	// Tag the message with sender identity so Claude knows who is speaking
	if senderName != "" {
		augmentedMsg = fmt.Sprintf("[From: %s]\n%s", senderName, augmentedMsg)
	}

	// Determine claude session ID for --resume
	procSess, _ := b.proc.Get(chatID)
	claudeSessionID := ""
	if procSess != nil && procSess.HasHistory {
		claudeSessionID = procSess.ClaudeSessionID
	}

	// Build system prompt from memory if available.
	systemPrompt := ""
	if b.memory != nil {
		systemPrompt = b.memory.SystemPrompt(ctx, chatID)
	}

	result, err := b.proc.Send(ctx, chatID, claudeSessionID, augmentedMsg, systemPrompt)
	if err != nil {
		return "", fmt.Errorf("claude: %w", err)
	}

	// Track session ID and mark as having history
	if procSess != nil {
		if result.SessionID != "" {
			procSess.ClaudeSessionID = result.SessionID
			if err := b.store.SaveSession(chatID, result.SessionID); err != nil {
				slog.Warn("failed to update session ID in store", "error", err)
			}
		}
		procSess.HasHistory = true
	}

	response := strings.TrimSpace(result.Text)

	// Extract and send relay directives (messages to other chats)
	response, relays := parseRelayDirectives(response)
	b.sendRelays(relays)

	// Parse memory directives ([remember]...[/remember])
	if b.memory != nil {
		response = b.memory.ParseMemoryDirectives(ctx, chatID, response)
	}

	// Log assistant response
	if err := b.store.LogMessage(sess.ID, "assistant", response); err != nil {
		slog.Warn("failed to log assistant message", "error", err)
	}

	// Log exchange to memory
	if b.memory != nil {
		b.memory.LogExchange(ctx, chatID, userMsg, response)
	}

	// Update session timestamp
	if err := b.store.UpdateSessionStatus(chatID, "active"); err != nil {
		slog.Warn("failed to update session", "error", err)
	}

	return response, nil
}

// HandleMessageStreaming is like HandleMessage but calls onUpdate with partial text as Claude generates it.
// senderName identifies who sent the message (e.g. Telegram first name).
func (b *Bridge) HandleMessageStreaming(ctx context.Context, chatID int64, userMsg, senderName string, onUpdate process.StreamFunc) (string, error) {
	// Check for active plan draft — intercept the message (no streaming needed).
	b.planMu.Lock()
	run, hasPlan := b.planRuns[chatID]
	b.planMu.Unlock()
	if hasPlan && run.state == planStateDrafting {
		return b.handlePlanDraft(ctx, chatID, userMsg)
	}
	if hasPlan && run.state == planStateBlocked {
		return b.handlePlanBlocked(ctx, chatID, userMsg)
	}

	sess, err := b.ensureSession(ctx, chatID)
	if err != nil {
		return "", fmt.Errorf("ensure session: %w", err)
	}

	// Log user message
	if err := b.store.LogMessage(sess.ID, "user", userMsg); err != nil {
		slog.Warn("failed to log user message", "error", err)
	}

	// Inject memory context if available
	augmentedMsg := userMsg
	if b.memory != nil {
		augmentedMsg = b.memory.InjectContext(ctx, chatID, userMsg)
	}

	// Tag the message with sender identity so Claude knows who is speaking
	if senderName != "" {
		augmentedMsg = fmt.Sprintf("[From: %s]\n%s", senderName, augmentedMsg)
	}

	// Determine claude session ID for --resume
	procSess, _ := b.proc.Get(chatID)
	claudeSessionID := ""
	if procSess != nil && procSess.HasHistory {
		claudeSessionID = procSess.ClaudeSessionID
	}

	// Build system prompt from memory if available.
	systemPrompt := ""
	if b.memory != nil {
		systemPrompt = b.memory.SystemPrompt(ctx, chatID)
	}

	// Send to Claude with streaming
	result, err := b.proc.SendStreaming(ctx, chatID, claudeSessionID, augmentedMsg, systemPrompt, onUpdate)
	if err != nil {
		return "", fmt.Errorf("claude: %w", err)
	}

	// Track session ID and mark as having history
	if procSess != nil {
		if result.SessionID != "" {
			procSess.ClaudeSessionID = result.SessionID
			if err := b.store.SaveSession(chatID, result.SessionID); err != nil {
				slog.Warn("failed to update session ID in store", "error", err)
			}
		}
		procSess.HasHistory = true
	}

	response := strings.TrimSpace(result.Text)

	// Extract and send relay directives (messages to other chats)
	response, relays := parseRelayDirectives(response)
	b.sendRelays(relays)

	// Parse memory directives ([remember]...[/remember])
	if b.memory != nil {
		response = b.memory.ParseMemoryDirectives(ctx, chatID, response)
	}

	// Log assistant response
	if err := b.store.LogMessage(sess.ID, "assistant", response); err != nil {
		slog.Warn("failed to log assistant message", "error", err)
	}

	// Log exchange to memory
	if b.memory != nil {
		b.memory.LogExchange(ctx, chatID, userMsg, response)
	}

	// Update session timestamp
	if err := b.store.UpdateSessionStatus(chatID, "active"); err != nil {
		slog.Warn("failed to update session", "error", err)
	}

	return response, nil
}

// HandleCommand processes a bot command.
func (b *Bridge) HandleCommand(ctx context.Context, chatID int64, cmd, args string) (string, error) {
	switch cmd {
	case "new":
		return b.Reset(ctx, chatID)
	case "status":
		return b.Status(chatID)
	case "help":
		return b.Help(), nil
	case "reactions":
		return b.Reactions(), nil
	case "start":
		return b.Start(ctx, chatID)
	case "remember":
		return b.Remember(ctx, chatID, args)
	case "forget":
		return b.Forget(ctx, chatID, args)
	case "memories":
		return b.ListMemories(ctx, chatID)
	case "plan":
		return b.Plan(ctx, chatID, args)
	case "planstatus":
		return b.PlanStatus(chatID)
	case "planstop":
		return b.PlanStop(chatID)
	case "planskip":
		return b.PlanSkip(chatID)
	case "planretry":
		return b.PlanRetry(ctx, chatID)
	default:
		return fmt.Sprintf("Unknown command: /%s", cmd), nil
	}
}

// Start handles the /start command.
func (b *Bridge) Start(ctx context.Context, chatID int64) (string, error) {
	_, err := b.ensureSession(ctx, chatID)
	if err != nil {
		return "", err
	}
	return "Welcome to teeny-relay! Send me a message and I'll forward it to Claude Code.\n\nCommands:\n/new — Start a fresh session\n/status — Show session info\n/remember <text> — Remember something\n/forget <key> — Forget a memory\n/memories — List memories\n/plan <goal> — Draft and run an autonomous plan\n/planstatus — Check plan progress\n/planstop — Cancel running plan\n/reactions — Show emoji reactions\n/help — Show help", nil
}

// Reset kills the current session and creates a fresh one.
func (b *Bridge) Reset(ctx context.Context, chatID int64) (string, error) {
	b.proc.Kill(chatID)
	if err := b.store.DeleteSession(chatID); err != nil {
		slog.Warn("failed to delete session from store", "error", err)
	}

	_, err := b.ensureSession(ctx, chatID)
	if err != nil {
		return "", err
	}
	return "Session reset. Starting fresh conversation.", nil
}

// Status returns info about the current session.
func (b *Bridge) Status(chatID int64) (string, error) {
	sess, err := b.store.GetSession(chatID)
	if err != nil {
		return "", err
	}
	if sess == nil {
		return "No active session. Send a message to start one.", nil
	}

	msgs, err := b.store.GetMessages(sess.ID, 0)
	if err != nil {
		return "", err
	}

	procSess, _ := b.proc.Get(chatID)
	status := "active"
	if procSess != nil {
		status = string(procSess.Status)
	}

	return fmt.Sprintf(
		"## Status\n\n"+
			"**Session:** `%s`\n"+
			"**Status:** %s\n"+
			"**Messages:** %d\n"+
			"**Created:** %s\n"+
			"**Last active:** %s",
		sess.ClaudeSessionID[:12]+"...",
		status,
		len(msgs),
		sess.CreatedAt.Format("2006-01-02 15:04:05"),
		sess.UpdatedAt.Format("2006-01-02 15:04:05"),
	), nil
}

func (b *Bridge) Help() string {
	help := "## teeny-relay\n\n" +
		"Telegram ↔ Claude Code bridge\n\n" +
		"Send any message to chat with Claude Code.\n\n" +
		"---\n\n" +
		"### Commands\n\n" +
		"- `/new` — Start a fresh conversation\n" +
		"- `/status` — Show current session info\n" +
		"- `/remember <text>` — Save a memory\n" +
		"- `/forget <key>` — Remove a stored memory\n" +
		"- `/memories` — List all stored memories\n"

	if b.plan != nil {
		help += "\n### Plan execution\n\n" +
			"- `/plan <goal>` — Draft and run an autonomous plan\n" +
			"- `/planstatus` — Check plan progress\n" +
			"- `/planstop` — Cancel running plan\n" +
			"- `/planskip` — Skip blocked task, continue with next\n" +
			"- `/planretry` — Retry blocked task automatically\n"
	}

	if len(b.reactionMap) > 0 {
		help += "\n### Reactions\n\nReact to any message with:\n\n"
		// Sort by action name for stable output.
		type entry struct{ emoji, action string }
		entries := make([]entry, 0, len(b.reactionMap))
		for emoji, action := range b.reactionMap {
			entries = append(entries, entry{emoji, action})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].action < entries[j].action })
		for _, e := range entries {
			help += fmt.Sprintf("- %s → `%s`\n", e.emoji, e.action)
		}
	}

	help += "\n---\n\n" +
		"`/reactions` — Show emoji→action mappings\n" +
		"`/help` — Show this help message"
	return help
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
func (b *Bridge) Regenerate(ctx context.Context, chatID int64, rc *ReactionContext) (string, error) {
	if rc == nil || rc.UserMessage == "" {
		return "Cannot regenerate: message not found.", nil
	}
	// Don't regenerate during an active plan.
	b.planMu.Lock()
	run, hasPlan := b.planRuns[chatID]
	b.planMu.Unlock()
	if hasPlan && run.state != planStateDone {
		return "Cannot regenerate while a plan is active.", nil
	}
	return b.HandleMessage(ctx, chatID, rc.UserMessage, "")
}

// RegenerateStreaming re-sends the original user message with streaming support.
// It looks up the exchange by botMessageID, checks plan state, and streams the
// new response via onUpdate. On success it updates the stored message map entry.
func (b *Bridge) RegenerateStreaming(ctx context.Context, chatID int64, botMessageID int, onUpdate process.StreamFunc) (string, error) {
	msgMap, err := b.store.GetMessageMapByBotMsg(chatID, botMessageID)
	if err != nil || msgMap == nil || msgMap.UserMessage == "" {
		return "Cannot regenerate: message not found.", nil
	}

	// Don't regenerate during an active plan.
	b.planMu.Lock()
	run, hasPlan := b.planRuns[chatID]
	b.planMu.Unlock()
	if hasPlan && run.state != planStateDone {
		return "Cannot regenerate while a plan is active.", nil
	}

	response, err := b.HandleMessageStreaming(ctx, chatID, msgMap.UserMessage, "", onUpdate)
	if err != nil {
		return "", err
	}

	// Update the stored bot response so subsequent reactions see the new text.
	if err := b.store.UpdateMessageMapResponse(msgMap.ID, response); err != nil {
		slog.Warn("failed to update message map response", "error", err)
	}

	return response, nil
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

// Plan starts plan drafting. If the input contains checklist tasks, it skips
// drafting and executes directly (backwards compatible). Otherwise it asks Claude
// to generate a plan from the intent, entering drafting state.
func (b *Bridge) Plan(ctx context.Context, chatID int64, input string) (string, error) {
	if b.plan == nil {
		return "Planner is not configured. Set planner.enabled=true in config.", nil
	}

	input = strings.TrimSpace(input)
	if input == "" {
		return "Usage: /plan <what you want to do>\n\nDescribe your goal and I'll draft a plan.", nil
	}

	b.planMu.Lock()
	if existing, ok := b.planRuns[chatID]; ok && !existing.done && existing.state != planStateDone {
		b.planMu.Unlock()
		return "A plan is already active. Use /planstop to cancel it first.", nil
	}
	b.planMu.Unlock()

	// If the input already contains checklist tasks, execute directly (backwards compat).
	if tasks := planner.ParsePlan(input); len(tasks) > 0 {
		return b.startExecution(ctx, chatID, input, input)
	}

	// Otherwise, draft a plan from the intent.
	draft, err := b.plan.DraftPlan(ctx, input, "", "")
	if err != nil {
		return "", fmt.Errorf("failed to generate plan: %w", err)
	}

	b.planMu.Lock()
	b.planRuns[chatID] = &planRun{
		state:     planStateDrafting,
		draftPlan: draft,
		intent:    input,
		startedAt: time.Now(),
	}
	b.planMu.Unlock()

	return formatDraftResponse(draft), nil
}

// handlePlanDraft processes user messages while in drafting state.
func (b *Bridge) handlePlanDraft(ctx context.Context, chatID int64, userMsg string) (string, error) {
	b.planMu.Lock()
	run := b.planRuns[chatID]
	b.planMu.Unlock()

	normalized := strings.TrimSpace(strings.ToLower(userMsg))

	switch normalized {
	case "go":
		return b.startExecution(ctx, chatID, run.draftPlan, run.intent)
	case "stop":
		b.planMu.Lock()
		delete(b.planRuns, chatID)
		b.planMu.Unlock()
		return "Plan cancelled.", nil
	default:
		// Treat as revision feedback.
		revised, err := b.plan.DraftPlan(ctx, run.intent, run.draftPlan, userMsg)
		if err != nil {
			return "", fmt.Errorf("failed to revise plan: %w", err)
		}
		b.planMu.Lock()
		run.draftPlan = revised
		b.planMu.Unlock()
		return formatDraftResponse(revised), nil
	}
}

// handlePlanBlocked processes user messages while in blocked state.
// "stop" cancels the plan; anything else is treated as guidance to retry the failed task.
func (b *Bridge) handlePlanBlocked(ctx context.Context, chatID int64, userMsg string) (string, error) {
	b.planMu.Lock()
	run := b.planRuns[chatID]
	failedIdx := run.failedTaskIdx
	planText := run.draftPlan
	tasks := planner.ParsePlan(planText)
	b.planMu.Unlock()

	normalized := strings.TrimSpace(strings.ToLower(userMsg))
	if normalized == "stop" {
		b.planMu.Lock()
		delete(b.planRuns, chatID)
		b.planMu.Unlock()
		return "Plan cancelled.", nil
	}

	// Re-run the failed task with user guidance.
	failedTask := tasks[failedIdx]
	planCtx, cancel := context.WithCancel(context.Background())

	b.planMu.Lock()
	run.cancel = cancel
	run.state = planStateExecuting
	completedCtx := buildCompletedContext(tasks, run.results, failedIdx)
	execPlan := run.execPlanner
	if execPlan == nil {
		execPlan = b.plan
	}
	b.planMu.Unlock()

	progress := func(msg string) {
		b.planMu.Lock()
		run.progress = append(run.progress, msg)
		b.planMu.Unlock()
		if b.notify != nil {
			b.notify(chatID, msg)
		}
	}

	go func() {
		defer cancel()
		progress(fmt.Sprintf("Retrying task %d/%d with guidance: %s", failedIdx+1, len(tasks), failedTask))

		result := execPlan.RunTaskWithGuidance(planCtx, failedTask, userMsg, completedCtx, progress)

		b.planMu.Lock()
		// Replace the failed result with the new one.
		run.results[failedIdx] = result
		b.planMu.Unlock()

		if result.Verdict != planner.VerdictDone {
			// Still blocked on this task.
			b.planMu.Lock()
			run.state = planStateBlocked
			b.planMu.Unlock()
			if b.notify != nil {
				b.notify(chatID, b.formatPlanSummary(run))
			}
			return
		}

		// Git checkpoint after guided retry succeeds.
		execPlan.GitCheckpoint(planCtx, failedTask)

		// Update completed context for remaining tasks.
		updatedCtx := completedCtx + fmt.Sprintf("- %s: %s\n", failedTask, result.Summary)

		// Task passed — continue with remaining tasks.
		if failedIdx+1 < len(tasks) {
			remaining := execPlan.RunPlanFrom(planCtx, planText, failedIdx+1, updatedCtx, progress)

			b.planMu.Lock()
			run.results = append(run.results, remaining...)

			lastIdx := len(remaining) - 1
			if lastIdx >= 0 && remaining[lastIdx].Verdict == planner.VerdictNeedsHuman {
				// Another task blocked — calculate its absolute index.
				run.state = planStateBlocked
				run.failedTaskIdx = failedIdx + 1 + lastIdx
				run.done = false
			} else {
				run.state = planStateDone
				run.done = true
			}
			b.planMu.Unlock()
		} else {
			b.planMu.Lock()
			run.state = planStateDone
			run.done = true
			b.planMu.Unlock()
		}

		// Store reviewer learnings to memory.
		b.storeReviewerLearnings(planCtx, run)

		// Handle worktree cleanup on completion
		b.cleanupWorktree(run)

		if b.notify != nil {
			b.notify(chatID, b.formatPlanSummary(run))
		}
	}()

	return fmt.Sprintf("Retrying task %d with your guidance. Use /planstatus to check progress.", failedIdx+1), nil
}

// startExecution transitions to executing and runs the plan in a background goroutine.
// intent is used to resolve which git repo to create a worktree from when the
// workspace contains multiple repositories.
func (b *Bridge) startExecution(ctx context.Context, chatID int64, planText, intent string) (string, error) {
	tasks := planner.ParsePlan(planText)
	if len(tasks) == 0 {
		return "No tasks found in plan.", nil
	}

	planCtx, cancel := context.WithCancel(context.Background())
	run := &planRun{
		cancel:    cancel,
		state:     planStateExecuting,
		draftPlan: planText,
		intent:    intent,
		startedAt: time.Now(),
	}

	// Create worktree for isolation if enabled
	execPlan := b.plan
	if b.useWorktree && b.repoDir != "" {
		repoDir, err := worktree.ResolveRepoDir(b.repoDir, intent)
		if err != nil {
			slog.Warn("worktree: could not resolve repo, running without isolation", "error", err)
		} else {
			wtPath, branch, err := worktree.Create(repoDir, b.worktreeDir, chatID)
			if err != nil {
				cancel()
				return "", fmt.Errorf("failed to create worktree: %w", err)
			}
			run.worktreeRepoDir = repoDir
			run.worktreePath = wtPath
			run.worktreeBranch = branch
			execPlan = b.plan.CloneWithWorkDir(wtPath)

			slog.Info("plan execution using worktree", "chat_id", chatID, "repo", repoDir, "branch", branch, "path", wtPath)
		}
	}
	// Inject reviewer memory (critical flows) before execution.
	execPlan = b.injectReviewerMemory(ctx, execPlan)
	run.execPlanner = execPlan

	b.planMu.Lock()
	b.planRuns[chatID] = run
	b.planMu.Unlock()

	progress := func(msg string) {
		b.planMu.Lock()
		run.progress = append(run.progress, msg)
		b.planMu.Unlock()

		if b.notify != nil {
			b.notify(chatID, msg)
		}
	}

	go func() {
		defer cancel()
		results := run.execPlanner.RunPlan(planCtx, planText, progress)

		b.planMu.Lock()
		run.results = results
		lastIdx := len(results) - 1
		if lastIdx >= 0 && results[lastIdx].Verdict == planner.VerdictNeedsHuman {
			run.state = planStateBlocked
			run.failedTaskIdx = lastIdx
			run.done = false
		} else {
			run.state = planStateDone
			run.done = true
		}
		b.planMu.Unlock()

		// Store reviewer learnings to memory.
		b.storeReviewerLearnings(planCtx, run)

		// Handle worktree cleanup
		b.cleanupWorktree(run)

		if b.notify != nil {
			b.notify(chatID, b.formatPlanSummary(run))
		}
	}()

	extra := ""
	if run.worktreeBranch != "" {
		extra = fmt.Sprintf("\nWorktree branch: %s", run.worktreeBranch)
	}
	return fmt.Sprintf("Plan started with %d tasks. Progress will be reported as tasks complete.\nUse /planstatus to check, /planstop to cancel.%s", len(tasks), extra), nil
}

// cleanupWorktree handles worktree lifecycle at the end of a plan.
// On success (all done): merge branch into main repo and remove worktree.
// On blocked/failure: remove worktree but keep the branch for inspection.
func (b *Bridge) cleanupWorktree(run *planRun) {
	if run.worktreePath == "" {
		return
	}

	if run.state == planStateDone && run.done {
		// All tasks completed — merge and clean up
		if err := worktree.MergeAndCleanup(run.worktreeRepoDir, run.worktreePath, run.worktreeBranch); err != nil {
			slog.Warn("worktree merge failed", "branch", run.worktreeBranch, "error", err)
			if b.notify != nil {
				// Find chatID from the run — notify via progress
				run.progress = append(run.progress, fmt.Sprintf("Worktree merge failed: %v\nBranch %s is still available for manual merge.", err, run.worktreeBranch))
			}
			return
		}
		slog.Info("worktree merged and cleaned up", "branch", run.worktreeBranch)
	}
	// For blocked/stopped state, worktree is cleaned up by PlanStop
}

func formatDraftResponse(draft string) string {
	return fmt.Sprintf("Here's the proposed plan:\n\n%s\n\nReply 'go' to execute, send edits to revise, or 'stop' to cancel.", draft)
}

// PlanStatus returns the current state of a running or completed plan.
func (b *Bridge) PlanStatus(chatID int64) (string, error) {
	b.planMu.Lock()
	run, ok := b.planRuns[chatID]
	if !ok {
		b.planMu.Unlock()
		return "No plan has been run. Use /plan to start one.", nil
	}

	state := run.state
	results := run.results
	elapsed := time.Since(run.startedAt).Truncate(time.Second)
	progressCount := len(run.progress)
	draft := run.draftPlan
	wtBranch := run.worktreeBranch
	b.planMu.Unlock()

	switch state {
	case planStateDrafting:
		return fmt.Sprintf("Plan: DRAFTING\n\n%s\n\nReply 'go' to execute, send edits to revise, or 'stop' to cancel.", draft), nil

	case planStateExecuting:
		var sb strings.Builder
		sb.WriteString("Plan: RUNNING\n")
		sb.WriteString(fmt.Sprintf("Elapsed: %s\n", elapsed))
		if wtBranch != "" {
			sb.WriteString(fmt.Sprintf("Worktree branch: %s\n", wtBranch))
		}
		sb.WriteString(fmt.Sprintf("Progress messages: %d\n\n", progressCount))

		if len(results) > 0 {
			sb.WriteString("Results:\n")
			for i, r := range results {
				icon := verdictIcon(r.Verdict)
				sb.WriteString(fmt.Sprintf("%d. [%s] %s (%d attempts)\n", i+1, icon, r.Task, r.Attempts))
			}
		}
		return sb.String(), nil

	case planStateBlocked:
		return b.formatBlockedSummary(run), nil

	case planStateDone:
		var sb strings.Builder
		sb.WriteString("Plan: COMPLETED\n")
		sb.WriteString(fmt.Sprintf("Elapsed: %s\n\n", elapsed))

		if len(results) > 0 {
			sb.WriteString("Results:\n")
			for i, r := range results {
				icon := verdictIcon(r.Verdict)
				sb.WriteString(fmt.Sprintf("%d. [%s] %s (%d attempts)\n", i+1, icon, r.Task, r.Attempts))
			}
		}
		return sb.String(), nil

	default:
		return "No plan has been run. Use /plan to start one.", nil
	}
}

// PlanStop cancels a plan from either drafting or executing state.
func (b *Bridge) PlanStop(chatID int64) (string, error) {
	b.planMu.Lock()
	run, ok := b.planRuns[chatID]
	if !ok {
		b.planMu.Unlock()
		return "No plan is currently active.", nil
	}

	state := run.state
	if state == planStateDone {
		b.planMu.Unlock()
		return "Plan already completed. Nothing to stop.", nil
	}

	if run.cancel != nil {
		run.cancel()
	}

	// Clean up worktree if one was created
	wtBranch := run.worktreeBranch
	if run.worktreePath != "" {
		worktree.Cleanup(run.worktreeRepoDir, run.worktreePath, run.worktreeBranch)
	}

	delete(b.planRuns, chatID)
	b.planMu.Unlock()

	suffix := ""
	if wtBranch != "" {
		suffix = fmt.Sprintf("\nWorktree removed. Branch %s kept for inspection.", wtBranch)
	}

	switch state {
	case planStateDrafting:
		return "Draft cancelled." + suffix, nil
	case planStateBlocked:
		return "Blocked plan cancelled." + suffix, nil
	case planStateExecuting:
		return "Plan execution cancelled." + suffix, nil
	default:
		return "Plan cleared." + suffix, nil
	}
}

func verdictIcon(v planner.Verdict) string {
	switch v {
	case planner.VerdictDone:
		return "ok"
	case planner.VerdictNeedsHuman:
		return "BLOCKED"
	case planner.VerdictNeedsRevision:
		return "retry"
	default:
		return "?"
	}
}

// formatPlanSummary creates a human-readable summary of plan results.
func (b *Bridge) formatPlanSummary(run *planRun) string {
	results := run.results
	if len(results) == 0 {
		return "Plan finished with no results."
	}

	// Blocked state: show actionable diagnostic info.
	if run.state == planStateBlocked {
		return b.formatBlockedSummary(run)
	}

	var sb strings.Builder
	sb.WriteString("--- Plan Complete ---\n\n")

	done := 0
	for _, r := range results {
		if r.Verdict == planner.VerdictDone {
			done++
		}
	}
	sb.WriteString(fmt.Sprintf("Tasks: %d/%d completed\n\n", done, len(results)))

	for i, r := range results {
		icon := verdictIcon(r.Verdict)
		sb.WriteString(fmt.Sprintf("%d. [%s] %s\n", i+1, icon, r.Task))
		if r.Verdict == planner.VerdictNeedsHuman {
			summary := r.Summary
			if len(summary) > 500 {
				summary = summary[:500] + "..."
			}
			sb.WriteString(fmt.Sprintf("   Reason: %s\n", summary))
		}
	}

	return sb.String()
}

// formatBlockedSummary creates an actionable summary when a plan is blocked.
func (b *Bridge) formatBlockedSummary(run *planRun) string {
	results := run.results
	totalTasks := len(planner.ParsePlan(run.draftPlan))
	blocked := results[run.failedTaskIdx]

	var sb strings.Builder
	sb.WriteString("--- Plan Blocked ---\n\n")

	// Show completed tasks first
	for i, r := range results {
		icon := verdictIcon(r.Verdict)
		sb.WriteString(fmt.Sprintf("Task %d/%d: [%s] %s\n", i+1, totalTasks, icon, r.Task))
	}

	sb.WriteString(fmt.Sprintf("\nAttempts: %d\n", blocked.Attempts))

	if blocked.Diff != "" {
		diff := blocked.Diff
		if len(diff) > 1000 {
			diff = diff[:1000] + "\n... (truncated)"
		}
		sb.WriteString(fmt.Sprintf("\nChanges on disk:\n%s\n", diff))
	}

	if blocked.TestOutput != "" {
		testOut := blocked.TestOutput
		if len(testOut) > 500 {
			testOut = testOut[:500] + "\n... (truncated)"
		}
		sb.WriteString(fmt.Sprintf("\nTest output:\n%s\n", testOut))
	}

	if blocked.Summary != "" {
		summary := blocked.Summary
		if len(summary) > 500 {
			summary = summary[:500] + "\n... (truncated)"
		}
		sb.WriteString(fmt.Sprintf("\nReviewer feedback:\n%s\n", summary))
	}

	sb.WriteString("\nReply with guidance to retry, or use:\n/planskip — skip this task\n/planretry — retry automatically\n/planstop — cancel the plan")
	return sb.String()
}

// reviewerNamespace returns the memory namespace for reviewer learnings.
func reviewerNamespace(workDir string) string {
	return "reviewer:" + workDir
}

// injectReviewerMemory queries reviewer memory and returns a planner with
// critical flows set. Returns the original planner if memory is unavailable.
func (b *Bridge) injectReviewerMemory(ctx context.Context, pl *planner.Planner) *planner.Planner {
	if b.memory == nil {
		return pl
	}
	ns := reviewerNamespace(pl.WorkDir())
	flows := b.memory.ReviewerContext(ctx, ns, "critical flows verification review", 500)
	if flows == "" {
		return pl
	}
	return pl.WithCriticalFlows(flows)
}

// storeReviewerLearnings persists reviewer learnings from a plan run.
func (b *Bridge) storeReviewerLearnings(ctx context.Context, run *planRun) {
	if b.memory == nil {
		return
	}
	execPlan := run.execPlanner
	if execPlan == nil {
		return
	}
	ns := reviewerNamespace(execPlan.WorkDir())
	for _, result := range run.results {
		for _, learning := range result.ReviewerLearnings {
			if err := b.memory.StoreReviewerLearning(ctx, ns, learning); err != nil {
				slog.Warn("failed to store reviewer learning", "ns", ns, "error", err)
			}
		}
	}
}

// buildCompletedContext creates a summary string from completed task results.
func buildCompletedContext(tasks []string, results []planner.TaskResult, upToIdx int) string {
	var sb strings.Builder
	for i := 0; i < upToIdx && i < len(results); i++ {
		if results[i].Verdict == planner.VerdictDone {
			task := ""
			if i < len(tasks) {
				task = tasks[i]
			} else {
				task = results[i].Task
			}
			sb.WriteString(fmt.Sprintf("- %s: %s\n", task, results[i].Summary))
		}
	}
	return sb.String()
}

// PlanSkip skips the currently blocked task and continues with the next one.
func (b *Bridge) PlanSkip(chatID int64) (string, error) {
	if b.plan == nil {
		return "Planner is not configured.", nil
	}

	b.planMu.Lock()
	run, ok := b.planRuns[chatID]
	if !ok || run.state != planStateBlocked {
		b.planMu.Unlock()
		return "No plan is currently blocked. Nothing to skip.", nil
	}

	failedIdx := run.failedTaskIdx
	planText := run.draftPlan
	tasks := planner.ParsePlan(planText)

	if failedIdx+1 >= len(tasks) {
		run.state = planStateDone
		run.done = true
		b.planMu.Unlock()
		b.cleanupWorktree(run)
		return "Skipped last task. Plan complete.", nil
	}

	planCtx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	run.state = planStateExecuting
	completedCtx := buildCompletedContext(tasks, run.results, failedIdx)
	execPlan := run.execPlanner
	if execPlan == nil {
		execPlan = b.plan
	}
	b.planMu.Unlock()

	progress := func(msg string) {
		b.planMu.Lock()
		run.progress = append(run.progress, msg)
		b.planMu.Unlock()
		if b.notify != nil {
			b.notify(chatID, msg)
		}
	}

	go func() {
		defer cancel()
		progress(fmt.Sprintf("Skipped task %d, continuing from task %d.", failedIdx+1, failedIdx+2))

		remaining := execPlan.RunPlanFrom(planCtx, planText, failedIdx+1, completedCtx, progress)

		b.planMu.Lock()
		run.results = append(run.results, remaining...)
		lastIdx := len(remaining) - 1
		if lastIdx >= 0 && remaining[lastIdx].Verdict == planner.VerdictNeedsHuman {
			run.state = planStateBlocked
			run.failedTaskIdx = failedIdx + 1 + lastIdx
			run.done = false
		} else {
			run.state = planStateDone
			run.done = true
		}
		b.planMu.Unlock()

		// Handle worktree cleanup on completion
		b.cleanupWorktree(run)

		if b.notify != nil {
			b.notify(chatID, b.formatPlanSummary(run))
		}
	}()

	return fmt.Sprintf("Skipping task %d, continuing from task %d. Use /planstatus to check progress.", failedIdx+1, failedIdx+2), nil
}

// PlanRetry retries the blocked task with generic guidance.
func (b *Bridge) PlanRetry(ctx context.Context, chatID int64) (string, error) {
	if b.plan == nil {
		return "Planner is not configured.", nil
	}

	b.planMu.Lock()
	run, ok := b.planRuns[chatID]
	if !ok || run.state != planStateBlocked {
		b.planMu.Unlock()
		return "No plan is currently blocked. Nothing to retry.", nil
	}
	b.planMu.Unlock()

	return b.handlePlanBlocked(ctx, chatID, "Try again, addressing any issues from the previous attempt.")
}

// ensureSession returns the existing session for a chat or creates a new one.
func (b *Bridge) ensureSession(ctx context.Context, chatID int64) (*store.Session, error) {
	sess, err := b.store.GetSession(chatID)
	if err != nil {
		return nil, err
	}
	if sess != nil {
		// Ensure process manager knows about it
		if _, ok := b.proc.Get(chatID); !ok {
			procSess := &process.Session{
				ID:              fmt.Sprintf("%d", sess.ID),
				ChatID:          chatID,
				ClaudeSessionID: sess.ClaudeSessionID,
				Status:          process.StatusActive,
				HasHistory:      true, // restored from DB = already has history
				CreatedAt:       sess.CreatedAt,
				UpdatedAt:       sess.UpdatedAt,
			}
			b.proc.Register(procSess)
		}
		return sess, nil
	}

	// Create new session
	procSess := process.NewSession(chatID)
	b.proc.Register(procSess)

	if err := b.store.SaveSession(chatID, procSess.ClaudeSessionID); err != nil {
		return nil, fmt.Errorf("save session: %w", err)
	}

	// Re-read to get the DB-assigned ID
	return b.store.GetSession(chatID)
}

// CleanupStaleSessions kills sessions that have been idle too long.
func (b *Bridge) CleanupStaleSessions(idleDuration time.Duration) error {
	chatIDs, err := b.store.StaleSessionChatIDs(idleDuration)
	if err != nil {
		return err
	}
	for _, chatID := range chatIDs {
		b.proc.Kill(chatID)
		b.store.UpdateSessionStatus(chatID, "stale")
		slog.Info("cleaned up stale session", "chat_id", chatID)
	}
	return nil
}
