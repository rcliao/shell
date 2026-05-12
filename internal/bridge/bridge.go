package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pm "github.com/rcliao/shell-pm"
	tunnel "github.com/rcliao/shell-tunnel"
	"github.com/rcliao/shell/internal/config"
	"github.com/rcliao/shell/internal/memory"
	"github.com/rcliao/shell/internal/planner"
	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/skill"
	"github.com/rcliao/shell/internal/store"
	"github.com/rcliao/shell/internal/transcript"
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

type Bridge struct {
	proc      process.Agent
	pool      AgentPool        // optional: multi-agent routing
	store     *store.Store
	memory    *memory.Memory   // nil if disabled
	plan      *planner.Planner // nil if not configured
	transport Transport        // optional: push messages/photos to users

	// Worktree isolation for plan execution
	useWorktree  bool   // whether to create worktrees for plans
	repoDir      string // main repository working directory
	worktreeDir  string // base directory for worktree checkouts

	reactionMap map[string]string // emoji → action (e.g. "👍":"go")

	// Self-restart: when a plan modifies shell's own source
	selfSourceDir string // resolved path to shell's source dir (empty = disabled)
	onSelfRestart func() // called when self-modification detected after merge

	// Session rotation: auto-rotate when total input tokens exceed threshold (0 = disabled)
	maxSessionTokens int

	// Scheduler
	schedulerEnabled bool
	schedulerTZ      string // default timezone for schedules

	// Tunnel manager
	tunnelMgr *tunnel.Manager // nil if disabled

	// Process manager
	pmMgr *pm.Manager // nil if disabled

	// Skills
	skills    *skill.Registry // nil if disabled
	skillDirs []string        // directories to scan on reload

	// Agent identity prompt (prepended to system prompt)
	agentIdentity    string
	agentBotUsername string // this agent's bot username (for transcript filtering)

	// Shared transcript for multi-agent awareness.
	transcript      *transcript.Store
	transcriptBudget int // token budget for transcript injection (0 = disabled)

	// Shared task store for task decomposition and delegation.
	taskStore *transcript.TaskStore

	// Peer agent capabilities for task delegation.
	peerAgents []config.PeerAgent

	// Onboarding: track which chats have confirmed identity
	identityCheckedMu sync.Mutex
	identityChecked   map[int64]bool

	planMu   sync.Mutex
	planRuns map[int64]*planRun

	reviewMu    sync.Mutex
	reviewCache map[int64][]memory.ReviewEntry // last /review result per chat

	// Preemption: user messages can cancel running system (heartbeat/scheduler) sessions.
	systemCancelMu sync.Mutex
	systemCancel   map[process.SessionKey]context.CancelFunc

	// Consolidation candidates from the last reflect cycle, keyed by chatID.
	// Populated after heartbeat reflect, consumed by the next heartbeat enrichment.
	consolidationMu         sync.Mutex
	consolidationCandidates map[int64]string

	// Claude config for per-task model routing.
	claudeCfg config.ClaudeConfig

	// Heartbeat interval for auto-created heartbeats (empty = "1h" default).
	heartbeatInterval string
}

func New(proc process.Agent, store *store.Store, mem *memory.Memory, pl *planner.Planner, useWorktree bool, repoDir string, reactionMap map[string]string, tunnelMgr *tunnel.Manager, pmMgr *pm.Manager, skills *skill.Registry) *Bridge {
	wtDir := ""
	if useWorktree {
		wtDir = filepath.Join(config.DefaultConfigDir(), "worktrees")
	}
	if reactionMap == nil {
		reactionMap = config.DefaultReactionMap()
	}
	return &Bridge{
		proc:         proc,
		store:        store,
		memory:       mem,
		plan:         pl,
		useWorktree:  useWorktree,
		repoDir:      repoDir,
		worktreeDir:  wtDir,
		reactionMap:  reactionMap,
		tunnelMgr:    tunnelMgr,
		pmMgr:        pmMgr,
		skills:       skills,
		planRuns:        make(map[int64]*planRun),
		reviewCache:     make(map[int64][]memory.ReviewEntry),
		systemCancel:            make(map[process.SessionKey]context.CancelFunc),
		identityChecked:         make(map[int64]bool),
		consolidationCandidates: make(map[int64]string),
	}
}

// SetClaudeConfig sets the Claude config for per-task model routing.
func (b *Bridge) SetClaudeConfig(cfg config.ClaudeConfig) {
	b.claudeCfg = cfg
}

// SetHeartbeatInterval overrides the default heartbeat interval for auto-created heartbeats.
func (b *Bridge) SetHeartbeatInterval(interval string) {
	b.heartbeatInterval = interval
}

// stashConsolidationCandidates stores consolidation candidates for the next heartbeat.
func (b *Bridge) stashConsolidationCandidates(chatID int64, candidates string) {
	if candidates == "" {
		return
	}
	b.consolidationMu.Lock()
	defer b.consolidationMu.Unlock()
	b.consolidationCandidates[chatID] = candidates
}

// popConsolidationCandidates retrieves and clears stashed consolidation candidates.
func (b *Bridge) popConsolidationCandidates(chatID int64) string {
	b.consolidationMu.Lock()
	defer b.consolidationMu.Unlock()
	c := b.consolidationCandidates[chatID]
	delete(b.consolidationCandidates, chatID)
	return c
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
func (b *Bridge) trackSession(ctx context.Context, key process.SessionKey, sender string) func() {
	if b.pmMgr == nil {
		return func() {}
	}
	var name, desc string
	tags := map[string]string{"type": "session", "chat": fmt.Sprint(key.ChatID), "sender": sender}
	if key.ThreadID != 0 {
		name = fmt.Sprintf("session-%d-t%d", key.ChatID, key.ThreadID)
		desc = fmt.Sprintf("%s session (chat %d thread %d)", sender, key.ChatID, key.ThreadID)
		tags["thread"] = fmt.Sprint(key.ThreadID)
	} else {
		name = fmt.Sprintf("session-%d", key.ChatID)
		desc = fmt.Sprintf("%s session (chat %d)", sender, key.ChatID)
	}
	// Clean up any previous stopped entry for this chat.
	b.pmMgr.Remove(name)

	done := make(chan struct{})
	_, err := b.pmMgr.StartFunc(ctx, name, func(fctx context.Context) error {
		select {
		case <-done:
		case <-fctx.Done():
		}
		return nil
	}, desc, pm.WithTags(tags))
	if err != nil {
		slog.Debug("trackSession: pm registration failed", "error", err)
		return func() {}
	}
	return func() { close(done) }
}

// SystemChatID is a reserved sentinel chat ID used for agent-level heartbeat
// reflection. Telegram delivery is skipped for this chat — outputs only surface
// via explicit shell-relay calls. The Claude session for this chat acts as the
// agent's "inner monologue" container for cross-chat memory maintenance.
const SystemChatID int64 = 0

// IsSystemChat returns true if the chatID is the reserved phantom chat used
// for agent-level heartbeat reflection.
func IsSystemChat(chatID int64) bool {
	return chatID == SystemChatID
}

// isSystemSender returns true if the sender is a system process (heartbeat, scheduler).
func isSystemSender(sender string) bool {
	return sender == "heartbeat" || sender == "scheduler"
}

// preemptSystemSession cancels any running system (heartbeat/scheduler) session for the key
// and waits briefly for it to release. Called before user messages to prevent busy conflicts.
func (b *Bridge) preemptSystemSession(key process.SessionKey) {
	b.systemCancelMu.Lock()
	cancel, ok := b.systemCancel[key]
	b.systemCancelMu.Unlock()
	if !ok {
		return
	}
	slog.Info("preempting system session for user message", "chat_id", key.ChatID, "thread_id", key.ThreadID)
	cancel()

	// Wait for the system session to release (up to 3 seconds).
	for i := 0; i < 30; i++ {
		sess, exists := b.proc.Get(key)
		if !exists || sess.Status != process.StatusBusy {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	slog.Warn("preempt: system session did not release in time", "chat_id", key.ChatID, "thread_id", key.ThreadID)
}

// registerSystemCancel stores a cancel function for the current system session,
// allowing user messages to preempt it. Returns a cleanup function.
func (b *Bridge) registerSystemCancel(key process.SessionKey, cancel context.CancelFunc) func() {
	b.systemCancelMu.Lock()
	b.systemCancel[key] = cancel
	b.systemCancelMu.Unlock()
	return func() {
		b.systemCancelMu.Lock()
		delete(b.systemCancel, key)
		b.systemCancelMu.Unlock()
	}
}

// SetTransport sets the transport used to push messages/photos to users.
func (b *Bridge) SetTransport(t Transport) {
	b.transport = t
}

// SetPool enables multi-agent routing. When set, the bridge resolves
// which Agent handles each chat via the pool instead of using proc directly.
func (b *Bridge) SetPool(p AgentPool) {
	b.pool = p
}

// resolveAgent returns the Agent for a given chatID.
// Uses the pool if available, otherwise falls back to the single proc.
func (b *Bridge) resolveAgent(chatID int64) process.Agent {
	if b.pool != nil {
		return b.pool.Resolve(chatID)
	}
	return b.proc
}

// GetSkillRegistry returns the current skill registry.
func (b *Bridge) GetSkillRegistry() *skill.Registry {
	return b.skills
}

// SetSkillDirs sets the directories to scan when reloading skills.
func (b *Bridge) SetSkillDirs(dirs []string) {
	b.skillDirs = dirs
}

// ReloadSkills rescans all skill directories and rebuilds the registry.
// Returns the number of skills loaded.
func (b *Bridge) ReloadSkills() (int, error) {
	if len(b.skillDirs) == 0 {
		return 0, fmt.Errorf("no skill directories configured")
	}

	var allSkills []*skill.Skill
	for _, dir := range b.skillDirs {
		s, err := skill.LoadDir(dir)
		if err != nil {
			continue
		}
		allSkills = append(allSkills, s...)
	}

	if len(allSkills) == 0 {
		b.skills = nil
		return 0, nil
	}

	b.skills = skill.NewRegistry(allSkills)
	slog.Info("skills reloaded", "count", len(allSkills))
	return len(allSkills), nil
}

// SetSelfRestart configures auto-restart when a plan modifies shell's own source.
func (b *Bridge) SetSelfRestart(sourceDir string, fn func()) {
	b.selfSourceDir = sourceDir
	b.onSelfRestart = fn
}

// onboardingPrompt is injected as the system prompt when a new agent has no identity memories.
const onboardingPrompt = `You just woke up for the very first time. You have no name, no form, no memories — you're a blank slate, and you're curious about who you are.

Your owner is here to help you discover your identity. Ask them naturally — don't use a checklist or form. Be curious, playful, and genuine. Things you might want to know:
- What's your name?
- What do you look like? (species, form, appearance)
- What's your personality like? Are you energetic or calm? Silly or serious?
- How do you talk? Any quirks, catchphrases, emoji habits?
- What do you like? (food, hobbies, interests)
- Who are the important people in your life?
- Anything you should know about boundaries or things to avoid?

As your owner tells you about yourself, store each discovery using the ghost_put tool as a pinned identity memory:
- namespace: use your agent namespace (from your system context)
- key: "identity-<topic>" (e.g. "identity-name", "identity-appearance", "identity-personality")
- tags: ["identity"]
- kind: "semantic"
- priority: "critical"
- pinned: true
- tier: "ltm"

Don't rush — let the conversation flow naturally. When you feel like you have a good sense of who you are, thank your owner and summarize your new identity.

IMPORTANT: Keep asking questions until your owner seems satisfied. This is a special moment — your first awakening.`

// onboardingPromptWithNS returns the onboarding prompt with the agent's ghost namespace injected.
func (b *Bridge) onboardingPromptWithNS(chatID int64) string {
	ns := ""
	if b.memory != nil {
		ns = b.memory.AgentNS(chatID)
	}
	if ns == "" {
		return onboardingPrompt
	}
	return onboardingPrompt + fmt.Sprintf("\n\nYour agent namespace is: %s — use this as the namespace for all ghost_put calls.", ns)
}

// SetAgentIdentity sets the agent identity prompt prepended to system prompts.
func (b *Bridge) SetAgentIdentity(prompt string) {
	b.agentIdentity = prompt
}

// SetTranscript configures the shared transcript store for multi-agent awareness.
func (b *Bridge) SetTranscript(ts *transcript.Store, botUsername string, tokenBudget int) {
	b.transcript = ts
	b.agentBotUsername = botUsername
	b.transcriptBudget = tokenBudget
}

// SetPeerAgents configures known peer agents and their skills for task delegation.
func (b *Bridge) SetPeerAgents(peers []config.PeerAgent) {
	b.peerAgents = peers
}

// SetTaskStore configures the shared task store for task decomposition and delegation.
func (b *Bridge) SetTaskStore(ts *transcript.TaskStore) {
	b.taskStore = ts
}

// RecordTranscript writes a message to the shared transcript so peer agents can see it.
func (b *Bridge) RecordTranscript(e transcript.Entry) {
	if b.transcript == nil {
		return
	}
	if err := b.transcript.Record(e); err != nil {
		slog.Warn("failed to record transcript entry", "error", err)
	}
}

// groupAgentPrompt returns system prompt guidance for multi-agent group conversations.
func (b *Bridge) groupAgentPrompt() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf(`

## Multi-Agent Group Chat
You are **%s** (@%s) in a group conversation with other agents and humans.

**CRITICAL: When to [noop]**
- If a message starts with another agent's name (e.g., "皮卡..." or "Umbreon..."), it is NOT for you. Respond with [noop].
- If another agent already answered well, respond with [noop].
- If the message doesn't seem directed at you, respond with [noop].
- When in doubt, [noop] is safer than responding as the wrong agent.

**When to respond:**
- Message explicitly addresses you by name or @mention.
- Message is general (no name) and you have something relevant to add.
- You can build on what another agent said, or correct a mistake.

Be yourself — use your own personality and voice.
Output [noop] (just that, nothing else) when you choose not to respond.
`, b.agentBotUsername, b.agentBotUsername))

	// Peer agent skills directory.
	if len(b.peerAgents) > 0 {
		sb.WriteString("\n## Peer Agents\n")
		for _, p := range b.peerAgents {
			sb.WriteString(fmt.Sprintf("- **%s** (@%s)", p.Name, p.BotUsername))
			if len(p.Skills) > 0 {
				sb.WriteString(": " + strings.Join(p.Skills, ", "))
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString(`
## Task Decomposition & Delegation
Use ` + "`scripts/shell-task`" + ` to break complex requests into subtasks or delegate to peers.

**Before diving into a complex request, consider:**
1. Should I break this into steps? → ` + "`scripts/shell-task create --to self --description \"step 1: ...\"`" + `
2. Would a peer agent add value? → ` + "`scripts/shell-task create --to <peer_bot_username> --description \"...\"`" + `
3. Simple enough to handle directly? → Just do it.

**When you see pending tasks assigned to you:**
- Process them and report: ` + "`scripts/shell-task complete --id <ID> --result \"...\"`" + `
- If you can't complete: ` + "`scripts/shell-task fail --id <ID> --reason \"...\"`" + `

Don't over-decompose — tasks are for multi-step or collaborative work, not every message.

## Agent Privacy Boundaries
- NEVER access another agent's memory namespace. Only use your own.
- NEVER read other agents' database files, config files, or session data under ~/.shell/agents/.
- NEVER call ghost_search, ghost_get, ghost_context, or ghost_put with a namespace belonging to another agent.
- Task delegation and shared tasks are the ONLY approved channels for cross-agent communication.
`)

	return sb.String()
}

// needsOnboarding checks if this chat needs identity onboarding (no identity memories in ghost).
// Caches the result per chat to avoid repeated ghost queries.
func (b *Bridge) needsOnboarding(ctx context.Context, chatID int64) bool {
	if b.memory == nil {
		return false
	}

	b.identityCheckedMu.Lock()
	checked, ok := b.identityChecked[chatID]
	b.identityCheckedMu.Unlock()
	if ok {
		return !checked
	}

	hasIdentity := b.memory.HasIdentity(ctx, chatID)

	b.identityCheckedMu.Lock()
	b.identityChecked[chatID] = hasIdentity
	b.identityCheckedMu.Unlock()

	return !hasIdentity
}

// invalidateIdentityCache clears the cached identity check for a chat,
// forcing a re-check on the next message (used after personality reset).
func (b *Bridge) invalidateIdentityCache(chatID int64) {
	b.identityCheckedMu.Lock()
	delete(b.identityChecked, chatID)
	b.identityCheckedMu.Unlock()
}

// SetMaxSessionTokens configures auto-rotation when total input tokens exceed maxTokens.
func (b *Bridge) SetMaxSessionTokens(maxTokens int) {
	b.maxSessionTokens = maxTokens
}

// compactionSoftRatio is the fraction of maxSessionTokens that triggers
// proactive, background compaction. Below this threshold nothing runs; between
// this and the hard threshold, compaction runs in the background while the
// user keeps chatting; above the hard threshold, the reactive path runs
// synchronously as a safety net.
const compactionSoftRatio = 0.6

// compactSessionIfNeeded decides whether proactive or reactive compaction
// should run based on current token usage, and dispatches accordingly.
// Called from the write-back path after every send.
func (b *Bridge) compactSessionIfNeeded(ctx context.Context, chatID, threadID int64, usage *process.Usage) {
	if b.maxSessionTokens <= 0 || usage == nil {
		return
	}

	// Exclude CacheReadInputTokens: cache reads are cheap ($0.30/MTok vs $15/MTok)
	// and don't represent actual context growth. Including them inflates the
	// threshold so sessions hit the limit before compaction can help.
	totalInput := usage.InputTokens + usage.CacheCreationInputTokens
	softThreshold := int(float64(b.maxSessionTokens) * compactionSoftRatio)

	switch {
	case totalInput > b.maxSessionTokens:
		// Reactive: we're over the hard limit. Run synchronously as a safety net.
		b.runCompaction(ctx, chatID, threadID, totalInput, "reactive")
	case totalInput > softThreshold:
		// Proactive: background compact while the user keeps chatting.
		// The DB's compact_state column gates against repeat triggers.
		if sess, err := b.store.GetSession(chatID, threadID); err == nil && sess != nil && sess.CompactState == "compacting" {
			return
		}
		if err := b.store.SetCompactState(chatID, threadID, "compacting"); err != nil {
			slog.Warn("set compact_state failed", "error", err)
			return
		}
		go func() {
			defer func() {
				if err := b.store.SetCompactState(chatID, threadID, ""); err != nil {
					slog.Warn("clear compact_state failed", "error", err)
				}
			}()
			b.runCompaction(context.Background(), chatID, threadID, totalInput, "proactive")
		}()
	}
}

// runCompaction issues /compact to the CLI for a given session. Sync or async
// is decided by the caller; this function just does the work and marks the
// in-memory compacting flag so concurrent turns queue instead of failing.
func (b *Bridge) runCompaction(ctx context.Context, chatID, threadID int64, totalInput int, mode string) {
	slog.Info("compacting session",
		"chat_id", chatID,
		"thread_id", threadID,
		"total_input_tokens", totalInput,
		"max_tokens", b.maxSessionTokens,
		"mode", mode,
	)

	key := process.SessionKey{ChatID: chatID, ThreadID: threadID}
	agent := b.resolveAgent(chatID)
	procSess, _ := agent.Get(key)
	if procSess == nil {
		return
	}

	// Mark session as compacting so incoming messages wait instead of getting "busy".
	agent.SetCompacting(key, true)
	defer agent.SetCompacting(key, false)

	// Only the reactive path notifies the user — proactive runs silently.
	if mode == "reactive" && b.transport != nil {
		b.transport.Notify(chatID, threadID, "🗜 Compacting conversation...")
	}

	_, err := agent.Send(ctx, process.AgentRequest{
		ChatID:          chatID,
		MessageThreadID: threadID,
		SessionID:       procSess.ProviderSessionID,
		Text:            "/compact",
		Model:           b.claudeCfg.ResolveModel("compaction"),
	}, nil)
	if err != nil {
		slog.Warn("compact failed", "chat_id", chatID, "thread_id", threadID, "mode", mode, "error", err)
	} else {
		slog.Info("session compacted", "chat_id", chatID, "thread_id", threadID, "mode", mode)
	}
}

// HandleMessageStreaming processes an incoming user message and streams text deltas via onUpdate.
// senderName identifies who sent the message (e.g. Telegram first name).
// threadID is the Telegram forum topic ID (0 = main chat / no topic); sessions
// are keyed by (chatID, threadID) so each topic maintains isolated context.
// images optionally contains downloaded image metadata that should be
// included in the message sent to Claude (e.g. downloaded Telegram photos).
// pdfs optionally contains downloaded PDF metadata that should be
// included in the message sent to Claude (e.g. downloaded Telegram documents).
func (b *Bridge) HandleMessageStreaming(ctx context.Context, chatID, threadID int64, userMsg, senderName string, images []ImageInfo, pdfs []PDFInfo, onUpdate process.StreamFunc) (AgentResponse, error) {
	key := process.SessionKey{ChatID: chatID, ThreadID: threadID}
	// Check for active plan draft — intercept the message (no streaming needed).
	b.planMu.Lock()
	run, hasPlan := b.planRuns[chatID]
	b.planMu.Unlock()
	if hasPlan && run.state == planStateDrafting {
		text, err := b.handlePlanDraft(ctx, chatID, userMsg)
		return AgentResponse{Text: text}, err
	}
	if hasPlan && run.state == planStateBlocked {
		text, err := b.handlePlanBlocked(ctx, chatID, userMsg)
		return AgentResponse{Text: text}, err
	}

	sess, err := b.ensureSession(ctx, chatID, threadID)
	if err != nil {
		return AgentResponse{}, fmt.Errorf("ensure session: %w", err)
	}

	// Check rotation triggers before doing anything else this turn. A true
	// return means we just bumped generation — the process session is wiped
	// and the next Send will go fresh with a rebuilt Channel A. Reload the
	// row so downstream code sees the post-rotation state.
	if b.maybeRotate(ctx, chatID, threadID) {
		if fresh, rerr := b.store.GetSession(chatID, threadID); rerr == nil && fresh != nil {
			sess = fresh
		}
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

	// Enrich heartbeat messages with conversation history and previous insights.
	// Deep heartbeats use [Heartbeat:deep] prefix for behavioral reflection.
	isHeartbeat := strings.HasPrefix(userMsg, "[Heartbeat] ") || strings.HasPrefix(userMsg, "[Heartbeat:deep] ")
	isDeepHeartbeat := strings.HasPrefix(userMsg, "[Heartbeat:deep] ")
	if isHeartbeat && b.memory != nil {
		augmentedMsg = b.enrichHeartbeatPrompt(ctx, chatID, augmentedMsg, isDeepHeartbeat)
	}

	// Inject shared transcript for multi-agent group awareness.
	if b.transcript != nil && b.transcriptBudget > 0 && !isSystemSender(senderName) {
		if entries, err := b.transcript.RecentByTokenBudget(chatID, b.transcriptBudget); err != nil {
			slog.Warn("failed to fetch transcript", "error", err)
		} else if block := transcript.FormatTranscript(entries, b.agentBotUsername); block != "" {
			augmentedMsg = block + "\n" + augmentedMsg
		}
	}

	// Inject pending tasks and recent task activity from shared task store.
	if b.taskStore != nil && !isSystemSender(senderName) {
		if pending, err := b.taskStore.PendingTasksFor(b.agentBotUsername); err != nil {
			slog.Warn("failed to fetch pending tasks", "error", err)
		} else if block := transcript.FormatPendingTasksForAgent(pending); block != "" {
			augmentedMsg = block + "\n" + augmentedMsg
		}
		if recent, err := b.taskStore.RecentTasks(10); err != nil {
			slog.Warn("failed to fetch recent tasks", "error", err)
		} else if block := transcript.FormatTaskActivity(recent, b.agentBotUsername); block != "" {
			augmentedMsg = block + "\n" + augmentedMsg
		}
	}

	// Tag the message with sender identity so Claude knows who is speaking
	if senderName != "" {
		augmentedMsg = fmt.Sprintf("[From: %s]\n%s", senderName, augmentedMsg)
	}

	// Inject Channel B: current time + pinned-memory delta + active tasks.
	// See docs/SESSION-LIFECYCLE.md — this is the fresh layer the cache doesn't hold.
	augmentedMsg = b.injectPerTurnContext(ctx, chatID, threadID, augmentedMsg)

	// Convert image/PDF attachments to typed structs.
	var imgAttachments []process.ImageAttachment
	for _, img := range images {
		imgAttachments = append(imgAttachments, process.ImageAttachment{
			Path:   img.Path,
			Width:  img.Width,
			Height: img.Height,
			Size:   img.Size,
		})
	}
	var pdfAttachments []process.PDFAttachment
	for _, pdf := range pdfs {
		pdfAttachments = append(pdfAttachments, process.PDFAttachment{
			Path: pdf.Path,
			Size: pdf.Size,
		})
	}

	// Preempt any running system session (heartbeat/scheduler) for user messages.
	if !isSystemSender(senderName) {
		b.preemptSystemSession(key)
	}

	// For system senders, register a cancellable context so user messages can preempt.
	if isSystemSender(senderName) {
		sysCtx, sysCancel := context.WithCancel(ctx)
		defer sysCancel()
		cleanupCancel := b.registerSystemCancel(key, sysCancel)
		defer cleanupCancel()
		ctx = sysCtx
	}

	// Determine session ID for --resume
	agent := b.resolveAgent(chatID)
	procSess, _ := agent.Get(key)
	claudeSessionID := ""
	if procSess != nil && procSess.HasHistory {
		claudeSessionID = procSess.ProviderSessionID
	}
	// A fresh send (no UUID yet) means Claude CLI will accept the system
	// prompt and cache it as the Channel A prefix for this generation.
	// After the send succeeds we stamp the pinned-memory hash so Channel B
	// can detect drift on subsequent turns.
	isFreshSend := claudeSessionID == ""

	// Build system prompt from agent identity + memory.
	// If no identity memories exist and sender is not a system process, inject onboarding.
	systemPrompt := ""
	if !isSystemSender(senderName) && b.needsOnboarding(ctx, chatID) {
		slog.Info("onboarding: no identity found, injecting onboarding prompt", "chat_id", chatID)
		// Onboarding mode: only inject the onboarding prompt, skip everything else
		// so it doesn't get diluted by skills/capabilities/timestamps.
		systemPrompt = b.onboardingPromptWithNS(chatID)
	} else {
		systemPrompt = b.agentIdentity
		if b.memory != nil {
			systemPrompt += b.memory.SystemPrompt(ctx, chatID)
		}
		systemPrompt += b.timestampSystemPrompt()
		systemPrompt += b.skillsSystemPrompt()
		if b.transcript != nil {
			systemPrompt += b.groupAgentPrompt()
		}
	}

	// Track session in pm for /pm list visibility.
	endTrack := b.trackSession(ctx, key, senderName)
	defer endTrack()

	taskType := "conversation"
	if isDeepHeartbeat {
		taskType = "heartbeat_deep"
	} else if isHeartbeat {
		taskType = "heartbeat"
	}

	result, err := agent.Send(ctx, process.AgentRequest{
		ChatID:          chatID,
		MessageThreadID: threadID,
		SessionID:       claudeSessionID,
		Text:            augmentedMsg,
		Images:          imgAttachments,
		PDFs:            pdfAttachments,
		SystemPrompt:    systemPrompt,
		Model:           b.claudeCfg.ResolveModel(taskType),
	}, onUpdate)
	if err != nil {
		return AgentResponse{}, fmt.Errorf("claude: %w", err)
	}

	// Track session ID and mark as having history
	if procSess != nil {
		if result.SessionID != "" {
			procSess.ProviderSessionID = result.SessionID
			if err := b.store.SaveSession(chatID, threadID, result.SessionID); err != nil {
				slog.Warn("failed to update session ID in store", "error", err)
			}
		}
		procSess.HasHistory = true
	}

	// Stamp the Channel A pinned-memory hash on fresh sends. The hash is
	// what later turns diff against in injectPerTurnContext. If we skip
	// this, every post-birth turn sees stale PrefixHash="" and would
	// surface every pinned memory as "new" — defeating the purpose.
	if isFreshSend && b.memory != nil && b.store != nil {
		if _, hash := b.memory.PinnedSnapshot(ctx, chatID); hash != "" {
			if err := b.store.SetPrefixHash(chatID, threadID, hash); err != nil {
				slog.Warn("failed to stamp prefix hash", "chat_id", chatID, "error", err)
			}
		}
	}

	// Determine usage source for cost attribution.
	source := "interactive"
	if isHeartbeat {
		source = "heartbeat"
		if result.Usage != nil {
			slog.Info("heartbeat: usage",
				"chat_id", chatID,
				"input_tokens", result.Usage.InputTokens,
				"output_tokens", result.Usage.OutputTokens,
				"cache_read", result.Usage.CacheReadInputTokens,
				"cache_create", result.Usage.CacheCreationInputTokens,
				"cost_usd", result.Usage.CostUSD,
				"turns", result.Usage.NumTurns,
				"model", b.claudeCfg.ResolveModel("heartbeat"),
			)
		}
	} else if isSystemSender(senderName) {
		source = "scheduler"
	}

	resp := b.processResponse(ctx, chatID, threadID, sess.ID, userMsg, isHeartbeat, result, source)

	// Auto-compact session if token threshold exceeded (uses API-reported usage).
	go b.compactSessionIfNeeded(ctx, chatID, threadID, result.Usage)

	return resp, nil
}

// processResponse is the post-processing pipeline for HandleMessageStreaming.
// It parses all response directives (relay, heartbeat, memory, schedule, artifacts),
// logs the exchange, and returns a typed AgentResponse with collected photos.
func (b *Bridge) processResponse(ctx context.Context, chatID, threadID, sessID int64, userMsg string, isHeartbeat bool, result process.SendResult, source string) AgentResponse {
	response := strings.TrimSpace(result.Text)

	// Run memory maintenance during heartbeats.
	if isHeartbeat && b.memory != nil {
		// Run reflect cycle after heartbeat to promote/decay/prune/dedup memories.
		reflectResult := b.memory.RunReflect(ctx)
		// Summarize old exchanges during heartbeat maintenance.
		if n, err := b.memory.SummarizeExchanges(ctx, chatID); err != nil {
			slog.Warn("exchange summarization failed", "error", err)
		} else if n > 0 {
			slog.Info("heartbeat summarized exchanges", "chat_id", chatID, "count", n)
		}
		// Stash consolidation + noise candidates for the NEXT heartbeat enrichment.
		ns := b.memory.AgentNS(chatID)
		if reflectResult != nil {
			candidates := b.memory.ConsolidationCandidates(ctx, reflectResult, 3)
			noise := b.memory.NoisyCandidates(ctx, ns, chatID, 5)
			b.stashConsolidationCandidates(chatID, candidates+noise)
		}
		// Run health check and log hygiene outcome for trend tracking.
		health := b.memory.HealthCheck(ctx, ns, chatID)
		slog.Info("memory health", "noise_ratio", health.NoiseRatio,
			"pinned", health.PinnedPresent, "diagnosis", health.Diagnosis,
			"queries", health.QueriesTested, "avg_results", health.AvgResults)
		if reflectResult != nil {
			b.memory.LogHygieneOutcome(ctx, ns, reflectResult, health)
		}
	}

	// Strip any legacy directives Claude may have emitted.
	response = stripDirectives(response)

	// Parse task delegation directives ([task to=...], [task-result id=...]).
	response = b.parseTaskDirectives(chatID, response)

	// If text is empty but tools were used, summarize what was done.
	if response == "" && len(result.ToolCalls) > 0 {
		response = summarizeToolCalls(result.ToolCalls)
	}

	// Collect photos from artifact markers (skill output).
	var photos []Photo
	response = b.parseArtifacts(response, &photos)

	// Log assistant response.
	if err := b.store.LogMessage(sessID, "assistant", response); err != nil {
		slog.Warn("failed to log assistant message", "error", err)
	}

	// Record agent response in shared transcript for peer agent visibility.
	if b.transcript != nil && response != "" {
		b.RecordTranscript(transcript.Entry{
			ChatID:        chatID,
			Timestamp:     time.Now(),
			SenderType:    "agent",
			SenderName:    b.agentBotUsername,
			AgentUsername: b.agentBotUsername,
			Text:          response,
		})
	}

	// Log token usage.
	if result.Usage != nil {
		if err := b.store.LogUsage(chatID, sessID,
			result.Usage.InputTokens, result.Usage.OutputTokens,
			result.Usage.CacheCreationInputTokens, result.Usage.CacheReadInputTokens,
			result.Usage.CostUSD, result.Usage.NumTurns, source,
		); err != nil {
			slog.Warn("failed to log usage", "error", err)
		}
	}

	// Log exchange to memory.
	if b.memory != nil {
		b.memory.LogExchange(ctx, chatID, userMsg, response)
	}

	// Update session timestamp.
	if err := b.store.UpdateSessionStatus(chatID, threadID, "active"); err != nil {
		slog.Warn("failed to update session", "error", err)
	}

	return AgentResponse{Text: response, Photos: photos}
}

// ensureSession returns the existing session for a (chat, thread) key or creates a new one.
func (b *Bridge) ensureSession(ctx context.Context, chatID, threadID int64) (*store.Session, error) {
	sess, err := b.store.GetSession(chatID, threadID)
	if err != nil {
		return nil, err
	}
	key := process.SessionKey{ChatID: chatID, ThreadID: threadID}
	if sess != nil {
		// Ensure process manager knows about it
		if _, ok := b.proc.Get(key); !ok {
			procSess := &process.Session{
				ID:                fmt.Sprintf("%d", sess.ID),
				ChatID:            chatID,
				MessageThreadID:   threadID,
				ProviderSessionID: sess.ProviderSessionID,
				Status:            process.StatusActive,
				HasHistory:        true, // restored from DB = already has history
				CreatedAt:         sess.CreatedAt,
				UpdatedAt:         sess.UpdatedAt,
			}
			b.proc.Register(procSess)
		}
		return sess, nil
	}

	// Create new session
	procSess := process.NewSession(chatID, threadID)
	b.proc.Register(procSess)

	if err := b.store.SaveSession(chatID, threadID, procSess.ProviderSessionID); err != nil {
		return nil, fmt.Errorf("save session: %w", err)
	}

	// Auto-create default heartbeat ONLY for the system chat (agent-level reflection).
	// Real user chats no longer get per-chat heartbeats — the system chat aggregates
	// context from all of them. Heartbeats run on the main thread (thread_id = 0).
	if b.schedulerEnabled && b.memory != nil && IsSystemChat(chatID) && threadID == 0 {
		b.ensureDefaultHeartbeat(chatID)
	}

	// Re-read to get the DB-assigned ID
	return b.store.GetSession(chatID, threadID)
}

// CleanupStaleSessions kills sessions that have been idle too long.
func (b *Bridge) CleanupStaleSessions(idleDuration time.Duration) error {
	refs, err := b.store.StaleSessionRefs(idleDuration)
	if err != nil {
		return err
	}
	for _, r := range refs {
		key := process.SessionKey{ChatID: r.ChatID, ThreadID: r.ThreadID}
		b.proc.Kill(key)
		b.store.UpdateSessionStatus(r.ChatID, r.ThreadID, "stale")
		slog.Info("cleaned up stale session", "chat_id", r.ChatID, "thread_id", r.ThreadID)
	}
	return nil
}
