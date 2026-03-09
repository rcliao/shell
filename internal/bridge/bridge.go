package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	browser "github.com/rcliao/shell-browser"
	pm "github.com/rcliao/shell-pm"
	tunnel "github.com/rcliao/shell-tunnel"
	"github.com/rcliao/shell/internal/config"
	shellimagen "github.com/rcliao/shell-imagen"
	"github.com/rcliao/shell/internal/memory"
	"github.com/rcliao/shell/internal/planner"
	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/search"
	"github.com/rcliao/shell/internal/store"
	"github.com/rcliao/shell/internal/worktree"
)

// ImageInfo holds a downloaded image file path together with optional metadata
// from the Telegram API (dimensions and file size).
type ImageInfo struct {
	Path   string // local file path
	Width  int    // image width in pixels (0 if unknown)
	Height int    // image height in pixels (0 if unknown)
	Size   int64  // file size in bytes (0 if unknown)
}

// PDFInfo holds a downloaded PDF file path together with optional metadata.
type PDFInfo struct {
	Path string // local file path
	Size int64  // file size in bytes (0 if unknown)
}

// relayDirective represents a message to send to another chat.
type relayDirective struct {
	ChatID  int64
	Message string
}

// relayRe matches [relay to=CHAT_ID]message[/relay] blocks in Claude's response.
var relayRe = regexp.MustCompile(`(?s)\[relay to=(\d+)\]\s*(.*?)\s*\[/relay\]`)

// scheduleRe matches [schedule at="..." tz="..."]message[/schedule] or [schedule cron="..." ...]message[/schedule].
var scheduleRe = regexp.MustCompile(`(?s)\[schedule\s+(.*?)\](.*?)\[/schedule\]`)

// scheduleAttrRe extracts key="value" pairs from schedule directive attributes.
var scheduleAttrRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

// heartbeatLearningRe matches [heartbeat-learning]...[/heartbeat-learning] blocks.
var heartbeatLearningRe = regexp.MustCompile(`(?s)\[heartbeat-learning\]\s*(.*?)\s*\[/heartbeat-learning\]`)

// taskCompleteRe matches [task-complete id=<N>] directives in heartbeat responses.
var taskCompleteRe = regexp.MustCompile(`\[task-complete id=(\d+)\]`)

// generateImageRe matches [generate-image prompt="..."] directives in Claude's response.
var generateImageRe = regexp.MustCompile(`\[generate-image prompt="([^"]+)"\]`)

// searchRe matches [search query="..." (optional: count="N" freshness="pw")] directives.
var searchRe = regexp.MustCompile(`\[search query="([^"]+)"(?:\s+count="(\d+)")?(?:\s+freshness="([^"]*)")?\]`)

// ImageSendFunc sends an image to a chat. Used by the bridge to send generated images.
type ImageSendFunc func(chatID int64, imageData []byte, caption string)

// ChatActionFunc sends a chat action (e.g. "upload_photo") to a chat.
type ChatActionFunc func(chatID int64, action string)

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
// If a relay message contains [generate-image] directives, images are generated
// and sent to the target chat with an upload_photo loading indicator.
func (b *Bridge) sendRelays(ctx context.Context, relays []relayDirective) {
	if b.notify == nil {
		return
	}
	for _, r := range relays {
		slog.Info("relaying message", "to_chat_id", r.ChatID, "len", len(r.Message))

		// Process any [generate-image] directives embedded in the relay message.
		msg := r.Message
		if b.imagen != nil && b.imageSend != nil {
			imageMatches := generateImageRe.FindAllStringSubmatchIndex(msg, -1)
			if len(imageMatches) > 0 {
				// Send upload_photo action to target chat during generation.
				actionCtx, actionCancel := context.WithCancel(ctx)
				go b.sendUploadPhotoLoop(actionCtx, r.ChatID)

				clean := msg
				for i := len(imageMatches) - 1; i >= 0; i-- {
					m := imageMatches[i]
					prompt := msg[m[2]:m[3]]
					imageData, err := b.imagen.Generate(ctx, prompt)
					if err != nil {
						slog.Error("relay image generation failed", "prompt", prompt, "to_chat_id", r.ChatID, "error", err)
						clean = clean[:m[0]] + "(image generation failed: " + err.Error() + ")" + clean[m[1]:]
						continue
					}
					b.imageSend(r.ChatID, imageData, prompt)
					clean = clean[:m[0]] + clean[m[1]:]
				}
				actionCancel()
				msg = strings.TrimSpace(clean)
			}
		}

		if msg != "" {
			b.notify(r.ChatID, msg)
		}
	}
}

// sendUploadPhotoLoop sends upload_photo chat action every 4s until ctx is cancelled.
func (b *Bridge) sendUploadPhotoLoop(ctx context.Context, chatID int64) {
	if b.chatAction == nil {
		return
	}
	b.chatAction(chatID, "upload_photo")
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.chatAction(chatID, "upload_photo")
		}
	}
}

// parseHeartbeatLearnings extracts [heartbeat-learning] blocks from the response,
// stores them to the heartbeat namespace, and returns the cleaned response.
func (b *Bridge) parseHeartbeatLearnings(ctx context.Context, chatID int64, response string) string {
	if b.memory == nil {
		return response
	}
	matches := heartbeatLearningRe.FindAllStringSubmatchIndex(response, -1)
	if len(matches) == 0 {
		return response
	}

	clean := response
	for i := len(matches) - 1; i >= 0; i-- {
		loc := matches[i]
		content := strings.TrimSpace(response[loc[2]:loc[3]])
		clean = clean[:loc[0]] + clean[loc[1]:]

		if err := b.memory.StoreHeartbeatLearning(ctx, chatID, content); err != nil {
			slog.Warn("failed to store heartbeat learning", "error", err)
		} else {
			slog.Info("stored heartbeat learning", "chat_id", chatID, "len", len(content))
		}
	}
	return strings.TrimSpace(clean)
}

// parseTaskCompletes extracts [task-complete id=N] directives from the response,
// marks the tasks as completed, and returns the cleaned response.
func (b *Bridge) parseTaskCompletes(chatID int64, response string) string {
	matches := taskCompleteRe.FindAllStringSubmatchIndex(response, -1)
	if len(matches) == 0 {
		return response
	}

	clean := response
	for i := len(matches) - 1; i >= 0; i-- {
		loc := matches[i]
		idStr := response[loc[2]:loc[3]]
		clean = clean[:loc[0]] + clean[loc[1]:]

		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		if err := b.store.CompleteTask(id); err != nil {
			slog.Warn("failed to complete task", "id", id, "error", err)
		} else {
			slog.Info("heartbeat completed task", "chat_id", chatID, "task_id", id)
		}
	}
	return strings.TrimSpace(clean)
}

// enrichHeartbeatPrompt augments a heartbeat message with recent conversation
// history, previous heartbeat insights, memory context, and pending background
// tasks for self-improvement reflection and proactive work.
// Returns the original message unchanged if there's nothing to reflect on.
func (b *Bridge) enrichHeartbeatPrompt(ctx context.Context, chatID int64, msg string) string {
	// Fetch context up front to decide whether enrichment is worthwhile
	exchanges := b.memory.RecentExchanges(ctx, chatID, 10)
	insights := b.memory.HeartbeatContext(ctx, chatID, 500)

	// Fetch pending background tasks
	pendingTasks, _ := b.store.PendingTasks(chatID)

	// Fetch general memory context for reflection
	memoryCtx := b.memory.SystemPrompt(ctx, chatID)

	hasContent := len(exchanges) > 0 || insights != "" || len(pendingTasks) > 0

	// Skip enrichment if there's no history, learnings, or tasks
	if !hasContent {
		slog.Debug("heartbeat: skipping enrichment, no history, insights, or tasks", "chat_id", chatID)
		return msg
	}

	var sb strings.Builder

	if len(exchanges) > 0 {
		sb.WriteString("[Recent conversation history for reflection]\n")
		for _, ex := range exchanges {
			sb.WriteString("- ")
			sb.WriteString(ex)
			sb.WriteString("\n")
		}
		sb.WriteString("[End of recent history]\n\n")
	}

	if insights != "" {
		sb.WriteString("[Previous heartbeat insights]\n")
		sb.WriteString(insights)
		sb.WriteString("\n[End of previous insights]\n\n")
	}

	// Include memory context so heartbeat can reflect on stored knowledge
	if memoryCtx != "" {
		truncated := memoryCtx
		if len(truncated) > 1000 {
			truncated = truncated[:1000] + "\n... (truncated)"
		}
		sb.WriteString("[Memory context for reflection]\n")
		sb.WriteString(truncated)
		sb.WriteString("\n[End of memory context]\n\n")
	}

	// Include pending background tasks
	if len(pendingTasks) > 0 {
		sb.WriteString("[Pending background tasks]\n")
		for _, t := range pendingTasks {
			sb.WriteString(fmt.Sprintf("- Task #%d: %s (queued %s)\n", t.ID, t.Description, t.CreatedAt.Format("Jan 2 15:04")))
		}
		sb.WriteString("[End of pending tasks]\n\n")
		sb.WriteString("If you can complete any pending tasks above, do so and emit:\n")
		sb.WriteString("[task-complete id=<task_id>]\n\n")
	}

	sb.WriteString(msg)

	sb.WriteString("\n\n---\n")
	sb.WriteString("Instructions for this heartbeat:\n")
	sb.WriteString("1. Proactively check for anything that needs attention (files, PRs, notifications, scheduled items).\n")
	sb.WriteString("2. If pending background tasks are listed, try to complete them.\n")
	sb.WriteString("3. Reflect on recent conversations and memory for patterns or corrections.\n")
	sb.WriteString("4. If you notice reusable patterns, user preferences, or useful corrections, emit:\n")
	sb.WriteString("   [heartbeat-learning]<specific, actionable insight>[/heartbeat-learning]\n")
	sb.WriteString("5. If there is genuinely nothing to report, respond with just: [noop]\n")
	sb.WriteString("6. Keep responses concise and actionable.\n")

	return sb.String()
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

// repoWorktree holds worktree state for a single repository.
type repoWorktree struct {
	repoDir string           // resolved git repo path
	path    string           // worktree checkout path
	branch  string           // git branch name
	planner *planner.Planner // planner with this worktree as WorkDir
}

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
	failedRepo    string // repo name of the failed task (for multi-repo routing)

	// Multi-repo worktree isolation
	repoWorktrees map[string]*repoWorktree // repo name → worktree info

	// Available repo names discovered at plan time
	repoNames []string

	// Legacy single-repo fields kept for backwards compat with non-repo-grouped plans
	worktreeRepoDir string           // resolved git repo directory (source of the worktree)
	worktreePath    string           // filesystem path to the worktree checkout
	worktreeBranch  string           // git branch name for the worktree
	execPlanner     *planner.Planner // planner configured with worktree WorkDir (nil = use bridge default)
}

type Bridge struct {
	proc    process.Agent
	store   *store.Store
	memory  *memory.Memory   // nil if disabled
	plan    *planner.Planner // nil if not configured
	notify  NotifyFunc       // optional: push progress to user

	// Worktree isolation for plan execution
	useWorktree  bool   // whether to create worktrees for plans
	repoDir      string // main repository working directory
	worktreeDir  string // base directory for worktree checkouts

	reactionMap map[string]string // emoji → action (e.g. "👍":"go")

	// Self-restart: when a plan modifies shell's own source
	selfSourceDir string // resolved path to shell's source dir (empty = disabled)
	onSelfRestart func() // called when self-modification detected after merge

	// Search API keys (resolved from secrets/env at startup)
	braveKey  string
	tavilyKey string

	// Scheduler
	schedulerEnabled bool
	schedulerTZ      string // default timezone for schedules

	// Image generation
	imagen     *shellimagen.Generator // nil if not configured
	imageSend  ImageSendFunc     // sends generated images to Telegram
	chatAction ChatActionFunc    // sends chat actions (e.g. upload_photo) to Telegram

	// Browser automation
	browserCfg browser.Config

	// Tunnel manager
	tunnelMgr *tunnel.Manager // nil if disabled

	// Process manager
	pmMgr *pm.Manager // nil if disabled

	planMu   sync.Mutex
	planRuns map[int64]*planRun

	reviewMu    sync.Mutex
	reviewCache map[int64][]memory.ReviewEntry // last /review result per chat

	// Preemption: user messages can cancel running system (heartbeat/scheduler) sessions.
	systemCancelMu sync.Mutex
	systemCancel   map[int64]context.CancelFunc
}

func New(proc process.Agent, store *store.Store, mem *memory.Memory, pl *planner.Planner, useWorktree bool, repoDir string, reactionMap map[string]string, ig *shellimagen.Generator, braveKey, tavilyKey string, browserCfg browser.Config, tunnelMgr *tunnel.Manager, pmMgr *pm.Manager) *Bridge {
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
		imagen:      ig,
		braveKey:    braveKey,
		tavilyKey:   tavilyKey,
		browserCfg:  browserCfg,
		tunnelMgr:   tunnelMgr,
		pmMgr:       pmMgr,
		planRuns:     make(map[int64]*planRun),
		reviewCache:  make(map[int64][]memory.ReviewEntry),
		systemCancel: make(map[int64]context.CancelFunc),
	}
}

// runBackground runs fn as a pm-managed process (if pm is available) or as a raw goroutine.
// name must be unique across active processes. tags are optional metadata for filtering.
func (b *Bridge) runBackground(ctx context.Context, name, description string, tags map[string]string, fn func(context.Context) error) {
	if b.pmMgr != nil {
		if _, err := b.pmMgr.StartFunc(ctx, name, fn, description, pm.WithTags(tags)); err != nil {
			slog.Warn("pm.StartFunc failed, falling back to goroutine", "name", name, "error", err)
			go func() { fn(ctx) }()
		}
	} else {
		go func() { fn(ctx) }()
	}
}

// trackSession registers the current Claude session as a pm-managed process for visibility.
// Returns a cleanup function that should be deferred. No-op if pm is disabled.
func (b *Bridge) trackSession(ctx context.Context, chatID int64, sender string) func() {
	if b.pmMgr == nil {
		return func() {}
	}
	name := fmt.Sprintf("session-%d", chatID)
	// Clean up any previous stopped entry for this chat.
	b.pmMgr.Remove(name)

	done := make(chan struct{})
	_, err := b.pmMgr.StartFunc(ctx, name, func(fctx context.Context) error {
		select {
		case <-done:
		case <-fctx.Done():
		}
		return nil
	}, fmt.Sprintf("%s session (chat %d)", sender, chatID),
		pm.WithTags(map[string]string{"type": "session", "chat": fmt.Sprint(chatID), "sender": sender}))
	if err != nil {
		slog.Debug("trackSession: pm registration failed", "error", err)
		return func() {}
	}
	return func() { close(done) }
}

// isSystemSender returns true if the sender is a system process (heartbeat, scheduler).
func isSystemSender(sender string) bool {
	return sender == "heartbeat" || sender == "scheduler"
}

// preemptSystemSession cancels any running system (heartbeat/scheduler) session for the chat
// and waits briefly for it to release. Called before user messages to prevent busy conflicts.
func (b *Bridge) preemptSystemSession(chatID int64) {
	b.systemCancelMu.Lock()
	cancel, ok := b.systemCancel[chatID]
	b.systemCancelMu.Unlock()
	if !ok {
		return
	}
	slog.Info("preempting system session for user message", "chat_id", chatID)
	cancel()

	// Wait for the system session to release (up to 3 seconds).
	for i := 0; i < 30; i++ {
		sess, exists := b.proc.Get(chatID)
		if !exists || sess.Status != process.StatusBusy {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	slog.Warn("preempt: system session did not release in time", "chat_id", chatID)
}

// registerSystemCancel stores a cancel function for the current system session,
// allowing user messages to preempt it. Returns a cleanup function.
func (b *Bridge) registerSystemCancel(chatID int64, cancel context.CancelFunc) func() {
	b.systemCancelMu.Lock()
	b.systemCancel[chatID] = cancel
	b.systemCancelMu.Unlock()
	return func() {
		b.systemCancelMu.Lock()
		delete(b.systemCancel, chatID)
		b.systemCancelMu.Unlock()
	}
}

// directiveResult holds the output from a single directive type execution.
type directiveResult struct {
	label   string
	content string
}

// executeDirectivesParallel runs all directive actions concurrently, collecting results.
// Returns the cleaned response and a combined follow-up string (empty if no directives).
func (b *Bridge) executeDirectivesParallel(ctx context.Context, chatID int64, response string) (string, string) {
	// 1. Extract matches and strip all directive patterns (fast, sequential).
	type pendingDirective struct {
		label string
		exec  func() string
	}
	var pending []pendingDirective

	// Search
	if b.braveKey != "" || b.tavilyKey != "" {
		if searchRe.MatchString(response) {
			snapshot := response
			pending = append(pending, pendingDirective{
				label: "Search results",
				exec: func() string {
					_, results := b.parseSearchDirectives(ctx, snapshot)
					return results
				},
			})
			response = searchRe.ReplaceAllString(response, "")
		}
	}

	// Browser
	if b.browserCfg.Enabled {
		if browser.BrowserRe.MatchString(response) {
			snapshot := response
			pending = append(pending, pendingDirective{
				label: "Browser results",
				exec: func() string {
					_, results := b.parseBrowserDirectives(ctx, chatID, snapshot)
					return results
				},
			})
			response = browser.BrowserRe.ReplaceAllString(response, "")
		}
	}

	// PM
	if b.pmMgr != nil {
		if pm.PMRe.MatchString(response) {
			snapshot := response
			pending = append(pending, pendingDirective{
				label: "Process manager",
				exec: func() string {
					_, results := b.parsePMDirectives(ctx, snapshot)
					return results
				},
			})
			response = pm.PMRe.ReplaceAllString(response, "")
		}
	}

	// Tunnel
	if b.tunnelMgr != nil {
		if tunnel.TunnelRe.MatchString(response) {
			snapshot := response
			pending = append(pending, pendingDirective{
				label: "Tunnel",
				exec: func() string {
					_, results := b.parseTunnelDirectives(ctx, snapshot)
					return results
				},
			})
			response = tunnel.TunnelRe.ReplaceAllString(response, "")
		}
	}

	response = strings.TrimSpace(response)

	if len(pending) == 0 {
		return response, ""
	}

	// 2. Execute all directive actions in parallel.
	results := make([]directiveResult, len(pending))
	var wg sync.WaitGroup
	for i, p := range pending {
		wg.Add(1)
		go func(idx int, pd pendingDirective) {
			defer wg.Done()
			content := pd.exec()
			results[idx] = directiveResult{label: pd.label, content: content}
		}(i, p)
	}
	wg.Wait()

	// 3. Combine non-empty results.
	var combined strings.Builder
	for _, r := range results {
		if r.content != "" {
			combined.WriteString(fmt.Sprintf("[%s]\n%s\n\n", r.label, strings.TrimSpace(r.content)))
		}
	}

	return response, combined.String()
}

// SetImageSender sets the function used to send generated images to Telegram.
func (b *Bridge) SetImageSender(fn ImageSendFunc) {
	b.imageSend = fn
}

// SetChatAction sets the function used to send chat actions (e.g. upload_photo) to Telegram.
func (b *Bridge) SetChatAction(fn ChatActionFunc) {
	b.chatAction = fn
}

// Imagen returns the image generator, or nil if not configured.
func (b *Bridge) Imagen() *shellimagen.Generator {
	return b.imagen
}

// SetNotifier sets the function used to push async messages (plan progress) to users.
func (b *Bridge) SetNotifier(fn NotifyFunc) {
	b.notify = fn
}

// SetSelfRestart configures auto-restart when a plan modifies shell's own source.
func (b *Bridge) SetSelfRestart(sourceDir string, fn func()) {
	b.selfSourceDir = sourceDir
	b.onSelfRestart = fn
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

	// Enrich heartbeat messages with conversation history and previous insights
	isHeartbeat := strings.HasPrefix(userMsg, "[Heartbeat] ")
	if isHeartbeat && b.memory != nil {
		augmentedMsg = b.enrichHeartbeatPrompt(ctx, chatID, augmentedMsg)
	}

	// Tag the message with sender identity so Claude knows who is speaking
	if senderName != "" {
		augmentedMsg = fmt.Sprintf("[From: %s]\n%s", senderName, augmentedMsg)
	}

	// Inject current time when scheduler is enabled so Claude can compute relative times
	augmentedMsg = b.injectCurrentTime(augmentedMsg)

	// Preempt any running system session (heartbeat/scheduler) for user messages.
	if !isSystemSender(senderName) {
		b.preemptSystemSession(chatID)
	}

	// For system senders, register a cancellable context so user messages can preempt.
	if isSystemSender(senderName) {
		sysCtx, sysCancel := context.WithCancel(ctx)
		defer sysCancel()
		cleanupCancel := b.registerSystemCancel(chatID, sysCancel)
		defer cleanupCancel()
		ctx = sysCtx
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
	systemPrompt += b.scheduleSystemPrompt()
	systemPrompt += b.imagenSystemPrompt()
	systemPrompt += b.relaySystemPrompt()
	systemPrompt += b.searchSystemPrompt()
	systemPrompt += b.browserSystemPrompt()
	systemPrompt += b.tunnelSystemPrompt()
	systemPrompt += b.pmSystemPrompt()

	// Track session in pm for /pm list visibility.
	endTrack := b.trackSession(ctx, chatID, senderName)
	defer endTrack()

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

	// Execute all directives (search, browser, pm, tunnel) in parallel, single follow-up.
	response, directiveResults := b.executeDirectivesParallel(ctx, chatID, response)
	if directiveResults != "" {
		followUp := directiveResults + "\nUse the results above to respond to the user."
		followUpResult, err := b.proc.Send(ctx, chatID, result.SessionID, followUp, systemPrompt)
		if err != nil {
			slog.Warn("directive follow-up failed", "error", err)
		} else {
			response = strings.TrimSpace(followUpResult.Text)
			if followUpResult.SessionID != "" {
				result.SessionID = followUpResult.SessionID
			}
		}
	}

	// Extract and send relay directives (messages to other chats)
	response, relays := parseRelayDirectives(response)
	b.sendRelays(ctx, relays)

	// Parse heartbeat learning and task-complete directives
	if isHeartbeat {
		if b.memory != nil {
			response = b.parseHeartbeatLearnings(ctx, chatID, response)
			// Run reflect cycle after heartbeat to promote/decay/prune memories
			b.memory.RunReflect(ctx)
			// Summarize old exchanges during heartbeat maintenance
			if n, err := b.memory.SummarizeExchanges(ctx, chatID); err != nil {
				slog.Warn("exchange summarization failed", "error", err)
			} else if n > 0 {
				slog.Info("heartbeat summarized exchanges", "chat_id", chatID, "count", n)
			}
		}
		response = b.parseTaskCompletes(chatID, response)
	}

	// Parse memory directives ([remember]...[/remember])
	if b.memory != nil {
		response = b.memory.ParseMemoryDirectives(ctx, chatID, response)
	}

	// Parse schedule directives ([schedule ...]...[/schedule])
	response = b.parseScheduleDirectives(chatID, response)

	// Parse generate-image directives
	response = b.parseGenerateImageDirectives(ctx, chatID, response)

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
// images optionally contains downloaded image metadata that should be
// included in the message sent to Claude (e.g. downloaded Telegram photos).
// pdfs optionally contains downloaded PDF metadata that should be
// included in the message sent to Claude (e.g. downloaded Telegram documents).
func (b *Bridge) HandleMessageStreaming(ctx context.Context, chatID int64, userMsg, senderName string, images []ImageInfo, pdfs []PDFInfo, onUpdate process.StreamFunc) (string, error) {
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

	// Enrich heartbeat messages with conversation history and previous insights
	isHeartbeat := strings.HasPrefix(userMsg, "[Heartbeat] ")
	if isHeartbeat && b.memory != nil {
		augmentedMsg = b.enrichHeartbeatPrompt(ctx, chatID, augmentedMsg)
	}

	// Tag the message with sender identity so Claude knows who is speaking
	if senderName != "" {
		augmentedMsg = fmt.Sprintf("[From: %s]\n%s", senderName, augmentedMsg)
	}

	// Inject current time when scheduler is enabled so Claude can compute relative times
	augmentedMsg = b.injectCurrentTime(augmentedMsg)

	// Prepend attachment metadata so Claude can read the attached files.
	if len(images) > 0 || len(pdfs) > 0 {
		var sb strings.Builder
		for _, img := range images {
			fmt.Fprintf(&sb, "[Attached image: %s", img.Path)
			if img.Width > 0 && img.Height > 0 {
				fmt.Fprintf(&sb, " | %dx%d", img.Width, img.Height)
			}
			if img.Size > 0 {
				fmt.Fprintf(&sb, " | %s", formatFileSize(img.Size))
			}
			sb.WriteString("]\n")
		}
		for _, pdf := range pdfs {
			fmt.Fprintf(&sb, "[Attached PDF: %s", pdf.Path)
			if pages := countPDFPages(pdf.Path); pages > 0 {
				fmt.Fprintf(&sb, " | %d pages", pages)
			}
			if pdf.Size > 0 {
				fmt.Fprintf(&sb, " | %s", formatFileSize(pdf.Size))
			}
			sb.WriteString("]\n")
		}
		augmentedMsg = sb.String() + augmentedMsg
	}

	// Preempt any running system session (heartbeat/scheduler) for user messages.
	if !isSystemSender(senderName) {
		b.preemptSystemSession(chatID)
	}

	// For system senders, register a cancellable context so user messages can preempt.
	if isSystemSender(senderName) {
		sysCtx, sysCancel := context.WithCancel(ctx)
		defer sysCancel()
		cleanupCancel := b.registerSystemCancel(chatID, sysCancel)
		defer cleanupCancel()
		ctx = sysCtx
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
	systemPrompt += b.scheduleSystemPrompt()
	systemPrompt += b.imagenSystemPrompt()
	systemPrompt += b.relaySystemPrompt()
	systemPrompt += b.searchSystemPrompt()
	systemPrompt += b.browserSystemPrompt()
	systemPrompt += b.tunnelSystemPrompt()
	systemPrompt += b.pmSystemPrompt()

	// Track session in pm for /pm list visibility.
	endTrack := b.trackSession(ctx, chatID, senderName)
	defer endTrack()

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

	// Execute all directives (search, browser, pm, tunnel) in parallel, single follow-up.
	response, directiveResults := b.executeDirectivesParallel(ctx, chatID, response)
	if directiveResults != "" {
		if onUpdate != nil {
			onUpdate("\n\n_Processing directives..._\n\n")
		}
		followUp := directiveResults + "\nUse the results above to respond to the user."
		followUpResult, err := b.proc.SendStreaming(ctx, chatID, result.SessionID, followUp, systemPrompt, onUpdate)
		if err != nil {
			slog.Warn("directive follow-up failed", "error", err)
		} else {
			response = strings.TrimSpace(followUpResult.Text)
			if followUpResult.SessionID != "" {
				result.SessionID = followUpResult.SessionID
			}
		}
	}

	// Extract and send relay directives (messages to other chats)
	response, relays := parseRelayDirectives(response)
	b.sendRelays(ctx, relays)

	// Parse heartbeat learning and task-complete directives
	if isHeartbeat {
		if b.memory != nil {
			response = b.parseHeartbeatLearnings(ctx, chatID, response)
			// Run reflect cycle after heartbeat to promote/decay/prune memories
			b.memory.RunReflect(ctx)
			// Summarize old exchanges during heartbeat maintenance
			if n, err := b.memory.SummarizeExchanges(ctx, chatID); err != nil {
				slog.Warn("exchange summarization failed", "error", err)
			} else if n > 0 {
				slog.Info("heartbeat summarized exchanges", "chat_id", chatID, "count", n)
			}
		}
		response = b.parseTaskCompletes(chatID, response)
	}

	// Parse memory directives ([remember]...[/remember])
	if b.memory != nil {
		response = b.memory.ParseMemoryDirectives(ctx, chatID, response)
	}

	// Parse schedule directives ([schedule ...]...[/schedule])
	response = b.parseScheduleDirectives(chatID, response)

	// Parse generate-image directives
	response = b.parseGenerateImageDirectives(ctx, chatID, response)

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
	case "review":
		return b.Review(ctx, chatID)
	case "correct":
		return b.Correct(ctx, chatID, args)
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
	case "schedule":
		return b.Schedule(ctx, chatID, args)
	case "heartbeat":
		return b.Heartbeat(ctx, chatID, args)
	case "task":
		return b.Task(ctx, chatID, args)
	case "pm":
		return b.PM(ctx, chatID, args)
	case "tunnel":
		return b.Tunnel(ctx, chatID, args)
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
	return "Welcome to shell! Send me a message and I'll forward it to Claude Code.\n\nCommands:\n/new — Start a fresh session\n/status — Show session info\n/remember <text> — Remember something\n/forget <key> — Forget a memory\n/memories — List memories\n/review — Review all memories with summary\n/correct <n> <text> — Correct a memory by number\n/plan <goal> — Draft and run an autonomous plan\n/planstatus — Check plan progress\n/planstop — Cancel running plan\n/reactions — Show emoji reactions\n/help — Show help", nil
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
	help := "## shell\n\n" +
		"Telegram ↔ Claude Code bridge\n\n" +
		"Send any message to chat with Claude Code.\n\n" +
		"---\n\n" +
		"### Commands\n\n" +
		"- `/new` — Start a fresh conversation\n" +
		"- `/status` — Show current session info\n" +
		"- `/remember <text>` — Save a memory\n" +
		"- `/forget <key>` — Remove a stored memory\n" +
		"- `/memories` — List all stored memories\n" +
		"- `/review` — Review all memories with summary\n" +
		"- `/correct <n> <text>` — Correct a memory by number\n"

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

	if b.schedulerEnabled {
		help += "\n### Scheduler\n\n" +
			"- `/schedule add \"*/30 * * * *\" Reminder text` — Recurring notification\n" +
			"- `/schedule add @daily Good morning` — Unquoted alias (`@hourly`, `@daily`, `@weekly`, `@monthly`)\n" +
			"- `/schedule add --prompt \"0 9 * * 1-5\" Check PRs` — Recurring prompt (via Claude)\n" +
			"- `/schedule add --tz America/Los_Angeles \"0 9 * * *\" Check PRs` — Per-schedule timezone\n" +
			"- `/schedule add \"2026-03-10T09:00:00\" One-time reminder` — One-shot\n" +
			"- `/schedule list` — Show all schedules\n" +
			"- `/schedule enable <id>` — Re-enable a paused schedule\n" +
			"- `/schedule pause <id>` — Pause a schedule\n" +
			"- `/schedule delete <id>` — Remove a schedule\n" +
			"\n### Heartbeat\n\n" +
			"Periodic check-in that routes through Claude with session context (like cron but conversational).\n\n" +
			"- `/heartbeat 30m Check inbox and calendar` — Set heartbeat (one per chat)\n" +
			"- `/heartbeat` or `/heartbeat status` — Show current heartbeat\n" +
			"- `/heartbeat stop` — Stop the heartbeat\n" +
			"\n### Background Tasks\n\n" +
			"Queue tasks for heartbeat to pick up during its next check-in.\n\n" +
			"- `/task add <description>` — Add a background task\n" +
			"- `/task list` — Show pending tasks\n" +
			"- `/task done <id>` — Mark a task as completed\n" +
			"- `/task delete <id>` — Remove a task\n"
	}

	if b.pmMgr != nil {
		help += "\n### Process Manager\n\n" +
			"Manage background processes (web servers, watchers, etc.).\n\n" +
			"- `/pm` or `/pm list` — List managed processes\n" +
			"- `/pm start <name> <command> [--dir <dir>]` — Start a background process\n" +
			"- `/pm logs <name>` — View process logs\n" +
			"- `/pm stop <name>` — Stop a process\n" +
			"- `/pm remove <name>` — Remove a stopped process\n"
	}

	if b.tunnelMgr != nil {
		help += "\n### HTTP Tunnels\n\n" +
			"Expose local ports via Cloudflare quick tunnels.\n\n" +
			"- `/tunnel` or `/tunnel list` — List active tunnels\n" +
			"- `/tunnel start <port>` or `/tunnel <port>` — Start a tunnel\n" +
			"- `/tunnel stop <port>` — Stop a tunnel\n"
	}

	if b.imagen != nil {
		help += "\n### Image Generation\n\n" +
			"- `/imagine <prompt>` — Generate an image from a text prompt\n" +
			"- Claude can also generate images in conversation when appropriate\n"
	}

	help += "\n---\n\n" +
		"`/reactions` — Show emoji→action mappings\n" +
		"`/help` — Show this help message"
	return help
}

// scheduleSystemPrompt returns the system prompt addition that documents
// the [schedule] directive for Claude. Empty if scheduler is disabled.
func (b *Bridge) scheduleSystemPrompt() string {
	if !b.schedulerEnabled {
		return ""
	}

	loc, _ := time.LoadLocation(b.schedulerTZ)
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	nowStr := now.Format("2006-01-02T15:04:05")
	tomorrow := now.AddDate(0, 0, 1).Format("2006-01-02")

	return "\n\n## Scheduling\n\n" +
		"**Current time:** " + nowStr + " (" + b.schedulerTZ + ")\n" +
		"**User timezone:** " + b.schedulerTZ + "\n\n" +
		"You can create scheduled reminders and prompts using the [schedule] directive in your responses.\n" +
		"Use this when the user asks to be reminded about something or wants recurring notifications.\n\n" +
		"### Natural language mapping:\n" +
		"- \"remind me tomorrow at 9am\" → `[schedule at=\"" + tomorrow + "T09:00:00\" tz=\"" + b.schedulerTZ + "\"]...[/schedule]`\n" +
		"- \"every weekday at 3pm\" → `[schedule cron=\"0 15 * * 1-5\" tz=\"" + b.schedulerTZ + "\"]...[/schedule]`\n" +
		"- \"daily at 8am\" → `[schedule cron=\"0 8 * * *\" tz=\"" + b.schedulerTZ + "\"]...[/schedule]`\n" +
		"- \"in 2 hours\" → compute " + nowStr + " + 2h and use `at=` with the result\n" +
		"- \"every hour\" → `[schedule cron=\"0 * * * *\"]...[/schedule]`\n\n" +
		"### One-shot (fires once at a specific time):\n" +
		"```\n[schedule at=\"2026-03-10T09:00:00\" tz=\"America/Los_Angeles\"]Remind me to check deployment[/schedule]\n```\n\n" +
		"### Recurring (cron expression):\n" +
		"```\n[schedule cron=\"0 9 * * 1-5\" tz=\"" + b.schedulerTZ + "\"]Weekly standup check[/schedule]\n```\n\n" +
		"### Cron aliases:\n" +
		"`@hourly` = `0 * * * *`, `@daily` = `0 0 * * *`, `@weekly` = `0 0 * * 0`, `@monthly` = `0 0 1 * *`\n\n" +
		"### Attributes:\n" +
		"- `at` — ISO8601 datetime for one-shot schedules (without timezone suffix, uses `tz`)\n" +
		"- `cron` — 5-field cron expression (minute hour day-of-month month day-of-week)\n" +
		"- `tz` — timezone (optional, default: " + b.schedulerTZ + ")\n" +
		"- `mode` — \"notify\" (default, plain message) or \"prompt\" (routed back through you)\n\n" +
		"The directive is stripped from your visible response and replaced with a confirmation.\n" +
		"Always include the `tz` attribute explicitly. Compute exact dates/times based on the current time shown above.\n\n" +
		"### Heartbeat\n" +
		"Messages prefixed with `[Heartbeat]` are periodic check-ins routed through you with full session context.\n" +
		"Recent conversation history, previous heartbeat insights, memory context, and pending background tasks are included.\n" +
		"Respond naturally as a proactive assistant — summarize what you checked, report findings, and suggest actions.\n" +
		"If you notice patterns, user preferences, or useful corrections, emit:\n" +
		"`[heartbeat-learning]<specific actionable insight>[/heartbeat-learning]`\n" +
		"If you complete a background task, emit: `[task-complete id=<task_id>]`\n" +
		"If there is genuinely nothing to report, respond with just: `[noop]`\n" +
		"Keep heartbeat responses concise and actionable.\n"
}

// injectCurrentTime prepends a precise timestamp to the user message when the
// scheduler is enabled, so Claude always knows the exact current time for
// computing relative schedule expressions like "in 30 minutes".
func (b *Bridge) injectCurrentTime(msg string) string {
	if !b.schedulerEnabled {
		return msg
	}
	loc, _ := time.LoadLocation(b.schedulerTZ)
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	return fmt.Sprintf("[Current time: %s (%s)]\n%s", now.Format("2006-01-02T15:04:05"), b.schedulerTZ, msg)
}

// SetSchedulerConfig enables schedule commands and sets the default timezone.
func (b *Bridge) SetSchedulerConfig(enabled bool, tz string) {
	b.schedulerEnabled = enabled
	b.schedulerTZ = tz
	if b.schedulerTZ == "" {
		b.schedulerTZ = "UTC"
	}
}

// Schedule handles the /schedule command with subcommands: add, list, delete.
func (b *Bridge) Schedule(ctx context.Context, chatID int64, args string) (string, error) {
	if !b.schedulerEnabled {
		return "Scheduler is not enabled.", nil
	}

	args = strings.TrimSpace(args)
	if args == "" {
		return "Usage: /schedule add|list|delete ...", nil
	}

	// Parse subcommand
	parts := strings.SplitN(args, " ", 2)
	sub := parts[0]
	rest := ""
	if len(parts) > 1 {
		rest = strings.TrimSpace(parts[1])
	}

	switch sub {
	case "add":
		return b.scheduleAdd(chatID, rest)
	case "list":
		return b.scheduleList(chatID)
	case "delete":
		return b.scheduleDelete(chatID, rest)
	case "enable":
		return b.scheduleEnable(chatID, rest)
	case "pause", "disable":
		return b.schedulePause(chatID, rest)
	default:
		return "Unknown subcommand. Usage: /schedule add|list|delete|enable|pause ...", nil
	}
}

func (b *Bridge) scheduleAdd(chatID int64, args string) (string, error) {
	if args == "" {
		return "Usage: /schedule add [--prompt] [--tz <timezone>] \"<cron or datetime>\" <message>\n\nUnquoted aliases also work: /schedule add @daily Good morning", nil
	}

	mode := "notify"
	if strings.HasPrefix(args, "--prompt ") {
		mode = "prompt"
		args = strings.TrimPrefix(args, "--prompt ")
		args = strings.TrimSpace(args)
	}

	// Parse --tz flag
	tzOverride := ""
	if strings.Contains(args, "--tz ") {
		idx := strings.Index(args, "--tz ")
		after := args[idx+5:]
		tzParts := strings.SplitN(after, " ", 2)
		tzOverride = tzParts[0]
		// Remove --tz <tz> from args
		args = strings.TrimSpace(args[:idx] + " " + strings.TrimSpace(after[len(tzParts[0]):]))
	}

	// Extract quoted expression, @-prefixed alias, or error
	var expr, message string
	if strings.HasPrefix(args, "\"") {
		endQuote := strings.Index(args[1:], "\"")
		if endQuote == -1 {
			return "Missing closing quote for schedule expression.", nil
		}
		expr = args[1 : endQuote+1]
		message = strings.TrimSpace(args[endQuote+2:])
	} else if strings.HasPrefix(args, "@") {
		// Unquoted @alias: split on first space
		aliasParts := strings.SplitN(args, " ", 2)
		expr = aliasParts[0]
		if len(aliasParts) > 1 {
			message = strings.TrimSpace(aliasParts[1])
		}
	} else {
		return "Schedule expression must be quoted or use an @alias. Example: /schedule add \"*/5 * * * *\" My reminder", nil
	}

	if message == "" {
		return "Please provide a message after the schedule expression.", nil
	}

	tz := b.schedulerTZ
	if tzOverride != "" {
		if _, err := time.LoadLocation(tzOverride); err != nil {
			return fmt.Sprintf("Invalid timezone: %s", tzOverride), nil
		}
		tz = tzOverride
	}
	sched := &store.Schedule{
		ChatID:   chatID,
		Label:    message,
		Message:  message,
		Schedule: expr,
		Timezone: tz,
		Mode:     mode,
		Enabled:  true,
	}

	// Auto-detect type: try to parse as datetime first
	if t, err := time.Parse(time.RFC3339, expr); err == nil {
		sched.Type = "once"
		sched.NextRunAt = t.UTC()
	} else if t, err := time.Parse("2006-01-02T15:04:05", expr); err == nil {
		// Parse in the configured timezone
		loc, _ := time.LoadLocation(tz)
		if loc == nil {
			loc = time.UTC
		}
		sched.Type = "once"
		sched.NextRunAt = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc).UTC()
	} else {
		// Try as cron expression
		cronExpr, err := parseScheduleCron(expr)
		if err != nil {
			return fmt.Sprintf("Invalid schedule expression: %s", err), nil
		}
		sched.Type = "cron"
		loc, _ := time.LoadLocation(tz)
		if loc == nil {
			loc = time.UTC
		}
		nextRun := cronExpr.Next(time.Now().In(loc)).UTC()
		if nextRun.IsZero() {
			return "Could not compute next run time.", nil
		}
		sched.NextRunAt = nextRun
	}

	id, err := b.store.SaveSchedule(sched)
	if err != nil {
		return "", fmt.Errorf("save schedule: %w", err)
	}

	modeStr := ""
	if mode == "prompt" {
		modeStr = " (prompt mode)"
	}

	if sched.Type == "once" {
		return fmt.Sprintf("Scheduled #%d: %s at %s%s", id, message, sched.NextRunAt.Format("2006-01-02 15:04 UTC"), modeStr), nil
	}
	return fmt.Sprintf("Scheduled #%d: %s (%s) next: %s%s", id, message, expr, sched.NextRunAt.Format("2006-01-02 15:04 UTC"), modeStr), nil
}

func (b *Bridge) scheduleList(chatID int64) (string, error) {
	schedules, err := b.store.ListSchedules(chatID)
	if err != nil {
		return "", fmt.Errorf("list schedules: %w", err)
	}
	if len(schedules) == 0 {
		return "No schedules found.", nil
	}

	var sb strings.Builder
	sb.WriteString("**Schedules:**\n\n")
	for _, sc := range schedules {
		status := "enabled"
		if !sc.Enabled {
			status = "disabled"
		}
		modeTag := ""
		if sc.Mode == "prompt" {
			modeTag = " [prompt]"
		}
		if sc.Type == "once" {
			sb.WriteString(fmt.Sprintf("**#%d** %s — at %s (%s)%s\n",
				sc.ID, sc.Label, sc.NextRunAt.Format("2006-01-02 15:04 UTC"), status, modeTag))
		} else {
			sb.WriteString(fmt.Sprintf("**#%d** %s — `%s` next: %s (%s)%s\n",
				sc.ID, sc.Label, sc.Schedule, sc.NextRunAt.Format("2006-01-02 15:04 UTC"), status, modeTag))
		}
	}
	return sb.String(), nil
}

func (b *Bridge) scheduleDelete(chatID int64, args string) (string, error) {
	args = strings.TrimSpace(args)
	id, err := strconv.ParseInt(args, 10, 64)
	if err != nil {
		return "Usage: /schedule delete <id>", nil
	}
	if err := b.store.DeleteSchedule(chatID, id); err != nil {
		return fmt.Sprintf("Failed to delete: %s", err), nil
	}
	return fmt.Sprintf("Schedule #%d deleted.", id), nil
}

func (b *Bridge) scheduleEnable(chatID int64, args string) (string, error) {
	args = strings.TrimSpace(args)
	id, err := strconv.ParseInt(args, 10, 64)
	if err != nil {
		return "Usage: /schedule enable <id>", nil
	}
	sc, err := b.store.GetSchedule(chatID, id)
	if err != nil {
		return "", fmt.Errorf("get schedule: %w", err)
	}
	if sc == nil {
		return fmt.Sprintf("Schedule #%d not found.", id), nil
	}

	// Recompute next_run_at for cron types
	if sc.Type == "cron" {
		cronExpr, err := parseScheduleCron(sc.Schedule)
		if err != nil {
			return fmt.Sprintf("Failed to parse cron expression: %s", err), nil
		}
		loc, _ := time.LoadLocation(sc.Timezone)
		if loc == nil {
			loc = time.UTC
		}
		nextRun := cronExpr.Next(time.Now().In(loc)).UTC()
		if nextRun.IsZero() {
			return "Could not compute next run time.", nil
		}
		lastRun := time.Time{}
		if sc.LastRunAt != nil {
			lastRun = *sc.LastRunAt
		}
		if err := b.store.UpdateScheduleNextRun(id, nextRun, lastRun); err != nil {
			return "", fmt.Errorf("update next run: %w", err)
		}
		sc.NextRunAt = nextRun
	}

	if err := b.store.EnableSchedule(id); err != nil {
		return "", fmt.Errorf("enable schedule: %w", err)
	}
	return fmt.Sprintf("Schedule #%d enabled. Next run: %s", id, sc.NextRunAt.Format("2006-01-02 15:04 UTC")), nil
}

func (b *Bridge) schedulePause(chatID int64, args string) (string, error) {
	args = strings.TrimSpace(args)
	id, err := strconv.ParseInt(args, 10, 64)
	if err != nil {
		return "Usage: /schedule pause <id>", nil
	}
	// Verify the schedule belongs to this chat
	sc, err := b.store.GetSchedule(chatID, id)
	if err != nil {
		return "", fmt.Errorf("get schedule: %w", err)
	}
	if sc == nil {
		return fmt.Sprintf("Schedule #%d not found.", id), nil
	}
	if err := b.store.DisableSchedule(id); err != nil {
		return "", fmt.Errorf("disable schedule: %w", err)
	}
	return fmt.Sprintf("Schedule #%d paused.", id), nil
}

// Heartbeat manages the per-chat heartbeat: a periodic prompt that routes through
// Claude with full session context, like a check-in.
func (b *Bridge) Heartbeat(ctx context.Context, chatID int64, args string) (string, error) {
	if !b.schedulerEnabled {
		return "Scheduler is not enabled.", nil
	}

	args = strings.TrimSpace(args)
	if args == "" {
		return b.heartbeatStatus(chatID)
	}

	// Parse subcommand
	parts := strings.SplitN(args, " ", 2)
	sub := parts[0]

	switch sub {
	case "stop":
		return b.heartbeatStop(chatID)
	case "status":
		return b.heartbeatStatus(chatID)
	default:
		// Treat as: /heartbeat <interval> <message>
		return b.heartbeatSet(chatID, args)
	}
}

func (b *Bridge) heartbeatSet(chatID int64, args string) (string, error) {
	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		return "Usage: /heartbeat <interval> <message>\n\nExamples:\n" +
			"  /heartbeat 30m Check inbox and calendar\n" +
			"  /heartbeat 1h Review open PRs and summarize\n" +
			"  /heartbeat 15m Any new notifications?", nil
	}

	intervalStr := parts[0]
	message := strings.TrimSpace(parts[1])

	interval, err := time.ParseDuration(intervalStr)
	if err != nil {
		return fmt.Sprintf("Invalid interval %q. Use Go duration format: 15m, 1h, 30m, 2h30m", intervalStr), nil
	}
	if interval < 1*time.Minute {
		return "Heartbeat interval must be at least 1 minute.", nil
	}

	// Remove existing heartbeat for this chat (one per chat)
	if err := b.store.DeleteHeartbeat(chatID); err != nil {
		slog.Warn("failed to delete old heartbeat", "error", err)
	}

	tz := b.schedulerTZ
	nextRun := time.Now().Add(interval).UTC()

	sched := &store.Schedule{
		ChatID:    chatID,
		Label:     "Heartbeat: " + message,
		Message:   message,
		Schedule:  intervalStr, // store the duration string
		Timezone:  tz,
		Type:      "heartbeat",
		Mode:      "prompt", // always prompt mode
		NextRunAt: nextRun,
		Enabled:   true,
	}

	id, err := b.store.SaveSchedule(sched)
	if err != nil {
		return "", fmt.Errorf("save heartbeat: %w", err)
	}

	return fmt.Sprintf("Heartbeat #%d set: every %s\nMessage: %s\nNext: %s",
		id, intervalStr, message, nextRun.Format("2006-01-02 15:04 UTC")), nil
}

func (b *Bridge) heartbeatStop(chatID int64) (string, error) {
	hb, err := b.store.GetHeartbeat(chatID)
	if err != nil {
		return "", fmt.Errorf("get heartbeat: %w", err)
	}
	if hb == nil {
		return "No active heartbeat.", nil
	}
	if err := b.store.DeleteHeartbeat(chatID); err != nil {
		return "", fmt.Errorf("delete heartbeat: %w", err)
	}
	return "Heartbeat stopped.", nil
}

func (b *Bridge) heartbeatStatus(chatID int64) (string, error) {
	hb, err := b.store.GetHeartbeat(chatID)
	if err != nil {
		return "", fmt.Errorf("get heartbeat: %w", err)
	}
	if hb == nil {
		return "No active heartbeat.\n\nUsage: /heartbeat <interval> <message>\nExample: /heartbeat 30m Check inbox and calendar", nil
	}

	status := "active"
	if !hb.Enabled {
		status = "paused"
	}
	lastRun := "never"
	if hb.LastRunAt != nil {
		lastRun = hb.LastRunAt.Format("2006-01-02 15:04 UTC")
	}

	return fmt.Sprintf("**Heartbeat #%d** (%s)\n"+
		"**Interval:** %s\n"+
		"**Message:** %s\n"+
		"**Next run:** %s\n"+
		"**Last run:** %s\n\n"+
		"Use `/heartbeat stop` to disable.",
		hb.ID, status, hb.Schedule, hb.Message,
		hb.NextRunAt.Format("2006-01-02 15:04 UTC"), lastRun), nil
}

// Task manages the background task queue for a chat.
func (b *Bridge) Task(ctx context.Context, chatID int64, args string) (string, error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return b.taskList(chatID)
	}

	parts := strings.SplitN(args, " ", 2)
	sub := parts[0]

	switch sub {
	case "list":
		return b.taskList(chatID)
	case "add":
		if len(parts) < 2 {
			return "Usage: /task add <description>", nil
		}
		return b.taskAdd(chatID, strings.TrimSpace(parts[1]))
	case "done":
		if len(parts) < 2 {
			return "Usage: /task done <id>", nil
		}
		return b.taskDone(chatID, strings.TrimSpace(parts[1]))
	case "delete":
		if len(parts) < 2 {
			return "Usage: /task delete <id>", nil
		}
		return b.taskDelete(chatID, strings.TrimSpace(parts[1]))
	default:
		// Treat the whole args as a task description for convenience
		return b.taskAdd(chatID, args)
	}
}

func (b *Bridge) taskList(chatID int64) (string, error) {
	tasks, err := b.store.PendingTasks(chatID)
	if err != nil {
		return "", fmt.Errorf("list tasks: %w", err)
	}
	if len(tasks) == 0 {
		return "No pending tasks.\n\nUsage: /task add <description>", nil
	}

	var sb strings.Builder
	sb.WriteString("**Pending Tasks:**\n")
	for _, t := range tasks {
		sb.WriteString(fmt.Sprintf("- #%d: %s (%s)\n", t.ID, t.Description, t.CreatedAt.Format("Jan 2 15:04")))
	}
	sb.WriteString("\nTasks are picked up by heartbeat automatically.")
	return sb.String(), nil
}

func (b *Bridge) taskAdd(chatID int64, description string) (string, error) {
	id, err := b.store.AddTask(chatID, description)
	if err != nil {
		return "", fmt.Errorf("add task: %w", err)
	}
	return fmt.Sprintf("Task #%d added: %s\nIt will be picked up by the next heartbeat.", id, description), nil
}

func (b *Bridge) taskDone(chatID int64, idStr string) (string, error) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return "Invalid task ID.", nil
	}
	if err := b.store.CompleteTask(id); err != nil {
		return "", fmt.Errorf("complete task: %w", err)
	}
	return fmt.Sprintf("Task #%d completed.", id), nil
}

func (b *Bridge) taskDelete(chatID int64, idStr string) (string, error) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return "Invalid task ID.", nil
	}
	if err := b.store.DeleteTask(chatID, id); err != nil {
		return "", fmt.Errorf("delete task: %w", err)
	}
	return fmt.Sprintf("Task #%d deleted.", id), nil
}

// PM manages background processes via user commands.
func (b *Bridge) PM(ctx context.Context, chatID int64, args string) (string, error) {
	if b.pmMgr == nil {
		return "Process manager is not enabled.", nil
	}

	args = strings.TrimSpace(args)
	if args == "" {
		// Default: list
		return pm.Execute(ctx, b.pmMgr, pm.Directive{Action: "list"}), nil
	}

	parts := strings.SplitN(args, " ", 2)
	sub := parts[0]
	rest := ""
	if len(parts) > 1 {
		rest = strings.TrimSpace(parts[1])
	}

	switch sub {
	case "list":
		return pm.Execute(ctx, b.pmMgr, pm.Directive{Action: "list"}), nil

	case "start":
		// /pm start <name> <command> [--dir <dir>]
		if rest == "" {
			return "Usage: /pm start <name> <command> [--dir <dir>]", nil
		}
		name, cmd, dir := parsePMStartArgs(rest)
		if name == "" || cmd == "" {
			return "Usage: /pm start <name> <command> [--dir <dir>]", nil
		}
		return pm.Execute(ctx, b.pmMgr, pm.Directive{Action: "start", Name: name, Command: cmd, Dir: dir}), nil

	case "stop":
		if rest == "" {
			return "Usage: /pm stop <name>", nil
		}
		return pm.Execute(ctx, b.pmMgr, pm.Directive{Action: "stop", Name: rest}), nil

	case "logs":
		if rest == "" {
			return "Usage: /pm logs <name>", nil
		}
		return pm.Execute(ctx, b.pmMgr, pm.Directive{Action: "logs", Name: rest}), nil

	case "remove":
		if rest == "" {
			return "Usage: /pm remove <name>", nil
		}
		return pm.Execute(ctx, b.pmMgr, pm.Directive{Action: "remove", Name: rest}), nil

	default:
		return "Usage: /pm list|start|stop|logs|remove\n\n" +
			"Examples:\n" +
			"  /pm list\n" +
			"  /pm start myserver node server.js --dir /path/to/app\n" +
			"  /pm logs myserver\n" +
			"  /pm stop myserver\n" +
			"  /pm remove myserver", nil
	}
}

// Tunnel manages HTTP tunnels via user commands.
func (b *Bridge) Tunnel(ctx context.Context, chatID int64, args string) (string, error) {
	if b.tunnelMgr == nil {
		return "Tunnel is not enabled.", nil
	}

	args = strings.TrimSpace(args)
	if args == "" {
		// Default: list
		return tunnel.Execute(ctx, b.tunnelMgr, tunnel.Directive{Action: "list"}), nil
	}

	parts := strings.SplitN(args, " ", 2)
	sub := parts[0]
	rest := ""
	if len(parts) > 1 {
		rest = strings.TrimSpace(parts[1])
	}

	switch sub {
	case "list":
		return tunnel.Execute(ctx, b.tunnelMgr, tunnel.Directive{Action: "list"}), nil
	case "start":
		if rest == "" {
			return "Usage: /tunnel start <port> [--protocol https]", nil
		}
		port, protocol := parseTunnelStartArgs(rest)
		return tunnel.Execute(ctx, b.tunnelMgr, tunnel.Directive{Action: "start", Port: port, Protocol: protocol}), nil
	case "stop":
		if rest == "" {
			return "Usage: /tunnel stop <port>", nil
		}
		return tunnel.Execute(ctx, b.tunnelMgr, tunnel.Directive{Action: "stop", Port: rest}), nil
	default:
		// Treat as port number for quick start: /tunnel 8080
		return tunnel.Execute(ctx, b.tunnelMgr, tunnel.Directive{Action: "start", Port: sub}), nil
	}
}

// parseTunnelStartArgs parses "<port> [--protocol <proto>]" from /tunnel start args.
func parseTunnelStartArgs(args string) (port, protocol string) {
	if idx := strings.Index(args, "--protocol "); idx >= 0 {
		after := args[idx+11:]
		protoParts := strings.SplitN(after, " ", 2)
		protocol = protoParts[0]
		args = strings.TrimSpace(args[:idx])
	}
	return strings.TrimSpace(args), protocol
}

// parsePMStartArgs parses "<name> <command> [--dir <dir>]" from /pm start args.
func parsePMStartArgs(args string) (name, cmd, dir string) {
	// Extract --dir flag if present.
	if idx := strings.Index(args, "--dir "); idx >= 0 {
		after := args[idx+6:]
		dirParts := strings.SplitN(after, " ", 2)
		dir = dirParts[0]
		args = strings.TrimSpace(args[:idx])
	}

	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		return "", "", ""
	}
	return parts[0], parts[1], dir
}

// parseGenerateImageDirectives extracts [generate-image prompt="..."] directives
// from Claude's response, generates images, and sends them via Telegram.
func (b *Bridge) parseGenerateImageDirectives(ctx context.Context, chatID int64, response string) string {
	if b.imagen == nil || b.imageSend == nil {
		return response
	}

	matches := generateImageRe.FindAllStringSubmatchIndex(response, -1)
	if len(matches) == 0 {
		return response
	}

	clean := response
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		prompt := response[m[2]:m[3]]

		imageData, err := b.imagen.Generate(ctx, prompt)
		if err != nil {
			slog.Error("image generation failed", "prompt", prompt, "error", err)
			clean = clean[:m[0]] + "(image generation failed: " + err.Error() + ")" + clean[m[1]:]
			continue
		}

		b.imageSend(chatID, imageData, prompt)
		clean = clean[:m[0]] + clean[m[1]:]
	}

	return strings.TrimSpace(clean)
}

// HandleImagine generates an image from the given prompt and returns the image bytes.
func (b *Bridge) HandleImagine(ctx context.Context, prompt string) ([]byte, error) {
	if b.imagen == nil {
		return nil, fmt.Errorf("image generation is not configured (set GEMINI_API_KEY)")
	}
	return b.imagen.Generate(ctx, prompt)
}

// imagenSystemPrompt returns the system prompt addition that documents
// the [generate-image] directive for Claude. Empty if imagen is not configured.
func (b *Bridge) imagenSystemPrompt() string {
	if b.imagen == nil {
		return ""
	}
	return "\n\n## Image Generation\n\n" +
		"You can generate images for the user using the [generate-image] directive.\n" +
		"When the user asks you to create, draw, generate, or visualize an image, include this directive in your response:\n\n" +
		"```\n[generate-image prompt=\"a detailed description of the image to generate\"]\n```\n\n" +
		"The prompt should be a detailed, descriptive text that captures what the user wants.\n" +
		"The directive will be replaced with the generated image sent as a photo.\n" +
		"You can include multiple directives in one response for multiple images.\n" +
		"Use this proactively when the user's request would benefit from a visual.\n"
}

// relaySystemPrompt returns the system prompt addition that documents
// the [relay] directive for Claude.
func (b *Bridge) relaySystemPrompt() string {
	return "\n\n## Message Relay\n\n" +
		"You can send messages to other Telegram chats using the [relay] directive.\n" +
		"Use this when the user asks you to forward, send, or share something with another user or chat.\n\n" +
		"```\n[relay to=CHAT_ID]\nMessage content here\n[/relay]\n```\n\n" +
		"Replace CHAT_ID with the numeric Telegram chat ID of the target.\n" +
		"You can include [generate-image] directives inside a relay block to generate and send images to the target chat:\n\n" +
		"```\n[relay to=CHAT_ID]\nHere's the image you requested!\n[generate-image prompt=\"a detailed description\"]\n[/relay]\n```\n\n" +
		"You can include multiple relay blocks in one response.\n"
}

// searchSystemPrompt returns the system prompt addition that tells Claude
// to use the [search] directive for web search.
func (b *Bridge) searchSystemPrompt() string {
	if b.braveKey == "" && b.tavilyKey == "" {
		return ""
	}
	return "\n\n## Web Search\n\n" +
		"You can search the web using the [search] directive. Use this instead of the built-in WebSearch tool.\n" +
		"It uses the Brave Search API and returns richer, more detailed results.\n\n" +
		"```\n[search query=\"your search query\"]\n```\n\n" +
		"Optional attributes:\n" +
		"- `count=\"N\"` — number of results (default 5)\n" +
		"- `freshness=\"pd|pw|pm|py\"` — time filter (24h, 7d, 31d, 1yr)\n\n" +
		"Example: `[search query=\"golang error handling best practices\" count=\"5\"]`\n\n" +
		"The directive will be replaced with search results, and you will be re-prompted to answer using those results.\n" +
		"Always prefer the [search] directive over WebSearch or ddgr.\n"
}

// parseSearchDirectives finds [search query="..."] directives in Claude's response,
// runs the search, and returns the query + results for a follow-up turn.
// Returns the cleaned response and search results (empty if no directives found).
func (b *Bridge) parseSearchDirectives(ctx context.Context, response string) (string, string) {
	if b.braveKey == "" && b.tavilyKey == "" {
		return response, ""
	}

	matches := searchRe.FindAllStringSubmatchIndex(response, -1)
	if len(matches) == 0 {
		return response, ""
	}

	var results strings.Builder
	for _, m := range matches {
		query := response[m[2]:m[3]]
		count := 5
		if m[4] >= 0 {
			if n, err := strconv.Atoi(response[m[4]:m[5]]); err == nil {
				count = n
			}
		}
		freshness := ""
		if m[6] >= 0 {
			freshness = response[m[6]:m[7]]
		}

		slog.Info("search directive", "query", query, "count", count, "freshness", freshness)

		searchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		resp, err := search.Search(searchCtx, b.braveKey, b.tavilyKey, search.Options{
			Query:     query,
			Count:     count,
			Freshness: freshness,
		})
		cancel()

		if err != nil {
			slog.Warn("search failed", "query", query, "error", err)
			results.WriteString(fmt.Sprintf("[Search for %q failed: %s]\n\n", query, err))
			continue
		}
		results.WriteString(search.Markdown(resp))
		results.WriteString("\n")
	}

	// Strip directives from response
	cleaned := searchRe.ReplaceAllString(response, "")
	cleaned = strings.TrimSpace(cleaned)

	return cleaned, results.String()
}

// browserSystemPrompt returns the system prompt addition that tells Claude
// how to use the [browser] directive for browser automation.
func (b *Bridge) browserSystemPrompt() string {
	if !b.browserCfg.Enabled {
		return ""
	}
	return "\n\n## Browser Automation\n\n" +
		"You can automate a headless Chrome browser using the `[browser]` directive.\n" +
		"Use this for tasks that require interacting with web pages: navigating, clicking, typing, extracting content, or taking screenshots.\n\n" +
		"```\n[browser url=\"https://example.com\"]\nscreenshot\n[/browser]\n```\n\n" +
		"**Available actions (one per line):**\n" +
		"- `navigate` — navigate to the URL (implicit, always happens first)\n" +
		"- `click \"<selector>\"` — click an element by CSS selector\n" +
		"- `type \"<selector>\" \"<value>\"` — clear and type into an input\n" +
		"- `wait \"<selector>\"` — wait for element to appear (up to 10s)\n" +
		"- `screenshot` — capture full page screenshot (sent as photo)\n" +
		"- `extract \"<selector>\"` — extract text content of element(s)\n" +
		"- `js \"<expression>\"` — evaluate JavaScript and return result\n" +
		"- `sleep \"<duration>\"` — wait (e.g., `sleep \"2s\"`)\n\n" +
		"**Example — multi-step login and extract:**\n```\n[browser url=\"https://example.com/login\"]\n" +
		"type \"#email\" \"user@example.com\"\ntype \"#password\" \"secret\"\nclick \"#submit\"\n" +
		"wait \"#dashboard\"\nscreenshot\nextract \"#welcome-message\"\njs \"document.title\"\n[/browser]\n```\n\n" +
		"The directive will be replaced with results, and you will be re-prompted to answer using those results.\n" +
		"Screenshots are sent as photos to the chat.\n"
}

func (b *Bridge) tunnelSystemPrompt() string {
	if b.tunnelMgr == nil {
		return ""
	}
	return "\n\n## HTTP Tunnels\n\n" +
		"You can expose local ports to the internet using the `[tunnel]` directive (powered by Cloudflare quick tunnels).\n\n" +
		"```\n[tunnel port=\"8080\"]\n[tunnel action=\"stop\" port=\"8080\"]\n[tunnel action=\"list\"]\n```\n\n" +
		"**Attributes:**\n" +
		"- `port` — local port to expose (required for start/stop)\n" +
		"- `action` — `start` (default), `stop`, or `list`\n" +
		"- `protocol` — `http` (default) or `https`\n\n" +
		"The bridge handles the tunnel — do NOT use Bash for cloudflared.\n"
}

func (b *Bridge) pmSystemPrompt() string {
	if b.pmMgr == nil {
		return ""
	}
	return "\n\n## Process Manager\n\n" +
		"Use the `[pm]` directive to manage background processes (web servers, watchers, etc.).\n" +
		"**CRITICAL:** NEVER run long-running processes (servers, watchers) directly via Bash — it will block your session. " +
		"Always use `[pm]` to start them in the background.\n\n" +
		"**Start a process:**\n```\n[pm name=\"myserver\" cmd=\"node server.js\" dir=\"/path/to/app\"]\n```\n\n" +
		"**Other actions:**\n```\n[pm action=\"list\"]\n[pm action=\"logs\" name=\"myserver\"]\n[pm action=\"stop\" name=\"myserver\"]\n[pm action=\"remove\" name=\"myserver\"]\n```\n\n" +
		"**Attributes:**\n" +
		"- `name` — unique name for the process (required for start/stop/logs/remove)\n" +
		"- `cmd` — shell command to run (required for start)\n" +
		"- `dir` — working directory (optional)\n" +
		"- `action` — `start` (default when cmd provided), `stop`, `list`, `logs`, `remove`\n\n" +
		"**Correct web app workflow:**\n" +
		"1. Write app files\n" +
		"2. `[pm name=\"web\" cmd=\"node server.js\" dir=\"/path/to/app\"]` — starts in background\n" +
		"3. `[tunnel port=\"8080\"]` — expose via public URL\n\n" +
		"The bridge manages processes and captures logs — do NOT use `nohup`, `&`, or Bash for background processes.\n"
}

// parseBrowserDirectives finds [browser url="..."]...[/browser] blocks in Claude's response,
// runs the browser actions, and returns the cleaned response + formatted results for follow-up.
func (b *Bridge) parseBrowserDirectives(ctx context.Context, chatID int64, response string) (string, string) {
	if !b.browserCfg.Enabled {
		return response, ""
	}

	matches := browser.BrowserRe.FindAllStringSubmatchIndex(response, -1)
	if len(matches) == 0 {
		return response, ""
	}

	var results strings.Builder
	for _, m := range matches {
		url := response[m[2]:m[3]]
		body := response[m[4]:m[5]]

		slog.Info("browser directive", "url", url)

		d := browser.ParseDirective(url, body)
		r := browser.Execute(ctx, b.browserCfg, d)

		// Send screenshots via imageSend
		for _, step := range r.Steps {
			if step.Screenshot != nil && b.imageSend != nil {
				if b.chatAction != nil {
					b.chatAction(chatID, "upload_photo")
				}
				b.imageSend(chatID, step.Screenshot, "")
			}
		}

		results.WriteString(browser.FormatResults(r))
		results.WriteString("\n")
	}

	// Strip directives from response
	cleaned := browser.BrowserRe.ReplaceAllString(response, "")
	cleaned = strings.TrimSpace(cleaned)

	return cleaned, results.String()
}

// parseTunnelDirectives finds [tunnel ...] blocks in Claude's response,
// executes tunnel actions (start/stop/list), and returns the cleaned response + results.
func (b *Bridge) parseTunnelDirectives(ctx context.Context, response string) (string, string) {
	if b.tunnelMgr == nil {
		return response, ""
	}

	matches := tunnel.TunnelRe.FindAllStringIndex(response, -1)
	if len(matches) == 0 {
		return response, ""
	}

	var results strings.Builder
	for _, m := range matches {
		match := response[m[0]:m[1]]
		d := tunnel.ParseDirective(match)
		slog.Info("tunnel directive", "action", d.Action, "port", d.Port)
		result := tunnel.Execute(ctx, b.tunnelMgr, d)
		results.WriteString(result)
		results.WriteString("\n")
	}

	cleaned := tunnel.TunnelRe.ReplaceAllString(response, "")
	cleaned = strings.TrimSpace(cleaned)

	return cleaned, results.String()
}

// parsePMDirectives finds [pm ...] blocks in Claude's response,
// executes process management actions, and returns the cleaned response + results.
func (b *Bridge) parsePMDirectives(ctx context.Context, response string) (string, string) {
	if b.pmMgr == nil {
		return response, ""
	}

	matches := pm.PMRe.FindAllStringIndex(response, -1)
	if len(matches) == 0 {
		return response, ""
	}

	var results strings.Builder
	for _, m := range matches {
		match := response[m[0]:m[1]]
		d := pm.ParseDirective(match)
		slog.Info("pm directive", "action", d.Action, "name", d.Name)
		result := pm.Execute(ctx, b.pmMgr, d)
		results.WriteString(result)
		results.WriteString("\n")
	}

	cleaned := pm.PMRe.ReplaceAllString(response, "")
	cleaned = strings.TrimSpace(cleaned)

	return cleaned, results.String()
}

func (b *Bridge) parseScheduleDirectives(chatID int64, response string) string {
	if !b.schedulerEnabled {
		return response
	}

	matches := scheduleRe.FindAllStringSubmatchIndex(response, -1)
	if len(matches) == 0 {
		return response
	}

	var cleaned strings.Builder
	lastEnd := 0

	for _, match := range matches {
		cleaned.WriteString(response[lastEnd:match[0]])
		lastEnd = match[1]

		attrs := response[match[2]:match[3]]
		msg := strings.TrimSpace(response[match[4]:match[5]])

		// Parse attributes
		attrMap := make(map[string]string)
		for _, am := range scheduleAttrRe.FindAllStringSubmatch(attrs, -1) {
			attrMap[am[1]] = am[2]
		}

		tz := attrMap["tz"]
		if tz == "" {
			tz = b.schedulerTZ
		}
		mode := attrMap["mode"]
		if mode == "" {
			mode = "notify"
		}

		sched := &store.Schedule{
			ChatID:   chatID,
			Label:    msg,
			Message:  msg,
			Timezone: tz,
			Mode:     mode,
			Enabled:  true,
		}

		if at, ok := attrMap["at"]; ok {
			sched.Schedule = at
			sched.Type = "once"
			if t, err := time.Parse(time.RFC3339, at); err == nil {
				sched.NextRunAt = t.UTC()
			} else if t, err := time.Parse("2006-01-02T15:04:05", at); err == nil {
				loc, _ := time.LoadLocation(tz)
				if loc == nil {
					loc = time.UTC
				}
				sched.NextRunAt = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc).UTC()
			} else {
				slog.Warn("schedule directive: invalid at time", "at", at)
				continue
			}
		} else if cronStr, ok := attrMap["cron"]; ok {
			sched.Schedule = cronStr
			sched.Type = "cron"
			cronExpr, err := parseScheduleCron(cronStr)
			if err != nil {
				slog.Warn("schedule directive: invalid cron", "cron", cronStr, "error", err)
				continue
			}
			loc, _ := time.LoadLocation(tz)
			if loc == nil {
				loc = time.UTC
			}
			nextRun := cronExpr.Next(time.Now().In(loc)).UTC()
			if nextRun.IsZero() {
				continue
			}
			sched.NextRunAt = nextRun
		} else {
			slog.Warn("schedule directive: missing at or cron attribute")
			continue
		}

		id, err := b.store.SaveSchedule(sched)
		if err != nil {
			slog.Error("schedule directive: failed to save", "error", err)
			continue
		}

		// Append a confirmation to the cleaned output
		if sched.Type == "once" {
			cleaned.WriteString(fmt.Sprintf("\n\n📅 Scheduled #%d: %s at %s", id, msg, sched.NextRunAt.Format("2006-01-02 15:04 UTC")))
		} else {
			cleaned.WriteString(fmt.Sprintf("\n\n📅 Scheduled #%d: %s (%s) next: %s", id, msg, sched.Schedule, sched.NextRunAt.Format("2006-01-02 15:04 UTC")))
		}
	}

	cleaned.WriteString(response[lastEnd:])
	return strings.TrimSpace(cleaned.String())
}

// parseScheduleCron is a bridge-level wrapper that calls the scheduler's cron parser.
func parseScheduleCron(expr string) (interface{ Next(time.Time) time.Time }, error) {
	return schedulerParseCron(expr)
}

// schedulerParseCron is set by the daemon during initialization to avoid
// a direct import of the scheduler package from bridge.
var schedulerParseCron func(string) (interface{ Next(time.Time) time.Time }, error)

// SetCronParser sets the cron parsing function used by schedule commands.
func SetCronParser(fn func(string) (interface{ Next(time.Time) time.Time }, error)) {
	schedulerParseCron = fn
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

	response, err := b.HandleMessageStreaming(ctx, chatID, msgMap.UserMessage, "", nil, nil, onUpdate)
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

	// Discover available repos for repo-aware plan generation.
	var repoNames []string
	if b.repoDir != "" {
		repos := worktree.ListRepos(b.repoDir)
		for name := range repos {
			repoNames = append(repoNames, name)
		}
		sort.Strings(repoNames)
	}

	// Otherwise, draft a plan from the intent.
	draft, err := b.plan.DraftPlan(ctx, input, "", "", repoNames...)
	if err != nil {
		return "", fmt.Errorf("failed to generate plan: %w", err)
	}

	b.planMu.Lock()
	b.planRuns[chatID] = &planRun{
		state:     planStateDrafting,
		draftPlan: draft,
		intent:    input,
		startedAt: time.Now(),
		repoNames: repoNames,
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
		revised, err := b.plan.DraftPlan(ctx, run.intent, run.draftPlan, userMsg, run.repoNames...)
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
	failedRepo := run.failedRepo
	b.planMu.Unlock()

	normalized := strings.TrimSpace(strings.ToLower(userMsg))
	if normalized == "stop" {
		b.planMu.Lock()
		delete(b.planRuns, chatID)
		b.planMu.Unlock()
		return "Plan cancelled.", nil
	}

	// Determine the failed task and the correct planner.
	var failedTask string
	var tasks []string // flat task list for legacy context building

	repoTasks := planner.ParsePlanByRepo(planText)
	isMultiRepo := len(repoTasks) > 0

	if isMultiRepo && failedIdx < len(repoTasks) {
		failedTask = repoTasks[failedIdx].Task
		for _, rt := range repoTasks {
			tasks = append(tasks, rt.Task)
		}
	} else {
		tasks = planner.ParsePlan(planText)
		if failedIdx < len(tasks) {
			failedTask = tasks[failedIdx]
		}
	}

	planCtx, cancel := context.WithCancel(context.Background())

	b.planMu.Lock()
	run.cancel = cancel
	run.state = planStateExecuting
	completedCtx := buildCompletedContext(tasks, run.results, failedIdx)

	// Route to the correct repo's planner for multi-repo plans.
	var execPlan *planner.Planner
	if isMultiRepo && failedRepo != "" {
		if rw, ok := run.repoWorktrees[failedRepo]; ok {
			execPlan = rw.planner
		}
	}
	if execPlan == nil {
		execPlan = run.execPlanner
	}
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

	b.runBackground(planCtx, fmt.Sprintf("plan-%d-retry", chatID), "plan retry with guidance",
		map[string]string{"type": "plan", "chat": fmt.Sprint(chatID)},
		func(ctx context.Context) error {
			defer cancel()
			progress(fmt.Sprintf("Retrying task %d/%d with guidance: %s", failedIdx+1, len(tasks), failedTask))

			result := execPlan.RunTaskWithGuidance(ctx, failedTask, userMsg, completedCtx, progress)

			b.planMu.Lock()
			// Replace the failed result with the new one.
			run.results[failedIdx] = result
			b.planMu.Unlock()

			if result.Verdict != planner.VerdictDone {
				b.planMu.Lock()
				run.state = planStateBlocked
				b.planMu.Unlock()
				if b.notify != nil {
					b.notify(chatID, b.formatPlanSummary(run))
				}
				return nil
			}

			execPlan.GitCheckpoint(ctx, failedTask)
			updatedCtx := completedCtx + fmt.Sprintf("- %s: %s\n", failedTask, result.Summary)

			// Continue with remaining tasks.
			if isMultiRepo && failedIdx+1 < len(repoTasks) {
				// Multi-repo: continue with remaining repo tasks.
				remaining := repoTasks[failedIdx+1:]
				b.executeMultiRepoFrom(ctx, chatID, run, remaining, updatedCtx, progress)
			} else if failedIdx+1 < len(tasks) {
				remaining := execPlan.RunPlanFrom(ctx, planText, failedIdx+1, updatedCtx, progress)

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
			} else {
				b.planMu.Lock()
				run.state = planStateDone
				run.done = true
				b.planMu.Unlock()
			}

			b.storeReviewerLearnings(ctx, run)
			b.cleanupWorktree(run, chatID)

			if b.notify != nil {
				b.notify(chatID, b.formatPlanSummary(run))
			}
			return nil
		})

	return fmt.Sprintf("Retrying task %d with your guidance. Use /planstatus to check progress.", failedIdx+1), nil
}

// startExecution transitions to executing and runs the plan in a background goroutine.
// intent is used to resolve which git repo to create a worktree from when the
// workspace contains multiple repositories.
func (b *Bridge) startExecution(ctx context.Context, chatID int64, planText, intent string) (string, error) {
	// Try repo-grouped parsing first; fall back to flat.
	repoTasks := planner.ParsePlanByRepo(planText)
	isMultiRepo := len(repoTasks) > 0

	if !isMultiRepo {
		flatTasks := planner.ParsePlan(planText)
		if len(flatTasks) == 0 {
			return "No tasks found in plan.", nil
		}
	}

	planCtx, cancel := context.WithCancel(context.Background())
	run := &planRun{
		cancel:        cancel,
		state:         planStateExecuting,
		draftPlan:     planText,
		intent:        intent,
		startedAt:     time.Now(),
		repoWorktrees: make(map[string]*repoWorktree),
	}

	if isMultiRepo && b.useWorktree && b.repoDir != "" {
		// Multi-repo: create one worktree per unique repo.
		availableRepos := worktree.ListRepos(b.repoDir)
		seen := map[string]bool{}
		for _, rt := range repoTasks {
			if seen[rt.Repo] {
				continue
			}
			seen[rt.Repo] = true

			repoPath, ok := availableRepos[rt.Repo]
			if !ok {
				// Try ResolveRepoDir as fallback
				resolved, err := worktree.ResolveRepoDir(b.repoDir, rt.Repo)
				if err != nil {
					slog.Warn("worktree: unknown repo in plan, skipping worktree", "repo", rt.Repo, "error", err)
					continue
				}
				repoPath = resolved
			}

			wtPath, branch, err := worktree.Create(repoPath, b.worktreeDir, chatID)
			if err != nil {
				slog.Warn("worktree: failed to create for repo", "repo", rt.Repo, "error", err)
				continue
			}

			pl := b.plan.CloneWithWorkDir(wtPath)
			pl = b.injectReviewerMemory(ctx, pl)

			run.repoWorktrees[rt.Repo] = &repoWorktree{
				repoDir: repoPath,
				path:    wtPath,
				branch:  branch,
				planner: pl,
			}
			slog.Info("plan execution using worktree", "chat_id", chatID, "repo", rt.Repo, "branch", branch, "path", wtPath)
		}
	} else if !isMultiRepo && b.useWorktree && b.repoDir != "" {
		// Legacy single-repo path
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
			execPlan := b.plan.CloneWithWorkDir(wtPath)
			execPlan = b.injectReviewerMemory(ctx, execPlan)
			run.execPlanner = execPlan
			slog.Info("plan execution using worktree", "chat_id", chatID, "repo", repoDir, "branch", branch, "path", wtPath)
		}
	}

	// Ensure legacy execPlanner is set for non-multi-repo plans.
	if run.execPlanner == nil && !isMultiRepo {
		execPlan := b.plan
		execPlan = b.injectReviewerMemory(ctx, execPlan)
		run.execPlanner = execPlan
	}

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

	if isMultiRepo {
		b.runBackground(planCtx, fmt.Sprintf("plan-%d", chatID), "plan execution (multi-repo)",
			map[string]string{"type": "plan", "chat": fmt.Sprint(chatID)},
			func(ctx context.Context) error {
				defer cancel()
				b.executeMultiRepo(ctx, chatID, run, repoTasks, progress)
				return nil
			})

		var branches []string
		for repo, rw := range run.repoWorktrees {
			branches = append(branches, fmt.Sprintf("%s: %s", repo, rw.branch))
		}
		sort.Strings(branches)
		extra := ""
		if len(branches) > 0 {
			extra = "\nWorktree branches:\n" + strings.Join(branches, "\n")
		}
		return fmt.Sprintf("Plan started with %d tasks across %d repos. Progress will be reported as tasks complete.\nUse /planstatus to check, /planstop to cancel.%s",
			len(repoTasks), len(run.repoWorktrees), extra), nil
	}

	// Legacy flat plan execution
	b.runBackground(planCtx, fmt.Sprintf("plan-%d", chatID), "plan execution",
		map[string]string{"type": "plan", "chat": fmt.Sprint(chatID)},
		func(ctx context.Context) error {
			defer cancel()
			results := run.execPlanner.RunPlan(ctx, planText, progress)

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

			b.storeReviewerLearnings(ctx, run)
			b.cleanupWorktree(run, chatID)

			if b.notify != nil {
				b.notify(chatID, b.formatPlanSummary(run))
			}
			return nil
		})

	extra := ""
	if run.worktreeBranch != "" {
		extra = fmt.Sprintf("\nWorktree branch: %s", run.worktreeBranch)
	}
	return fmt.Sprintf("Plan started with %d tasks. Progress will be reported as tasks complete.\nUse /planstatus to check, /planstop to cancel.%s", len(planner.ParsePlan(planText)), extra), nil
}

// executeMultiRepo runs repo-grouped tasks sequentially, routing each to the
// correct repo's planner. Stops on first needs_human.
func (b *Bridge) executeMultiRepo(ctx context.Context, chatID int64, run *planRun, repoTasks []planner.RepoTask, progress planner.ProgressFunc) {
	total := len(repoTasks)
	progress(fmt.Sprintf("Plan has %d tasks across repos.", total))
	var completedContext string

	for i, rt := range repoTasks {
		rw, ok := run.repoWorktrees[rt.Repo]
		if !ok {
			progress(fmt.Sprintf("Skipping task %d/%d (no worktree for repo %s): %s", i+1, total, rt.Repo, rt.Task))
			continue
		}

		progress(fmt.Sprintf("\n=== Task %d/%d [%s]: %s ===", i+1, total, rt.Repo, rt.Task))
		result := rw.planner.RunTask(ctx, rt.Task, completedContext, progress)

		b.planMu.Lock()
		run.results = append(run.results, result)
		b.planMu.Unlock()

		if result.Verdict != planner.VerdictDone {
			progress(fmt.Sprintf("Task %d stopped: %s", i+1, result.Verdict))
			b.planMu.Lock()
			run.state = planStateBlocked
			run.failedTaskIdx = i
			run.failedRepo = rt.Repo
			run.done = false
			b.planMu.Unlock()

			b.storeReviewerLearningsMultiRepo(ctx, run)

			if b.notify != nil {
				b.notify(chatID, b.formatPlanSummary(run))
			}
			return
		}

		rw.planner.GitCheckpoint(ctx, rt.Task)
		completedContext += fmt.Sprintf("- [%s] %s: %s\n", rt.Repo, rt.Task, result.Summary)
		progress(fmt.Sprintf("Task %d/%d: DONE", i+1, total))

		if i < total-1 {
			time.Sleep(3 * time.Second)
		}
	}

	b.planMu.Lock()
	run.state = planStateDone
	run.done = true
	b.planMu.Unlock()

	b.storeReviewerLearningsMultiRepo(ctx, run)
	b.cleanupWorktree(run, chatID)

	if b.notify != nil {
		b.notify(chatID, b.formatPlanSummary(run))
	}
}

// executeMultiRepoFrom continues multi-repo execution from a slice of remaining
// repo tasks, using the given completedContext. Called when resuming after a
// blocked task is resolved.
func (b *Bridge) executeMultiRepoFrom(ctx context.Context, chatID int64, run *planRun, remaining []planner.RepoTask, completedContext string, progress planner.ProgressFunc) {
	total := len(remaining)
	for i, rt := range remaining {
		rw, ok := run.repoWorktrees[rt.Repo]
		if !ok {
			progress(fmt.Sprintf("Skipping task (no worktree for repo %s): %s", rt.Repo, rt.Task))
			continue
		}

		progress(fmt.Sprintf("\n=== Continuing [%s]: %s ===", rt.Repo, rt.Task))
		result := rw.planner.RunTask(ctx, rt.Task, completedContext, progress)

		b.planMu.Lock()
		run.results = append(run.results, result)
		b.planMu.Unlock()

		if result.Verdict != planner.VerdictDone {
			b.planMu.Lock()
			run.state = planStateBlocked
			// Calculate absolute index from original repoTasks
			allRepoTasks := planner.ParsePlanByRepo(run.draftPlan)
			run.failedTaskIdx = len(allRepoTasks) - total + i
			run.failedRepo = rt.Repo
			run.done = false
			b.planMu.Unlock()
			return
		}

		rw.planner.GitCheckpoint(ctx, rt.Task)
		completedContext += fmt.Sprintf("- [%s] %s: %s\n", rt.Repo, rt.Task, result.Summary)

		if i < total-1 {
			time.Sleep(3 * time.Second)
		}
	}

	b.planMu.Lock()
	run.state = planStateDone
	run.done = true
	b.planMu.Unlock()
}

// storeReviewerLearningsMultiRepo persists reviewer learnings from all repo planners.
func (b *Bridge) storeReviewerLearningsMultiRepo(ctx context.Context, run *planRun) {
	if b.memory == nil {
		return
	}
	for _, rw := range run.repoWorktrees {
		if rw.planner == nil {
			continue
		}
		ns := reviewerNamespace(rw.planner.WorkDir())
		for _, result := range run.results {
			for _, learning := range result.ReviewerLearnings {
				if err := b.memory.StoreReviewerLearning(ctx, ns, learning); err != nil {
					slog.Warn("failed to store reviewer learning", "ns", ns, "error", err)
				}
			}
		}
	}
}

// cleanupWorktree handles worktree lifecycle at the end of a plan.
// On success (all done): merge branch into main repo and remove worktree.
// On blocked/failure: remove worktree but keep the branch for inspection.
func (b *Bridge) cleanupWorktree(run *planRun, chatID int64) {
	selfModified := false

	// Multi-repo cleanup
	if len(run.repoWorktrees) > 0 {
		if run.state == planStateDone && run.done {
			for repo, rw := range run.repoWorktrees {
				if err := worktree.MergeAndCleanup(rw.repoDir, rw.path, rw.branch); err != nil {
					slog.Warn("worktree merge failed", "repo", repo, "branch", rw.branch, "error", err)
					run.progress = append(run.progress, fmt.Sprintf("Worktree merge failed for %s: %v\nBranch %s is still available for manual merge.", repo, err, rw.branch))
				} else {
					slog.Info("worktree merged and cleaned up", "repo", repo, "branch", rw.branch)
					if b.isSelfRepo(rw.repoDir) {
						selfModified = true
					}
				}
			}
		}
		// For blocked/stopped state, worktrees are cleaned up by PlanStop
		if selfModified {
			b.triggerSelfRestart(run, chatID)
		}
		return
	}

	// Legacy single-repo cleanup
	if run.worktreePath == "" {
		return
	}

	if run.state == planStateDone && run.done {
		if err := worktree.MergeAndCleanup(run.worktreeRepoDir, run.worktreePath, run.worktreeBranch); err != nil {
			slog.Warn("worktree merge failed", "branch", run.worktreeBranch, "error", err)
			run.progress = append(run.progress, fmt.Sprintf("Worktree merge failed: %v\nBranch %s is still available for manual merge.", err, run.worktreeBranch))
			return
		}
		slog.Info("worktree merged and cleaned up", "branch", run.worktreeBranch)
		if b.isSelfRepo(run.worktreeRepoDir) {
			selfModified = true
		}
	}

	if selfModified {
		b.triggerSelfRestart(run, chatID)
	}
}

// isSelfRepo checks if repoDir matches shell's own source directory.
func (b *Bridge) isSelfRepo(repoDir string) bool {
	if b.selfSourceDir == "" || repoDir == "" {
		return false
	}
	// Resolve symlinks for reliable comparison.
	selfReal, err1 := filepath.EvalSymlinks(b.selfSourceDir)
	repoReal, err2 := filepath.EvalSymlinks(repoDir)
	if err1 != nil || err2 != nil {
		return b.selfSourceDir == repoDir
	}
	return selfReal == repoReal
}

// triggerSelfRestart notifies the user and triggers a rebuild + restart.
func (b *Bridge) triggerSelfRestart(run *planRun, chatID int64) {
	if b.onSelfRestart == nil {
		return
	}
	slog.Info("self-modification detected after plan merge, scheduling rebuild+restart")
	run.progress = append(run.progress, "Changes affect shell itself — rebuilding and restarting...")
	// Give a short delay so the notification can be sent before restart.
	notify := b.notify
	go func() {
		time.Sleep(2 * time.Second)
		b.onSelfRestart()
		// If we get here, restart failed (exec replaces the process on success).
		msg := "Self-restart failed. Relay continues running with old code."
		slog.Error("self-restart: onSelfRestart returned (rebuild likely failed)")
		if notify != nil && chatID != 0 {
			notify(chatID, msg)
		}
	}()
}

// pdfPagePattern matches "/Type /Page" but not "/Type /Pages".
var pdfPagePattern = regexp.MustCompile(`/Type\s*/Page[^s]`)

// countPDFPages returns the number of pages in a PDF file by counting
// "/Type /Page" object references (excluding "/Type /Pages" which is the
// page tree root). Returns 0 if the file cannot be read or parsed.
func countPDFPages(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return len(pdfPagePattern.FindAllIndex(data, -1))
}

// formatFileSize returns a human-readable file size string.
func formatFileSize(bytes int64) string {
	switch {
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
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
	repoWTs := run.repoWorktrees
	b.planMu.Unlock()

	switch state {
	case planStateDrafting:
		return fmt.Sprintf("Plan: DRAFTING\n\n%s\n\nReply 'go' to execute, send edits to revise, or 'stop' to cancel.", draft), nil

	case planStateExecuting:
		var sb strings.Builder
		sb.WriteString("Plan: RUNNING\n")
		sb.WriteString(fmt.Sprintf("Elapsed: %s\n", elapsed))
		if len(repoWTs) > 0 {
			sb.WriteString("Worktree branches:\n")
			for repo, rw := range repoWTs {
				sb.WriteString(fmt.Sprintf("  %s: %s\n", repo, rw.branch))
			}
		} else if wtBranch != "" {
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

	// Clean up worktrees
	var branches []string
	for repo, rw := range run.repoWorktrees {
		worktree.Cleanup(rw.repoDir, rw.path, rw.branch)
		branches = append(branches, fmt.Sprintf("%s: %s", repo, rw.branch))
	}
	if run.worktreePath != "" {
		worktree.Cleanup(run.worktreeRepoDir, run.worktreePath, run.worktreeBranch)
		branches = append(branches, run.worktreeBranch)
	}

	delete(b.planRuns, chatID)
	b.planMu.Unlock()

	suffix := ""
	if len(branches) > 0 {
		sort.Strings(branches)
		suffix = fmt.Sprintf("\nWorktrees removed. Branches kept for inspection:\n%s", strings.Join(branches, "\n"))
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

	// Determine task list — multi-repo or flat.
	repoTasks := planner.ParsePlanByRepo(planText)
	isMultiRepo := len(repoTasks) > 0

	var tasks []string
	if isMultiRepo {
		for _, rt := range repoTasks {
			tasks = append(tasks, rt.Task)
		}
	} else {
		tasks = planner.ParsePlan(planText)
	}

	if failedIdx+1 >= len(tasks) {
		run.state = planStateDone
		run.done = true
		b.planMu.Unlock()
		b.cleanupWorktree(run, chatID)
		return "Skipped last task. Plan complete.", nil
	}

	planCtx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	run.state = planStateExecuting
	completedCtx := buildCompletedContext(tasks, run.results, failedIdx)

	// Resolve exec planner for non-multi-repo.
	var execPlan *planner.Planner
	if !isMultiRepo {
		execPlan = run.execPlanner
		if execPlan == nil {
			execPlan = b.plan
		}
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

	b.runBackground(planCtx, fmt.Sprintf("plan-%d-skip", chatID), "plan skip and continue",
		map[string]string{"type": "plan", "chat": fmt.Sprint(chatID)},
		func(ctx context.Context) error {
			defer cancel()
			progress(fmt.Sprintf("Skipped task %d, continuing from task %d.", failedIdx+1, failedIdx+2))

			if isMultiRepo {
				remaining := repoTasks[failedIdx+1:]
				b.executeMultiRepoFrom(ctx, chatID, run, remaining, completedCtx, progress)

				// Check if executeMultiRepoFrom left us in done state.
				b.planMu.Lock()
				if run.state != planStateBlocked {
					run.state = planStateDone
					run.done = true
				}
				b.planMu.Unlock()
			} else {
				remaining := execPlan.RunPlanFrom(ctx, planText, failedIdx+1, completedCtx, progress)

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
			}

			b.cleanupWorktree(run, chatID)

			if b.notify != nil {
				b.notify(chatID, b.formatPlanSummary(run))
			}
			return nil
		})

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

	// Auto-create default heartbeat for new chats if scheduler + memory are enabled
	if b.schedulerEnabled && b.memory != nil {
		b.ensureDefaultHeartbeat(chatID)
	}

	// Re-read to get the DB-assigned ID
	return b.store.GetSession(chatID)
}

// defaultHeartbeatInterval is the interval for auto-created heartbeats.
const defaultHeartbeatInterval = "1h"

// defaultHeartbeatMessage is the prompt for auto-created heartbeats.
const defaultHeartbeatMessage = "Review recent activity and check for anything that needs attention."

// EnsureDefaultHeartbeats creates default heartbeats for all active sessions
// that don't already have one. Called at daemon startup.
func (b *Bridge) EnsureDefaultHeartbeats() {
	if !b.schedulerEnabled || b.memory == nil {
		return
	}
	sessions, err := b.store.ListActiveSessions()
	if err != nil {
		slog.Warn("failed to list sessions for default heartbeats", "error", err)
		return
	}
	for _, sess := range sessions {
		b.ensureDefaultHeartbeat(sess.ChatID)
	}
}

// ensureDefaultHeartbeat creates a default 1-hour heartbeat for a chat if none exists.
func (b *Bridge) ensureDefaultHeartbeat(chatID int64) {
	hb, err := b.store.GetHeartbeat(chatID)
	if err != nil {
		slog.Warn("failed to check for existing heartbeat", "chat_id", chatID, "error", err)
		return
	}
	if hb != nil {
		return // already has a heartbeat
	}

	interval, _ := time.ParseDuration(defaultHeartbeatInterval)
	nextRun := time.Now().Add(interval).UTC()

	sched := &store.Schedule{
		ChatID:    chatID,
		Label:     "Heartbeat: " + defaultHeartbeatMessage,
		Message:   defaultHeartbeatMessage,
		Schedule:  defaultHeartbeatInterval,
		Timezone:  b.schedulerTZ,
		Type:      "heartbeat",
		Mode:      "prompt",
		NextRunAt: nextRun,
		Enabled:   true,
	}

	id, err := b.store.SaveSchedule(sched)
	if err != nil {
		slog.Warn("failed to create default heartbeat", "chat_id", chatID, "error", err)
		return
	}
	slog.Info("default heartbeat created", "chat_id", chatID, "id", id, "interval", defaultHeartbeatInterval)
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
