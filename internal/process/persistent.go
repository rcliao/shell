package process

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdinW *syncWriter // all protocol writes go through this (parse loop + V2-H46 injector run concurrently)
	stdout io.Reader
	stderr bytes.Buffer

	sessionID string // Claude session ID (from init response); guarded by turn.mu (read by the mid-turn injector)
	key       SessionKey
	model     string // model used when spawning this process

	mu        sync.Mutex // serializes sendMessage callers
	turn      turnState  // live-turn tracking for mid-turn injection (V2-H46)
	cancel    context.CancelFunc
	idleTimer *time.Timer

	// Continuous reader (2026-09-05 off-by-one incident). stdout used to be
	// read only inside sendMessage, until the first "result". The CLI can
	// produce a whole turn on its own between our messages — a background
	// Agent subagent finishing injects a <task-notification> user message,
	// the model answers it, and a "result" follows. The next sendMessage
	// consumed that buffered turn as its answer and every reply after it was
	// the answer to the previous message. Now a reader goroutine owns stdout
	// and a pump parses turns continuously; a turn nobody asked for is handed
	// to onUnsolicited (the bridge delivers it as a follow-up message).
	lines    chan []byte   // fed by readLoop; closed on EOF
	pumpDone chan struct{} // closed when pump exits (EOF)

	dispatch      sync.Mutex      // guards waiter, emit, turnOpen, idle
	waiter        chan SendResult // the in-flight sendMessage, if any (buffered 1)
	emit          EventFunc       // that turn's event sink; nil when idle
	turnOpen      bool            // a turn (ours or the CLI's) has started streaming
	idle          chan struct{}   // closed while no turn is open; replaced when one opens
	onUnsolicited func(SessionKey, SendResult)
	claimWait     time.Duration // max wait behind a CLI-initiated turn (claimTurnMaxWait; tests shorten it)
}

// ErrTurnAbandoned is returned by sendMessage when the caller's context ends
// while the turn is still running. The process is NOT killed: the turn keeps
// going and its result, when it lands, is routed to the unsolicited handler
// (delivered as a follow-up) instead of being lost. Callers must not fall
// back to a second subprocess on this error — the session is still busy.
var ErrTurnAbandoned = errors.New("turn abandoned: caller context ended while the turn was in flight")

// ErrStalledBehindCLITurn is returned when a send waited claimTurnMaxWait for
// a CLI-initiated turn to finish and it never did. sendPersistent treats it
// like a dead process: kill, clean up, fall back to a one-shot resume — the
// owner's message is answered within a bounded time; the CLI turn's
// follow-up is lost. Nothing above the process layer bounds a persistent
// send, so without this a runaway continuation could stall a chat until the
// 10-minute idle kill.
var ErrStalledBehindCLITurn = errors.New("send stalled behind a CLI-initiated turn")

// claimTurnMaxWait bounds how long a send waits for a CLI-initiated turn.
const claimTurnMaxWait = 2 * time.Minute

// newPersistentProc wires the reader and pump around an already-started
// process (or, in tests, an io.Pipe). It does not send the initialize
// request; the caller does that before or after — the pump treats the init
// system/control_response events as housekeeping, not as a turn.
func newPersistentProc(stdin io.WriteCloser, stdout io.Reader, key SessionKey, model string, onUnsolicited func(SessionKey, SendResult)) *persistentProc {
	idle := make(chan struct{})
	close(idle)
	p := &persistentProc{
		stdin:         stdin,
		stdinW:        &syncWriter{w: stdin},
		stdout:        stdout,
		key:           key,
		model:         model,
		lines:         make(chan []byte, 256),
		pumpDone:      make(chan struct{}),
		idle:          idle,
		onUnsolicited: onUnsolicited,
		claimWait:     claimTurnMaxWait,
	}
	go p.readLoop()
	go p.pump()
	return p
}

// readLoop is the only reader of stdout. Each line is copied (the scanner
// reuses its buffer) and queued for the pump. Closes lines on EOF.
func (p *persistentProc) readLoop() {
	defer close(p.lines)
	sc := bufio.NewScanner(p.stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		p.lines <- cp
	}
	if err := sc.Err(); err != nil {
		slog.Debug("persistent: stdout read ended", "chat_id", p.key.ChatID, "thread_id", p.key.ThreadID, "error", err)
	}
}

// chanSource adapts the line channel to lineSource for parseEvents and
// notes turn boundaries as lines go by.
type chanSource struct {
	p   *persistentProc
	cur []byte
	eof bool
}

func (c *chanSource) Scan() bool {
	line, ok := <-c.p.lines
	if !ok {
		c.eof = true
		c.cur = nil
		return false
	}
	c.cur = line
	c.p.noteLine(line)
	return true
}

func (c *chanSource) Bytes() []byte { return c.cur }

// noteLine marks the turn open on the first event that is actually part of
// a turn. Init/control/keep-alive events never open one — otherwise the very
// first sendMessage would wait forever for "idle" behind the init handshake.
func (p *persistentProc) noteLine(line []byte) {
	var ev struct {
		Type    string         `json:"type"`
		Message *stdoutMessage `json:"message,omitempty"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	meaningful := false
	switch ev.Type {
	case "assistant", "stream_event":
		meaningful = true
	case "user":
		meaningful = ev.Message != nil && !isToolResultOnly(ev.Message)
	}
	if !meaningful {
		return
	}
	p.dispatch.Lock()
	if !p.turnOpen {
		p.turnOpen = true
		p.idle = make(chan struct{})
		if p.waiter == nil {
			// Nobody asked: the CLI started this turn itself. Keep the
			// process alive for it — an idle kill mid-continuation would
			// lose the follow-up silently, the failure this exists to fix.
			slog.Info("persistent: CLI-initiated turn started", "chat_id", p.key.ChatID, "thread_id", p.key.ThreadID)
			if p.idleTimer != nil {
				p.idleTimer.Reset(idleTimeout)
			}
		}
	}
	p.dispatch.Unlock()
}

// gatedObserver forwards tool lifecycle to the injector's turnState only for
// turns the bridge initiated; tools of a CLI-initiated turn are not on an
// injectable turn.
type gatedObserver struct{ p *persistentProc }

func (g gatedObserver) toolStarted() {
	g.p.dispatch.Lock()
	ours := g.p.waiter != nil
	g.p.dispatch.Unlock()
	if ours {
		g.p.turn.toolStarted()
	}
}

func (g gatedObserver) toolEnded() {
	g.p.dispatch.Lock()
	ours := g.p.waiter != nil
	g.p.dispatch.Unlock()
	if ours {
		g.p.turn.toolEnded()
	}
}

// emitCurrent routes stream events to the in-flight sendMessage's sink, or
// drops them while idle (a CLI-initiated turn streams to nobody; its text
// arrives whole in the result).
func (p *persistentProc) emitCurrent(ev StreamEvent) {
	p.dispatch.Lock()
	e := p.emit
	p.dispatch.Unlock()
	if e != nil {
		e(ev)
	}
}

// pump parses turns from the line stream forever. Each completed turn goes
// to the waiting sendMessage if there is one, else to onUnsolicited. On EOF
// a waiting sender gets an empty result (parity with the old scanner-ended
// shape, which the manager already treats as "retry fresh").
func (p *persistentProc) pump() {
	defer close(p.pumpDone)
	src := &chanSource{p: p}
	for {
		res := parseEvents(src, p.stdinW, p.emitCurrent, gatedObserver{p})

		p.dispatch.Lock()
		w := p.waiter
		p.waiter = nil
		p.emit = nil
		if p.turnOpen {
			p.turnOpen = false
			close(p.idle)
		}
		if w != nil {
			// Deliver under the lock: the waiter is buffered(1) and carries
			// exactly one result, so this cannot block, and an abandoning
			// sender that nils p.waiter under the same lock either finds the
			// result already in its channel or sees nil here and we route
			// it to the handler below — never a send into a channel nobody
			// will read.
			w <- res
		}
		p.dispatch.Unlock()

		switch {
		case w != nil:
			// delivered above
		case src.eof:
			// nothing to deliver
		case res.Text != "" || res.Usage != nil || len(res.ToolCalls) > 0:
			slog.Info("persistent: CLI-initiated turn finished, routing as follow-up",
				"chat_id", p.key.ChatID, "thread_id", p.key.ThreadID,
				"chars", len(res.Text), "tool_calls", len(res.ToolCalls))
			if p.onUnsolicited != nil {
				// Off the pump: delivery does store writes and a Telegram
				// send; blocking here would back up the reader and stall the
				// CLI on stdout.
				go p.onUnsolicited(p.key, res)
			}
		}
		if src.eof {
			return
		}
	}
}

// claimTurn waits until no turn is open on the process and, under the same
// lock as that check, registers the caller as the waiter for the next turn.
// Checking idle and claiming in two lock regions would leave a gap in which
// a CLI-initiated turn could open and then be handed to the new waiter — the
// off-by-one in a window of our own making. A CLI-initiated turn that is
// mid-stream finishes first and is delivered as a follow-up.
func (p *persistentProc) claimTurn(ctx context.Context, emit EventFunc) (chan SendResult, error) {
	for {
		p.dispatch.Lock()
		if !p.turnOpen {
			w := make(chan SendResult, 1)
			p.waiter = w
			p.emit = emit
			p.dispatch.Unlock()
			return w, nil
		}
		ch := p.idle
		p.dispatch.Unlock()
		slog.Info("persistent: waiting for CLI-initiated turn before sending", "chat_id", p.key.ChatID, "thread_id", p.key.ThreadID)
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.pumpDone:
			return nil, errors.New("persistent process exited")
		case <-time.After(p.claimWait):
			slog.Warn("persistent: CLI-initiated turn did not finish; abandoning process",
				"chat_id", p.key.ChatID, "thread_id", p.key.ThreadID, "waited", p.claimWait)
			return nil, fmt.Errorf("%w after %s", ErrStalledBehindCLITurn, p.claimWait)
		}
	}
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
		// Process is still running — reset idle timer. Reset returns false
		// when the AfterFunc already fired (or was stopped): the idle
		// callback is closing stdin right now, and writing to this proc
		// would hit "broken pipe" and fall back to a cold spawn after the
		// fact. Treat it as dead and spawn fresh instead of racing it.
		if proc.idleTimer.Reset(idleTimeout) {
			return proc, nil
		}
		// Reset on an already-fired AfterFunc re-arms it; Stop so the old
		// callback does not fire again in 10m against the replacement.
		proc.idleTimer.Stop()
		slog.Info("persistent process idle-expired during lookup, respawning",
			"chat_id", key.ChatID, "thread_id", key.ThreadID)
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

	proc := newPersistentProc(stdin, stdout, key, model, m.onUnsolicited)
	proc.cmd = cmd
	proc.stderr = stderr
	proc.cancel = cancel

	// Set up idle timer to kill the process if no messages arrive. Assigned
	// under dispatch because noteLine (pump goroutine, already running) reads
	// it there to keep a CLI-initiated turn alive.
	proc.dispatch.Lock()
	proc.idleTimer = time.AfterFunc(idleTimeout, func() {
		slog.Info("persistent process idle timeout", "chat_id", key.ChatID, "thread_id", key.ThreadID)
		proc.kill()
		// Compare-and-delete: getOrSpawn may already have replaced this
		// proc under the same key (it spawns fresh when it sees the timer
		// has fired). Deleting by key alone would evict the healthy
		// replacement and orphan its process.
		m.mu.Lock()
		if m.persistent[key] == proc {
			delete(m.persistent, key)
		}
		m.mu.Unlock()
	})
	proc.dispatch.Unlock()

	return proc, nil
}

// sendMessage sends a user message to the persistent process and streams the response.
// This is the persistent equivalent of runClaudeBidirectional.
func (p *persistentProc) sendMessage(ctx context.Context, req AgentRequest, emit EventFunc) (SendResult, error) {
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

	// A CLI-initiated turn may be mid-stream; let the pump finish and route
	// it as a follow-up, then claim the next turn as ours.
	waiter, err := p.claimTurn(ctx, emit)
	if err != nil {
		return SendResult{}, err
	}

	abandon := func() chan SendResult {
		p.dispatch.Lock()
		w := p.waiter
		p.waiter = nil
		p.emit = nil
		p.dispatch.Unlock()
		return w
	}

	// Send user message.
	if err := writeJSON(p.stdinW, newUserMessage(req, sessionID)); err != nil {
		abandon()
		return SendResult{}, fmt.Errorf("send user message: %w", err)
	}

	select {
	case result := <-waiter:
		// Update session ID if we got one (turn.mu: the injector reads it).
		if result.SessionID != "" {
			p.turn.mu.Lock()
			p.sessionID = result.SessionID
			p.turn.mu.Unlock()
		}
		return result, nil
	case <-p.pumpDone:
		// Reader hit EOF. The pump delivers an empty result to a waiter
		// before exiting, so drain it; an empty SendResult is what the old
		// code returned when the scanner ended, and the manager retries fresh.
		select {
		case result := <-waiter:
			return result, nil
		default:
			return SendResult{}, nil
		}
	case <-ctx.Done():
		// The caller gave up but the turn is still running. Detach: any
		// result that lands from here on is routed to onUnsolicited by the
		// pump (waiter is nil) — or by us, if it landed in the gap.
		if w := abandon(); w != nil {
			select {
			case late := <-w:
				if p.onUnsolicited != nil {
					go p.onUnsolicited(p.key, late)
				}
			default:
			}
		}
		return SendResult{}, fmt.Errorf("%w: %v", ErrTurnAbandoned, ctx.Err())
	}
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
func (m *Manager) sendPersistent(ctx context.Context, req AgentRequest, emit EventFunc) (SendResult, error) {
	proc, err := m.getOrSpawn(ctx, req)
	if err != nil {
		return SendResult{}, err
	}

	result, err := proc.sendMessage(ctx, req, emit)
	if errors.Is(err, ErrTurnAbandoned) {
		// The turn is still running in a healthy process; its result will be
		// delivered as a follow-up. Do not kill, do not fall back.
		slog.Warn("persistent turn abandoned by caller; result will arrive as follow-up", "chat_id", req.ChatID, "thread_id", req.MessageThreadID, "error", err)
		return SendResult{}, err
	}
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
