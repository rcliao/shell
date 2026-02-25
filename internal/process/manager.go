package process

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

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
		cfg.Timeout = 5 * time.Minute
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
// If continueSession is true, uses --continue to pick up the most recent session.
// Otherwise starts a fresh session. Returns SendResult with text and session ID.
func (m *Manager) Send(ctx context.Context, chatID int64, message string, continueSession bool) (SendResult, error) {
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

	result, err := m.runClaude(ctx, message, continueSession)
	if err != nil && continueSession {
		// If --continue failed, retry as fresh session
		slog.Warn("continue failed, retrying as fresh session", "chat_id", chatID, "error", err)
		result, err = m.runClaude(ctx, message, false)
	}
	return result, err
}

func (m *Manager) runClaude(ctx context.Context, message string, continueSession bool) (SendResult, error) {
	procCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	args := []string{
		"-p", message,
		"--output-format", "json",
	}
	if continueSession {
		args = append(args, "--continue")
	}
	if m.model != "" {
		args = append(args, "--model", m.model)
	}
	args = append(args, m.extraArgs...)

	slog.Debug("spawning claude", "continue", continueSession, "binary", m.binary)

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
