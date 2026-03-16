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

	// Scheduler
	schedulerEnabled bool
	schedulerTZ      string // default timezone for schedules

	// Tunnel manager
	tunnelMgr *tunnel.Manager // nil if disabled

	// Process manager
	pmMgr *pm.Manager // nil if disabled

	// Skills
	skills *skill.Registry // nil if disabled

	planMu   sync.Mutex
	planRuns map[int64]*planRun

	reviewMu    sync.Mutex
	reviewCache map[int64][]memory.ReviewEntry // last /review result per chat

	// Preemption: user messages can cancel running system (heartbeat/scheduler) sessions.
	systemCancelMu sync.Mutex
	systemCancel   map[int64]context.CancelFunc
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
		planRuns:      make(map[int64]*planRun),
		reviewCache:   make(map[int64][]memory.ReviewEntry),
		systemCancel:  make(map[int64]context.CancelFunc),
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

// SetSelfRestart configures auto-restart when a plan modifies shell's own source.
func (b *Bridge) SetSelfRestart(sourceDir string, fn func()) {
	b.selfSourceDir = sourceDir
	b.onSelfRestart = fn
}

// HandleMessageStreaming processes an incoming user message and streams text deltas via onUpdate.
// senderName identifies who sent the message (e.g. Telegram first name).
// images optionally contains downloaded image metadata that should be
// included in the message sent to Claude (e.g. downloaded Telegram photos).
// pdfs optionally contains downloaded PDF metadata that should be
// included in the message sent to Claude (e.g. downloaded Telegram documents).
func (b *Bridge) HandleMessageStreaming(ctx context.Context, chatID int64, userMsg, senderName string, images []ImageInfo, pdfs []PDFInfo, onUpdate process.StreamFunc) (AgentResponse, error) {
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

	sess, err := b.ensureSession(ctx, chatID)
	if err != nil {
		return AgentResponse{}, fmt.Errorf("ensure session: %w", err)
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

	// Determine session ID for --resume
	agent := b.resolveAgent(chatID)
	procSess, _ := agent.Get(chatID)
	claudeSessionID := ""
	if procSess != nil && procSess.HasHistory {
		claudeSessionID = procSess.ProviderSessionID
	}

	// Build system prompt from memory if available.
	systemPrompt := ""
	if b.memory != nil {
		systemPrompt = b.memory.SystemPrompt(ctx, chatID)
	}
	systemPrompt += b.timestampSystemPrompt()
	systemPrompt += b.skillsSystemPrompt()

	// Track session in pm for /pm list visibility.
	endTrack := b.trackSession(ctx, chatID, senderName)
	defer endTrack()

	result, err := agent.Send(ctx, process.AgentRequest{
		ChatID:       chatID,
		SessionID:    claudeSessionID,
		Text:         augmentedMsg,
		Images:       imgAttachments,
		PDFs:         pdfAttachments,
		SystemPrompt: systemPrompt,
	}, onUpdate)
	if err != nil {
		return AgentResponse{}, fmt.Errorf("claude: %w", err)
	}

	// Track session ID and mark as having history
	if procSess != nil {
		if result.SessionID != "" {
			procSess.ProviderSessionID = result.SessionID
			if err := b.store.SaveSession(chatID, result.SessionID); err != nil {
				slog.Warn("failed to update session ID in store", "error", err)
			}
		}
		procSess.HasHistory = true
	}

	return b.processResponse(ctx, chatID, sess.ID, userMsg, isHeartbeat, result), nil
}

// processResponse is the post-processing pipeline for HandleMessageStreaming.
// It parses all response directives (relay, heartbeat, memory, schedule, artifacts),
// logs the exchange, and returns a typed AgentResponse with collected photos.
func (b *Bridge) processResponse(ctx context.Context, chatID, sessID int64, userMsg string, isHeartbeat bool, result process.SendResult) AgentResponse {
	response := strings.TrimSpace(result.Text)

	// Run memory maintenance during heartbeats.
	if isHeartbeat && b.memory != nil {
		// Run reflect cycle after heartbeat to promote/decay/prune memories.
		b.memory.RunReflect(ctx)
		// Summarize old exchanges during heartbeat maintenance.
		if n, err := b.memory.SummarizeExchanges(ctx, chatID); err != nil {
			slog.Warn("exchange summarization failed", "error", err)
		} else if n > 0 {
			slog.Info("heartbeat summarized exchanges", "chat_id", chatID, "count", n)
		}
	}

	// Collect photos from artifact markers (skill output).
	var photos []Photo
	response = b.parseArtifacts(response, &photos)

	// Log assistant response.
	if err := b.store.LogMessage(sessID, "assistant", response); err != nil {
		slog.Warn("failed to log assistant message", "error", err)
	}

	// Log exchange to memory.
	if b.memory != nil {
		b.memory.LogExchange(ctx, chatID, userMsg, response)
	}

	// Update session timestamp.
	if err := b.store.UpdateSessionStatus(chatID, "active"); err != nil {
		slog.Warn("failed to update session", "error", err)
	}

	return AgentResponse{Text: response, Photos: photos}
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
				ProviderSessionID: sess.ProviderSessionID,
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

	if err := b.store.SaveSession(chatID, procSess.ProviderSessionID); err != nil {
		return nil, fmt.Errorf("save session: %w", err)
	}

	// Auto-create default heartbeat for new chats if scheduler + memory are enabled
	if b.schedulerEnabled && b.memory != nil {
		b.ensureDefaultHeartbeat(chatID)
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
