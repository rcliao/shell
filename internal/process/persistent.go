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
	"sync"
	"syscall"
	"time"
)

// persistentProc holds a long-lived Claude CLI process for a single
// (chat, message_thread_id) key. Messages are sent via stdin and responses
// streamed from stdout.
type persistentProc struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdinW  *syncWriter // all protocol writes go through this (parse loop + V2-H46 injector run concurrently)
	stdout  io.ReadCloser
	stderr  bytes.Buffer
	scanner *bufio.Scanner // persistent scanner across messages — avoids losing buffered bytes

	sessionID string // Claude session ID (from init response); guarded by turn.mu (read by the mid-turn injector)
	key       SessionKey
	model     string // model used when spawning this process

	mu        sync.Mutex // guards turn dispatch and scanner reads
	turn      turnState  // live-turn tracking for mid-turn injection (V2-H46)
	cancel    context.CancelFunc
	idleTimer *time.Timer
}

// idleTimeout is how long a persistent process stays alive without messages.
const idleTimeout = 10 * time.Minute

// getOrSpawn returns the persistent process for a (chat, thread) key,
// spawning one if needed. Returns nil if persistent mode is not suitable
// (will fall back to per-message).
func (m *Manager) getOrSpawn(ctx context.Context, req AgentRequest) (*persistentProc, error) {
	key := req.Key()
	m.mu.Lock()
	proc, ok := m.persistent[key]
	m.mu.Unlock()

	if ok && proc.cmd.ProcessState == nil {
		// Check model mismatch — if the request wants a different model than
		// the one used to spawn this process, fall back to per-message mode.
		reqModel := req.Model
		if reqModel == "" {
			reqModel = m.model
		}
		if proc.model != reqModel {
			return nil, fmt.Errorf("model mismatch: proc=%q req=%q", proc.model, reqModel)
		}
		// Process is still running — reset idle timer.
		proc.idleTimer.Reset(idleTimeout)
		return proc, nil
	}

	// Clean up dead process if any.
	if ok {
		m.mu.Lock()
		delete(m.persistent, key)
		m.mu.Unlock()
	}

	// Spawn new persistent process.
	proc, err := m.spawnPersistent(ctx, req)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.persistent[key] = proc
	m.mu.Unlock()

	return proc, nil
}

// spawnPersistent starts a new long-lived Claude CLI process.
func (m *Manager) spawnPersistent(ctx context.Context, req AgentRequest) (*persistentProc, error) {
	key := req.Key()
	procCtx, cancel := context.WithCancel(ctx)

	args, model := buildClaudeArgs(req, m.claudeArgOpts())

	cmd := exec.CommandContext(procCtx, m.binary, args...)
	// Terminate politely on cancellation. exec.CommandContext defaults to
	// Process.Kill() — SIGKILL — which gives the CLI no chance to flush its
	// session state or finish writing a reply. WaitDelay escalates to SIGKILL
	// anyway if SIGTERM is ignored, so this can only improve the outcome.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = sigtermGrace

	env := filterEnv(os.Environ(), "CLAUDECODE")
	for k := range m.env {
		env = filterEnv(env, k)
	}
	for k, v := range m.env {
		env = append(env, k+"="+v)
	}
	env = append(env, fmt.Sprintf("SHELL_CHAT_ID=%d", req.ChatID))
	if req.MessageThreadID != 0 {
		env = append(env, fmt.Sprintf("SHELL_MESSAGE_THREAD_ID=%d", req.MessageThreadID))
	}
	if m.bridgeSockPath != "" {
		env = append(env, "SHELL_BRIDGE_SOCK="+m.bridgeSockPath)
	}
	if m.agentNS != "" {
		env = append(env, "GHOST_NS="+m.agentNS)
	}
	if m.ghostDB != "" {
		env = append(env, "GHOST_DB="+m.ghostDB)
	}
	if m.botUsername != "" {
		env = append(env, "SHELL_BOT_USERNAME="+m.botUsername)
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

	slog.Info("persistent process spawned", "chat_id", req.ChatID, "thread_id", req.MessageThreadID, "pid", cmd.Process.Pid, "resume", req.SessionID != "")

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
		stdinW:  &syncWriter{w: stdin},
		stdout:  stdout,
		stderr:  stderr,
		scanner: sc,
		key:     key,
		model:   model,
		cancel:  cancel,
	}

	// Set up idle timer to kill the process if no messages arrive.
	proc.idleTimer = time.AfterFunc(idleTimeout, func() {
		slog.Info("persistent process idle timeout", "chat_id", key.ChatID, "thread_id", key.ThreadID)
		proc.kill()
		m.mu.Lock()
		delete(m.persistent, key)
		m.mu.Unlock()
	})

	return proc, nil
}

// sendMessage sends a user message to the persistent process and streams the response.
// This is the persistent equivalent of runClaudeBidirectional.
func (p *persistentProc) sendMessage(ctx context.Context, req AgentRequest, onUpdate StreamFunc) (SendResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Track the live turn so a same-sender follow-up can be absorbed into it
	// mid-flight (V2-H46). begin/end bracket exactly the window where the
	// injector may act.
	p.turn.mu.Lock()
	sessionID := p.sessionID
	p.turn.mu.Unlock()
	p.turn.begin()
	defer p.turn.end()

	// Send user message.
	if err := writeJSON(p.stdinW, newUserMessage(req, sessionID)); err != nil {
		return SendResult{}, fmt.Errorf("send user message: %w", err)
	}

	// Read events using the persistent scanner (not a new one per message).
	// This avoids losing buffered bytes between turns.
	result := parseBidirectionalEventsObserved(p.scanner, p.stdinW, onUpdate, &p.turn)

	// Update session ID if we got one (turn.mu: the injector reads it).
	if result.SessionID != "" {
		p.turn.mu.Lock()
		p.sessionID = result.SessionID
		p.turn.mu.Unlock()
	}

	return result, nil
}

// stdinCloseGrace is how long the CLI gets to exit on its own after stdin is
// closed. Closing stdin is the protocol's own end-of-input signal, so a healthy
// process finishes its turn and exits by itself — the graceful path.
const stdinCloseGrace = 5 * time.Second

// sigtermGrace is how long SIGTERM gets before Go escalates to SIGKILL.
const sigtermGrace = 10 * time.Second

// kill terminates the persistent process, escalating only as far as needed.
//
// The previous version closed stdin and then cancelled the context on the very
// next line — and exec.CommandContext cancels with SIGKILL, so the polite
// signal never had time to work. Every shutdown was a hard kill; a deep
// reflection beat interrupted by a deploy on 2026-08-01 died with
// "signal: killed" after 340s of work.
//
// Now: close stdin, give the process a moment to leave on its own, then SIGTERM
// (via cmd.Cancel), with SIGKILL behind it via WaitDelay. Bounded at every step
// so shutdown can never hang.
func (p *persistentProc) kill() {
	if p.idleTimer != nil {
		p.idleTimer.Stop()
	}
	p.stdin.Close()

	exited := make(chan error, 1)
	go func() { exited <- p.cmd.Wait() }()

	select {
	case <-exited:
		slog.Info("persistent process exited cleanly on stdin close",
			"chat_id", p.key.ChatID, "thread_id", p.key.ThreadID)
		return
	case <-time.After(stdinCloseGrace):
	}

	// Still running: escalate. cancel() sends SIGTERM; WaitDelay turns it into
	// SIGKILL if that is ignored.
	p.cancel()
	<-exited
	slog.Info("persistent process terminated after grace period",
		"chat_id", p.key.ChatID, "thread_id", p.key.ThreadID, "grace", stdinCloseGrace)
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
		slog.Warn("persistent process send failed, cleaning up", "chat_id", req.ChatID, "thread_id", req.MessageThreadID, "error", err)
		proc.kill()
		m.mu.Lock()
		delete(m.persistent, req.Key())
		m.mu.Unlock()
		return SendResult{}, err
	}

	return result, nil
}

// killPersistent kills the persistent process for a (chat, thread) key if one exists.
func (m *Manager) killPersistent(key SessionKey) {
	m.mu.Lock()
	proc, ok := m.persistent[key]
	if ok {
		delete(m.persistent, key)
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
	m.persistent = make(map[SessionKey]*persistentProc)
	m.mu.Unlock()

	for _, p := range procs {
		p.kill()
	}
}

// hasPersistent returns true if a persistent process exists for the (chat, thread) key.
func (m *Manager) hasPersistent(key SessionKey) bool {
	m.mu.RLock()
	_, ok := m.persistent[key]
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
