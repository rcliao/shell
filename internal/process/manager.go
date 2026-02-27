package process

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// StreamFunc is called with a text delta as Claude generates it.
type StreamFunc func(delta string)

// streamEvent represents a line from --output-format stream-json.
// The Claude CLI emits several event types:
//   - {"type":"system", "subtype":"init", "session_id":"..."}
//   - {"type":"stream_event", "event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"..."}}}
//   - {"type":"assistant", "message":{"content":[{"type":"text","text":"..."}]}, "session_id":"..."}
//   - {"type":"result", "result":"final text", "session_id":"..."}
type streamEvent struct {
	Type      string         `json:"type"`
	Subtype   string         `json:"subtype,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	Result    string         `json:"result,omitempty"`
	Event     *innerEvent    `json:"event,omitempty"`
	Message   *streamMessage `json:"message,omitempty"`
}

type innerEvent struct {
	Type  string       `json:"type"`
	Delta *streamDelta `json:"delta,omitempty"`
}

type streamDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type streamMessage struct {
	Content []streamContent `json:"content"`
}

type streamContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// claudeResult is the JSON structure returned by claude --output-format json.
type claudeResult struct {
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
}

// SendResult contains the response text and the Claude session ID.
type SendResult struct {
	Text      string
	SessionID string
}

type Manager struct {
	sessions map[int64]*Session
	mu       sync.RWMutex

	binary      string
	model       string
	timeout     time.Duration
	maxSessions int
	workDir     string
	extraArgs   []string
}

type ManagerConfig struct {
	Binary      string
	Model       string
	Timeout     time.Duration
	MaxSessions int
	WorkDir     string
	ExtraArgs   []string
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
		sessions:    make(map[int64]*Session),
		binary:      cfg.Binary,
		model:       cfg.Model,
		timeout:     cfg.Timeout,
		maxSessions: cfg.MaxSessions,
		workDir:     cfg.WorkDir,
		extraArgs:   cfg.ExtraArgs,
	}
}

// Send sends a message to Claude via the CLI and returns the response.
// If claudeSessionID is non-empty, uses --resume <id> to resume a specific session.
// Otherwise starts a fresh session. Returns SendResult with text and session ID.
// If systemPrompt is non-empty, it is passed via --append-system-prompt.
func (m *Manager) Send(ctx context.Context, chatID int64, claudeSessionID, message, systemPrompt string) (SendResult, error) {
	m.mu.RLock()
	sess, exists := m.sessions[chatID]
	m.mu.RUnlock()

	if exists && sess.Status == StatusBusy {
		return SendResult{}, fmt.Errorf("session for chat %d is busy", chatID)
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

	result, err := m.runClaude(ctx, message, claudeSessionID, systemPrompt)
	if err != nil && claudeSessionID != "" {
		// If --resume failed, retry as fresh session
		slog.Warn("resume failed, retrying as fresh session", "chat_id", chatID, "error", err)
		result, err = m.runClaude(ctx, message, "", systemPrompt)
	}
	return result, err
}

func (m *Manager) runClaude(ctx context.Context, message, claudeSessionID, systemPrompt string) (SendResult, error) {
	procCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	args := []string{
		"-p", message,
		"--output-format", "json",
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
	args = append(args, m.extraArgs...)

	slog.Debug("spawning claude", "resume", claudeSessionID, "binary", m.binary)

	cmd := exec.CommandContext(procCtx, m.binary, args...)
	// Clear CLAUDECODE env var so claude doesn't think it's nested inside another session
	cmd.Env = filterEnv(os.Environ(), "CLAUDECODE")
	if m.workDir != "" {
		cmd.Dir = m.workDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if procCtx.Err() == context.DeadlineExceeded {
			return SendResult{}, fmt.Errorf("claude process timed out after %s", m.timeout)
		}
		stderrStr := stderr.String()
		if stderrStr != "" {
			return SendResult{}, fmt.Errorf("claude process failed: %w\nstderr: %s", err, stderrStr)
		}
		return SendResult{}, fmt.Errorf("claude process failed: %w", err)
	}

	var result claudeResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		// Fallback: treat as plain text
		return SendResult{Text: stdout.String()}, nil
	}

	return SendResult{
		Text:      result.Result,
		SessionID: result.SessionID,
	}, nil
}

// SendStreaming sends a message to Claude and calls onUpdate with accumulated text as it streams.
// Returns the final SendResult when complete.
// If systemPrompt is non-empty, it is passed via --append-system-prompt.
func (m *Manager) SendStreaming(ctx context.Context, chatID int64, claudeSessionID, message, systemPrompt string, onUpdate StreamFunc) (SendResult, error) {
	m.mu.RLock()
	sess, exists := m.sessions[chatID]
	m.mu.RUnlock()

	if exists && sess.Status == StatusBusy {
		return SendResult{}, fmt.Errorf("session for chat %d is busy", chatID)
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

	result, err := m.runClaudeStreaming(ctx, message, claudeSessionID, systemPrompt, onUpdate)
	if err != nil && claudeSessionID != "" {
		// If --resume failed, retry as fresh session
		slog.Warn("resume failed, retrying as fresh session (streaming)", "chat_id", chatID, "error", err)
		result, err = m.runClaudeStreaming(ctx, message, "", systemPrompt, onUpdate)
	}
	return result, err
}

func (m *Manager) runClaudeStreaming(ctx context.Context, message, claudeSessionID, systemPrompt string, onUpdate StreamFunc) (SendResult, error) {
	procCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	args := []string{
		"-p", message,
		"--output-format", "stream-json",
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
	args = append(args, m.extraArgs...)

	slog.Debug("spawning claude (streaming)", "resume", claudeSessionID, "binary", m.binary)

	cmd := exec.CommandContext(procCtx, m.binary, args...)
	cmd.Env = filterEnv(os.Environ(), "CLAUDECODE")
	if m.workDir != "" {
		cmd.Dir = m.workDir
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

	finalResult := parseStreamEvents(stdout, onUpdate)

	if err := cmd.Wait(); err != nil {
		if procCtx.Err() == context.DeadlineExceeded {
			return SendResult{}, fmt.Errorf("claude process timed out after %s", m.timeout)
		}
		// If we already got a result, return it despite exit code
		if finalResult.Text != "" {
			return finalResult, nil
		}
		stderrStr := stderr.String()
		if stderrStr != "" {
			return SendResult{}, fmt.Errorf("claude process failed: %w\nstderr: %s", err, stderrStr)
		}
		return SendResult{}, fmt.Errorf("claude process failed: %w", err)
	}

	return finalResult, nil
}

// parseStreamEvents reads NDJSON lines from r, calling onUpdate for each text delta
// and returning the final SendResult. This is extracted for testability.
func parseStreamEvents(r io.Reader, onUpdate StreamFunc) SendResult {
	var finalResult SendResult
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event streamEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}

		switch event.Type {
		case "stream_event":
			if event.Event != nil && event.Event.Delta != nil && event.Event.Delta.Type == "text_delta" {
				if event.Event.Delta.Text != "" && onUpdate != nil {
					onUpdate(event.Event.Delta.Text)
				}
			}
		case "assistant":
			if event.Message != nil {
				text := extractContentText(event.Message.Content)
				if text != "" && onUpdate != nil {
					onUpdate(text)
				}
			}
			if event.SessionID != "" {
				finalResult.SessionID = event.SessionID
			}
		case "system":
			if event.SessionID != "" {
				finalResult.SessionID = event.SessionID
			}
		case "result":
			finalResult.Text = event.Result
			if event.SessionID != "" {
				finalResult.SessionID = event.SessionID
			}
		}
	}

	return finalResult
}

// extractContentText joins all text content blocks from a stream message.
func extractContentText(content []streamContent) string {
	var sb strings.Builder
	for _, c := range content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
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
