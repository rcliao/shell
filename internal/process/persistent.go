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

// persistentProc holds a long-lived Claude CLI process for a single chat.
// Messages are sent via stdin and responses streamed from stdout.
type persistentProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr bytes.Buffer
	scanner *bufio.Scanner // persistent scanner across messages — avoids losing buffered bytes

	sessionID string // Claude session ID (from init response)
	chatID    int64

	mu       sync.Mutex // guards stdin writes and scanner reads
	cancel   context.CancelFunc
	idleTimer *time.Timer
}

// idleTimeout is how long a persistent process stays alive without messages.
const idleTimeout = 10 * time.Minute

// getOrSpawn returns the persistent process for a chat, spawning one if needed.
// Returns nil if persistent mode is not suitable (will fall back to per-message).
func (m *Manager) getOrSpawn(ctx context.Context, req AgentRequest) (*persistentProc, error) {
	m.mu.Lock()
	proc, ok := m.persistent[req.ChatID]
	m.mu.Unlock()

	if ok && proc.cmd.ProcessState == nil {
		// Process is still running — reset idle timer.
		proc.idleTimer.Reset(idleTimeout)
		return proc, nil
	}

	// Clean up dead process if any.
	if ok {
		m.mu.Lock()
		delete(m.persistent, req.ChatID)
		m.mu.Unlock()
	}

	// Spawn new persistent process.
	proc, err := m.spawnPersistent(ctx, req)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.persistent[req.ChatID] = proc
	m.mu.Unlock()

	return proc, nil
}

// spawnPersistent starts a new long-lived Claude CLI process.
func (m *Manager) spawnPersistent(ctx context.Context, req AgentRequest) (*persistentProc, error) {
	procCtx, cancel := context.WithCancel(ctx)

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
	}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if m.model != "" {
		args = append(args, "--model", m.model)
	}
	// Only append system prompt on fresh sessions — resumed sessions
	// already have the system prompt in their conversation history.
	if req.SystemPrompt != "" && req.SessionID == "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}
	if len(m.allowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(m.allowedTools, ","))
	}
	if len(m.settingSources) > 0 {
		args = append(args, "--setting-sources", strings.Join(m.settingSources, ","))
	}
	args = append(args, "--permission-mode", "bypassPermissions")
	if m.mcpConfigPath != "" {
		args = append(args, "--mcp-config", m.mcpConfigPath)
	}
	args = append(args, m.extraArgs...)

	cmd := exec.CommandContext(procCtx, m.binary, args...)
	env := filterEnv(os.Environ(), "CLAUDECODE")
	env = append(env, fmt.Sprintf("SHELL_CHAT_ID=%d", req.ChatID))
	if m.bridgeSockPath != "" {
		env = append(env, "SHELL_BRIDGE_SOCK="+m.bridgeSockPath)
	}
	cmd.Env = env
	if m.workDir != "" {
		cmd.Dir = m.workDir
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start claude: %w", err)
	}

	slog.Info("persistent process spawned", "chat_id", req.ChatID, "pid", cmd.Process.Pid, "resume", req.SessionID != "")

	// Send initialize.
	if err := writeJSON(stdin, stdinControlRequest{
		Type:      "control_request",
		RequestID: initRequestID,
		Request:   map[string]any{"subtype": "initialize"},
	}); err != nil {
		stdin.Close()
		cancel()
		cmd.Wait()
		return nil, fmt.Errorf("send initialize: %w", err)
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	proc := &persistentProc{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		scanner: sc,
		chatID:  req.ChatID,
		cancel:  cancel,
	}

	// Set up idle timer to kill the process if no messages arrive.
	proc.idleTimer = time.AfterFunc(idleTimeout, func() {
		slog.Info("persistent process idle timeout", "chat_id", req.ChatID)
		proc.kill()
		m.mu.Lock()
		delete(m.persistent, req.ChatID)
		m.mu.Unlock()
	})

	return proc, nil
}

// sendMessage sends a user message to the persistent process and streams the response.
// This is the persistent equivalent of runClaudeBidirectional.
func (p *persistentProc) sendMessage(ctx context.Context, req AgentRequest, onUpdate StreamFunc) (SendResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Send user message.
	if err := writeJSON(p.stdin, newUserMessage(req, p.sessionID)); err != nil {
		return SendResult{}, fmt.Errorf("send user message: %w", err)
	}

	// Read events using the persistent scanner (not a new one per message).
	// This avoids losing buffered bytes between turns.
	result := parseBidirectionalEventsScanner(p.scanner, p.stdin, onUpdate)

	// Update session ID if we got one.
	if result.SessionID != "" {
		p.sessionID = result.SessionID
	}

	return result, nil
}

// kill terminates the persistent process.
func (p *persistentProc) kill() {
	if p.idleTimer != nil {
		p.idleTimer.Stop()
	}
	p.stdin.Close()
	p.cancel()
	p.cmd.Wait()
	slog.Info("persistent process killed", "chat_id", p.chatID)
}

// sendPersistent tries to use a persistent process for the request.
// Returns the result, or an error if the persistent process failed.
func (m *Manager) sendPersistent(ctx context.Context, req AgentRequest, onUpdate StreamFunc) (SendResult, error) {
	proc, err := m.getOrSpawn(ctx, req)
	if err != nil {
		return SendResult{}, err
	}

	result, err := proc.sendMessage(ctx, req, onUpdate)
	if err != nil {
		// Process likely died — clean up and let caller retry with spawn-per-message.
		slog.Warn("persistent process send failed, cleaning up", "chat_id", req.ChatID, "error", err)
		proc.kill()
		m.mu.Lock()
		delete(m.persistent, req.ChatID)
		m.mu.Unlock()
		return SendResult{}, err
	}

	return result, nil
}

// killPersistent kills the persistent process for a chat if one exists.
func (m *Manager) killPersistent(chatID int64) {
	m.mu.Lock()
	proc, ok := m.persistent[chatID]
	if ok {
		delete(m.persistent, chatID)
	}
	m.mu.Unlock()

	if ok {
		proc.kill()
	}
}

// killAllPersistent kills all persistent processes.
func (m *Manager) killAllPersistent() {
	m.mu.Lock()
	procs := make([]*persistentProc, 0, len(m.persistent))
	for _, p := range m.persistent {
		procs = append(procs, p)
	}
	m.persistent = make(map[int64]*persistentProc)
	m.mu.Unlock()

	for _, p := range procs {
		p.kill()
	}
}

// hasPersistent returns true if a persistent process exists for the chat.
func (m *Manager) hasPersistent(chatID int64) bool {
	m.mu.RLock()
	_, ok := m.persistent[chatID]
	m.mu.RUnlock()
	return ok
}

// readInitEvents reads initial events from stdout to drain the init response.
// This handles the control_response for our initialize request and any
// system events before we send the first user message.
func drainInitEvents(stdout io.Reader, stdin io.Writer) string {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event stdoutEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}

		switch event.Type {
		case "system":
			if event.SessionID != "" {
				return event.SessionID
			}
		case "control_response":
			// Init response received — continue reading until system event.
			slog.Debug("persistent: init control_response received")
		case "control_request":
			handleControlRequest(event, stdin)
		default:
			slog.Debug("persistent: init event", "type", event.Type)
		}
	}
	return ""
}
