package process

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// StreamFunc is called with a text delta as Claude generates it.
type StreamFunc func(delta string)

// Artifact represents binary output produced by a tool call (image, file, etc.).
type Artifact struct {
	Type    string // "image", "file", "audio"
	Data    []byte // binary content
	Caption string // optional description
	Path    string // optional file path (alternative to inline Data)
}

// SendResult contains the response text, session ID, and any binary artifacts.
type SendResult struct {
	Text      string
	SessionID string
	Artifacts []Artifact
	ToolCalls []ToolCall // tool calls observed during execution
}

type Manager struct {
	sessions map[int64]*Session
	mu       sync.RWMutex

	binary         string
	model          string
	timeout        time.Duration
	maxSessions    int
	workDir        string
	allowedTools   []string
	extraArgs      []string
	settingSources []string
}

type ManagerConfig struct {
	Binary         string
	Model          string
	Timeout        time.Duration
	MaxSessions    int
	WorkDir        string
	AllowedTools   []string
	ExtraArgs      []string
	SettingSources []string
}

func NewManager(cfg ManagerConfig) *Manager {
	if cfg.Binary == "" {
		cfg.Binary = "claude"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Minute
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 4
	}
	return &Manager{
		sessions:       make(map[int64]*Session),
		binary:         cfg.Binary,
		model:          cfg.Model,
		timeout:        cfg.Timeout,
		maxSessions:    cfg.MaxSessions,
		workDir:        cfg.WorkDir,
		allowedTools:   cfg.AllowedTools,
		extraArgs:      cfg.ExtraArgs,
		settingSources: cfg.SettingSources,
	}
}

// Send sends a prompt and streams text deltas via onUpdate (nil for no streaming).
func (m *Manager) Send(ctx context.Context, req AgentRequest, onUpdate StreamFunc) (SendResult, error) {
	m.mu.RLock()
	sess, exists := m.sessions[req.ChatID]
	m.mu.RUnlock()

	if exists && sess.Status == StatusBusy {
		return SendResult{}, fmt.Errorf("session for chat %d is busy", req.ChatID)
	}

	// Check concurrency limit
	m.mu.RLock()
	busy := 0
	for _, s := range m.sessions {
		if s.Status == StatusBusy {
			busy++
		}
	}
	m.mu.RUnlock()
	if busy >= m.maxSessions {
		return SendResult{}, fmt.Errorf("max concurrent sessions (%d) reached", m.maxSessions)
	}

	// Mark session as busy
	if exists {
		m.mu.Lock()
		sess.Status = StatusBusy
		sess.UpdatedAt = time.Now()
		m.mu.Unlock()
	}

	defer func() {
		if exists {
			m.mu.Lock()
			sess.Status = StatusActive
			sess.UpdatedAt = time.Now()
			m.mu.Unlock()
		}
	}()

	result, err := m.runClaudeBidirectional(ctx, req, onUpdate)
	if err != nil && req.SessionID != "" {
		slog.Warn("resume failed, retrying as fresh session", "chat_id", req.ChatID, "error", err)
		freshReq := req
		freshReq.SessionID = ""
		result, err = m.runClaudeBidirectional(ctx, freshReq, onUpdate)
	}
	return result, err
}

func (m *Manager) runClaudeBidirectional(ctx context.Context, req AgentRequest, onUpdate StreamFunc) (SendResult, error) {
	claudeSessionID := req.SessionID
	systemPrompt := req.SystemPrompt
	procCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
	}
	if claudeSessionID != "" {
		args = append(args, "--resume", claudeSessionID)
	}
	if m.model != "" {
		args = append(args, "--model", m.model)
	}
	if systemPrompt != "" {
		args = append(args, "--append-system-prompt", systemPrompt)
	}
	if len(m.allowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(m.allowedTools, ","))
	}
	if len(m.settingSources) > 0 {
		args = append(args, "--setting-sources", strings.Join(m.settingSources, ","))
	}
	args = append(args, "--permission-mode", "bypassPermissions")
	args = append(args, m.extraArgs...)

	hasAttachments := len(req.Images) > 0 || len(req.PDFs) > 0
	slog.Info("claude send", "resume", claudeSessionID != "", "multimodal", hasAttachments)

	cmd := exec.CommandContext(procCtx, m.binary, args...)
	cmd.Env = filterEnv(os.Environ(), "CLAUDECODE")
	if m.workDir != "" {
		cmd.Dir = m.workDir
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return SendResult{}, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return SendResult{}, fmt.Errorf("stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return SendResult{}, fmt.Errorf("start claude: %w", err)
	}

	// Phase 1: Send initialize control request (SDK → CLI)
	if err := writeJSON(stdin, stdinControlRequest{
		Type:      "control_request",
		RequestID: initRequestID,
		Request:   map[string]any{"subtype": "initialize"},
	}); err != nil {
		stdin.Close()
		cmd.Wait()
		return SendResult{}, fmt.Errorf("send initialize: %w", err)
	}

	// Phase 2: Send user message immediately after init (SDK → CLI)
	// The CLI processes messages in order; no need to wait for init response
	// before sending the user message. parseBidirectionalEvents handles all
	// event types including control_response.
	if err := writeJSON(stdin, newUserMessage(req, claudeSessionID)); err != nil {
		stdin.Close()
		cmd.Wait()
		return SendResult{}, fmt.Errorf("send user message: %w", err)
	}

	// Phase 3: Stream all responses through a single reader
	finalResult := parseBidirectionalEvents(stdout, stdin, onUpdate)

	// Phase 4: Close stdin, wait for process
	stdin.Close()

	if err := cmd.Wait(); err != nil {
		if procCtx.Err() == context.DeadlineExceeded {
			return SendResult{}, fmt.Errorf("claude process timed out after %s", m.timeout)
		}
		if finalResult.Text != "" {
			return finalResult, nil
		}
		stderrStr := stderr.String()
		slog.Warn("claude process failed", "error", err, "stderr", stderrStr, "resume", claudeSessionID != "")
		if stderrStr != "" {
			return SendResult{}, fmt.Errorf("claude process failed: %w\nstderr: %s", err, stderrStr)
		}
		return SendResult{}, fmt.Errorf("claude process failed: %w", err)
	}

	return finalResult, nil
}

// Register adds or updates a session in the manager.
func (m *Manager) Register(sess *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[sess.ChatID] = sess
}

// Get returns the session for a chat ID.
func (m *Manager) Get(chatID int64) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[chatID]
	return s, ok
}

// Remove removes a session from the manager.
func (m *Manager) Remove(chatID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, chatID)
}

// Kill terminates any running process for a chat ID and removes the session.
func (m *Manager) Kill(chatID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sess, ok := m.sessions[chatID]; ok {
		sess.Status = StatusClosed
	}
	delete(m.sessions, chatID)
}

// KillAll terminates all sessions.
func (m *Manager) KillAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sess := range m.sessions {
		sess.Status = StatusClosed
	}
	m.sessions = make(map[int64]*Session)
}

// ActiveCount returns the number of active sessions.
func (m *Manager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// ListSessions returns info about all tracked sessions.
func (m *Manager) ListSessions() []Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sessions := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, *s)
	}
	return sessions
}

// FormatMessage builds the prompt string from an AgentRequest, prepending
// image and PDF attachment metadata so the CLI can read the files.
func FormatMessage(req AgentRequest) string {
	if len(req.Images) == 0 && len(req.PDFs) == 0 {
		return req.Text
	}

	var sb strings.Builder
	for _, img := range req.Images {
		fmt.Fprintf(&sb, "[Attached image: %s", img.Path)
		if img.Width > 0 && img.Height > 0 {
			fmt.Fprintf(&sb, " | %dx%d", img.Width, img.Height)
		}
		if img.Size > 0 {
			fmt.Fprintf(&sb, " | %s", formatFileSize(img.Size))
		}
		sb.WriteString("]\n")
	}
	for _, pdf := range req.PDFs {
		fmt.Fprintf(&sb, "[Attached PDF: %s", pdf.Path)
		if pages := countPDFPages(pdf.Path); pages > 0 {
			fmt.Fprintf(&sb, " | %d pages", pages)
		}
		if pdf.Size > 0 {
			fmt.Fprintf(&sb, " | %s", formatFileSize(pdf.Size))
		}
		sb.WriteString("]\n")
	}
	sb.WriteString(req.Text)
	return sb.String()
}

// formatFileSize returns a human-readable file size string.
func formatFileSize(size int64) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(size)/float64(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(size)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", size)
	}
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

// filterEnv returns env with the named variable removed.
func filterEnv(env []string, name string) []string {
	prefix := name + "="
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			filtered = append(filtered, e)
		}
	}
	return filtered
}
