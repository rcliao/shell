// Package browserhandoff lets an agent hand its browser tab to a person, or
// let them watch it, through a tailnet-only live view
// (docs/DESIGN-BROWSER-HANDOFF.md).
//
// Story: the agent drives a named browser session with the browser skill,
// hits a step only a person can do (captcha, approval, login) and calls the
// shell_browser tool. The Manager records a handoff, serves a live view of the
// session's tab on a loopback port, publishes it at /h/<token> with tailscale
// serve, posts the agent's message and the link to the chat, and holds the
// tab so the agent cannot drive it meanwhile. When the person taps Done — or
// the link expires — the Manager takes the view down and starts a new agent
// turn in the same chat and thread, saying where the tab ended up.
package browserhandoff

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// Store is the slice of the shell store the Manager needs.
type Store interface {
	AddBrowserHandoff(store.BrowserHandoff) (int64, error)
	SetBrowserHandoffLink(id int64, link string) error
	EndBrowserHandoff(id int64, status, finalURL, endedBy, note string) (bool, error)
	GetBrowserHandoff(id int64) (*store.BrowserHandoff, error)
	OpenBrowserHandoffs() ([]store.BrowserHandoff, error)
}

// Publisher makes a loopback port reachable at a path on the tailnet.
type Publisher interface {
	Publish(ctx context.Context, path string, port int) (link string, err error)
	Unpublish(ctx context.Context, path string) error
}

// View is a live view of one session tab (package liveview in production).
type View interface {
	http.Handler
	URL() string
	Close(message string)
}

// ViewSpec describes the view to open.
type ViewSpec struct {
	Session   string
	Mode      string
	Title     string
	Reason    string
	ExpiresAt time.Time
	OnDone    func(finalURL, by, note string)
}

// Sessions is the agent's browser sessions (package session in production).
type Sessions interface {
	// NewView attaches a live view to a RUNNING session's current tab.
	NewView(ctx context.Context, spec ViewSpec) (View, error)
	// Hold marks the session's tab as held by a person until `until`;
	// the browser skill refuses to drive it meanwhile.
	Hold(session string, handoffID int64, reason string, until time.Time) error
	Release(session string)
	// Touch marks the session used now, so the idle reaper counts from the
	// end of a handoff rather than from the agent's last run before it.
	Touch(session string)
	// ReapIdle closes sessions unused for longer than idle, except busy ones.
	ReapIdle(ctx context.Context, idle time.Duration, busy map[string]bool) []string
}

// Config wires a Manager.
type Config struct {
	AgentName  string // shown on the page, e.g. "pikamini"
	DefaultTTL time.Duration
	MaxTTL     time.Duration
	Store      Store
	Publisher  Publisher
	Sessions   Sessions
	// Notify posts text with one link button to a chat/thread.
	Notify func(chatID, threadID int64, text, buttonLabel, link string) error
	// Resume starts an agent turn in chat/thread with prompt. Called in its
	// own goroutine; it may block for a long time (busy-session retries).
	Resume func(chatID, threadID int64, prompt string)
	Now    func() time.Time
}

// OpenRequest is what the agent asks for.
type OpenRequest struct {
	Session  string
	Mode     string // store.HandoffModeHandoff (default) or store.HandoffModeWatch
	Reason   string // what the person should do; shown on the page
	Message  string // posted to the chat with the link, in the chat's language
	ChatID   int64
	ThreadID int64 // real thread (callers map lane threads first)
	TTL      time.Duration
}

type live struct {
	h     store.BrowserHandoff
	view  View
	srv   *http.Server
	timer *time.Timer
}

// Manager owns every open handoff of one agent.
type Manager struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	active map[int64]*live
	// starting holds sessions whose view is being brought up (publish can
	// take seconds), so a concurrent Open for the same session is refused.
	starting map[string]bool
}

// New returns a Manager. Call Recover once at startup.
func New(cfg Config) *Manager {
	if cfg.DefaultTTL <= 0 {
		cfg.DefaultTTL = 10 * time.Minute
	}
	if cfg.MaxTTL <= 0 {
		cfg.MaxTTL = 60 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Notify == nil {
		cfg.Notify = func(int64, int64, string, string, string) error { return nil }
	}
	if cfg.Resume == nil {
		cfg.Resume = func(int64, int64, string) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{cfg: cfg, ctx: ctx, cancel: cancel, active: map[int64]*live{}, starting: map[string]bool{}}
}

// ErrBusy is returned when the session already has an open view.
var ErrBusy = errors.New("session already has an open live view")

// Open starts a handoff or watch view and posts the link to the chat.
func (m *Manager) Open(ctx context.Context, req OpenRequest) (*store.BrowserHandoff, error) {
	if req.Mode == "" {
		req.Mode = store.HandoffModeHandoff
	}
	if req.Mode != store.HandoffModeHandoff && req.Mode != store.HandoffModeWatch {
		return nil, fmt.Errorf("mode must be %q or %q", store.HandoffModeHandoff, store.HandoffModeWatch)
	}
	if strings.TrimSpace(req.Session) == "" {
		return nil, errors.New("session is required (the --session name you have been driving)")
	}
	if req.ChatID == 0 {
		return nil, errors.New("no chat to send the link to (this turn has no chat)")
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = m.cfg.DefaultTTL
	}
	if ttl < time.Minute {
		ttl = time.Minute
	}
	if ttl > m.cfg.MaxTTL {
		ttl = m.cfg.MaxTTL
	}

	if l, busy := m.reserve(req.Session); busy {
		if l != nil {
			return &l.h, fmt.Errorf("%w: #%d (%s) for session %q, link %s", ErrBusy, l.h.ID, l.h.Mode, req.Session, l.h.Link)
		}
		return nil, fmt.Errorf("%w: one is being opened for session %q right now", ErrBusy, req.Session)
	}
	defer m.unreserve(req.Session)

	tok, err := token()
	if err != nil {
		return nil, err
	}
	h := store.BrowserHandoff{
		Session: req.Session, Mode: req.Mode, ChatID: req.ChatID, ThreadID: req.ThreadID,
		Reason: req.Reason, Path: "/h/" + tok, ExpiresAt: m.cfg.Now().Add(ttl),
	}
	h.ID, err = m.cfg.Store.AddBrowserHandoff(h)
	if err != nil {
		return nil, err
	}
	h.Status = store.HandoffOpen
	if err := m.start(ctx, &h); err != nil {
		_, _ = m.cfg.Store.EndBrowserHandoff(h.ID, store.HandoffFailed, "", "", "")
		return nil, err
	}

	label := "Open the browser"
	if h.Mode == store.HandoffModeWatch {
		label = "Watch live"
	}
	text := strings.TrimSpace(req.Message)
	if text == "" {
		text = defaultMessage(h)
	}
	if err := m.cfg.Notify(h.ChatID, h.ThreadID, text, label, h.Link); err != nil {
		// The view is up; the agent still has the link and can post it itself.
		slog.Warn("browser handoff: posting the link failed", "id", h.ID, "error", err)
	}
	slog.Info("browser handoff: opened", "id", h.ID, "session", h.Session, "mode", h.Mode, "expires", h.ExpiresAt)
	return &h, nil
}

// reserve claims session for an Open/Recover in progress. busy is true when
// it already has a live view (returned) or another start is in flight.
func (m *Manager) reserve(session string) (*live, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.active {
		if l.h.Session == session {
			return l, true
		}
	}
	if m.starting[session] {
		return nil, true
	}
	m.starting[session] = true
	return nil, false
}

func (m *Manager) unreserve(session string) {
	m.mu.Lock()
	delete(m.starting, session)
	m.mu.Unlock()
}

func defaultMessage(h store.BrowserHandoff) string {
	when := h.ExpiresAt.Local().Format("15:04")
	if h.Mode == store.HandoffModeWatch {
		return fmt.Sprintf("Live view of my browser (Tailscale only, until %s).", when)
	}
	if h.Reason != "" {
		return fmt.Sprintf("Have a look in my browser: %s\nOpen the link on a device signed in to Tailscale and tap Done when you're finished (add a note if you like). It expires at %s.", h.Reason, when)
	}
	return fmt.Sprintf("Have a look in my browser. Open the link on a device signed in to Tailscale and tap Done when you're finished. It expires at %s.", when)
}

// start brings up the view, server, tailnet path, hold and expiry timer for h
// and records it as active. On error everything it started is torn down.
func (m *Manager) start(ctx context.Context, h *store.BrowserHandoff) error {
	id := h.ID
	title := "Live browser"
	if h.Mode == store.HandoffModeHandoff {
		title = "Shared browser"
	}
	if m.cfg.AgentName != "" {
		title += " · " + m.cfg.AgentName
	}
	view, err := m.cfg.Sessions.NewView(m.ctx, ViewSpec{
		Session: h.Session, Mode: h.Mode, Title: title, Reason: h.Reason, ExpiresAt: h.ExpiresAt,
		OnDone: func(finalURL, by, note string) {
			go m.finish(id, ending{status: store.HandoffDone, url: finalURL, by: by, note: note})
		},
	})
	if err != nil {
		return fmt.Errorf("open live view of session %q: %w", h.Session, err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		view.Close("")
		return err
	}
	srv := &http.Server{Handler: view, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	port := ln.Addr().(*net.TCPAddr).Port

	link, err := m.cfg.Publisher.Publish(ctx, h.Path, port)
	if err != nil {
		view.Close("")
		_ = srv.Close()
		return err
	}
	h.Link = link
	if err := m.cfg.Store.SetBrowserHandoffLink(id, link); err != nil {
		slog.Warn("browser handoff: storing link failed", "id", id, "error", err)
	}
	if h.Mode == store.HandoffModeHandoff {
		if err := m.cfg.Sessions.Hold(h.Session, id, h.Reason, h.ExpiresAt); err != nil {
			slog.Warn("browser handoff: hold failed; the agent could drive the tab meanwhile", "id", id, "error", err)
		}
	}
	wait := h.ExpiresAt.Sub(m.cfg.Now())
	if wait < 0 {
		wait = 0
	}
	// Register before arming the timer: an expiry that fires at once (a
	// recovered row that ran out during Publish) must find the entry, or it
	// ends the row without closing the view and the entry is never removed.
	l := &live{h: *h, view: view, srv: srv}
	m.mu.Lock()
	m.active[id] = l
	l.timer = time.AfterFunc(wait, func() { m.finish(id, ending{status: store.HandoffExpired}) })
	m.mu.Unlock()
	return nil
}

// finish ends handoff id exactly once: the store decides who wins a
// Done-vs-expiry race. Then it takes the view down and, for a handoff,
// resumes the agent.
// ending is how a handoff ended: a final status, plus what the person left
// (page, name, note) on Done, or why it failed.
type ending struct {
	status string
	url    string // page the tab ended on ("" = ask the view)
	by     string // who tapped Done (tailnet user), if known
	note   string // what they typed for the agent, if anything
	detail string // failure reason, for HandoffFailed
}

func (m *Manager) finish(id int64, e ending) {
	status, finalURL := e.status, e.url
	m.mu.Lock()
	l := m.active[id]
	m.mu.Unlock()

	var h store.BrowserHandoff
	if l != nil {
		h = l.h
		if finalURL == "" {
			finalURL = l.view.URL()
		}
	} else {
		row, err := m.cfg.Store.GetBrowserHandoff(id)
		if err != nil || row == nil {
			return
		}
		h = *row
	}

	ended, err := m.cfg.Store.EndBrowserHandoff(id, status, finalURL, e.by, e.note)
	if err != nil {
		slog.Error("browser handoff: ending failed", "id", id, "status", status, "error", err)
		return
	}
	if !ended {
		return // already finished by the other path
	}

	m.mu.Lock()
	delete(m.active, id)
	var timer *time.Timer
	if l != nil {
		timer = l.timer
	}
	m.mu.Unlock()
	if l != nil {
		if timer != nil {
			timer.Stop()
		}
		l.view.Close(endMessage(status))
		_ = l.srv.Close()
	}
	uctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := m.cfg.Publisher.Unpublish(uctx, h.Path); err != nil {
		slog.Warn("browser handoff: unpublish failed", "id", id, "path", h.Path, "error", err)
	}
	cancel()
	if h.Mode == store.HandoffModeHandoff {
		m.cfg.Sessions.Release(h.Session)
	}
	m.cfg.Sessions.Touch(h.Session)
	slog.Info("browser handoff: ended", "id", id, "status", status)

	if h.Mode == store.HandoffModeHandoff && status != store.HandoffCancelled {
		h.Status, h.FinalURL, h.EndedBy, h.Note = status, finalURL, e.by, e.note
		prompt := ResumePrompt(h, e.detail)
		go m.cfg.Resume(h.ChatID, h.ThreadID, prompt)
	}
}

func endMessage(status string) string {
	switch status {
	case store.HandoffDone:
		return "Thanks. The agent has the browser back. You can close this page."
	case store.HandoffExpired:
		return "This link has expired."
	case store.HandoffCancelled:
		return "The agent closed this view."
	}
	return "This view has ended."
}

// ResumePrompt is the turn the agent gets when a handoff ends. It starts with
// "[" like the other system-originated turns. It reports what happened and
// leaves the next move to the agent: a handoff may have been a captcha to
// clear, a find to look at, or "I've done what I can" — the follow-up may be
// to continue, to answer, or nothing at all.
func ResumePrompt(h store.BrowserHandoff, detail string) string {
	at := h.FinalURL
	if at == "" {
		at = "(unknown page)"
	}
	look := fmt.Sprintf("`browser --session %s - snapshot`", h.Session)
	switch h.Status {
	case store.HandoffDone:
		who := h.EndedBy
		if who == "" {
			who = "The person"
		}
		said := "They left no note."
		if strings.TrimSpace(h.Note) != "" {
			said = fmt.Sprintf("Their note to you: %q.", h.Note)
		}
		return fmt.Sprintf("[Browser handoff #%d done: %s tapped Done. %s The tab (session %q) is at %s. "+
			"You shared it because: %s. Decide what follows — continue in the tab (%s), answer them, or nothing if they are set.]",
			h.ID, who, said, h.Session, at, orDash(h.Reason), look)
	case store.HandoffExpired:
		return fmt.Sprintf("[Browser handoff #%d expired at %s without anyone tapping Done. The tab (session %q) is still open at %s; "+
			"%s shows what, if anything, they did. You shared it because: %s. Decide whether a follow-up is worth it.]",
			h.ID, h.ExpiresAt.Local().Format("15:04"), h.Session, at, look, orDash(h.Reason))
	}
	return fmt.Sprintf("[Browser handoff #%d stopped: %s. Session %q may be closed. Tell the chat the live link no longer works.]",
		h.ID, orDash(detail), h.Session)
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// Cancel ends an open handoff or watch view without resuming the agent.
func (m *Manager) Cancel(id int64) error {
	row, err := m.cfg.Store.GetBrowserHandoff(id)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("no browser handoff #%d", id)
	}
	if row.Status != store.HandoffOpen {
		return fmt.Errorf("browser handoff #%d is already %s", id, row.Status)
	}
	m.finish(id, ending{status: store.HandoffCancelled})
	return nil
}

// Get returns one handoff.
func (m *Manager) Get(id int64) (*store.BrowserHandoff, error) {
	return m.cfg.Store.GetBrowserHandoff(id)
}

// Active lists the open views this daemon is serving.
func (m *Manager) Active() []store.BrowserHandoff {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.BrowserHandoff, 0, len(m.active))
	for _, l := range m.active {
		out = append(out, l.h)
	}
	return out
}

// Recover runs at daemon start. Open rows from before a restart are either
// re-served at the SAME path (the link already in the chat keeps working) or,
// past their expiry, ended and resumed as expired.
func (m *Manager) Recover(ctx context.Context) {
	rows, err := m.cfg.Store.OpenBrowserHandoffs()
	if err != nil {
		slog.Error("browser handoff: recover: listing open handoffs failed", "error", err)
		return
	}
	for i := range rows {
		h := rows[i]
		if !m.cfg.Now().Before(h.ExpiresAt) {
			m.finish(h.ID, ending{status: store.HandoffExpired})
			continue
		}
		if _, busy := m.reserve(h.Session); busy {
			m.finish(h.ID, ending{status: store.HandoffFailed, detail: "another live view of the same session was already open"})
			continue
		}
		err := m.start(ctx, &h)
		m.unreserve(h.Session)
		if err != nil {
			slog.Warn("browser handoff: recover failed", "id", h.ID, "error", err)
			m.finish(h.ID, ending{status: store.HandoffFailed, detail: "the browser could not be reopened after a restart (" + err.Error() + ")"})
			continue
		}
		slog.Info("browser handoff: recovered", "id", h.ID, "session", h.Session)
	}
}

// Shutdown stops serving for a restart. It does NOT end handoffs or remove
// their tailnet paths: the next daemon's Recover re-serves them.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	all := m.active
	m.active = map[int64]*live{}
	m.mu.Unlock()
	for _, l := range all {
		if l.timer != nil {
			l.timer.Stop()
		}
		l.view.Close("The agent is restarting. Reload this page in a few seconds.")
		_ = l.srv.Close()
	}
	m.cancel()
}

// RunReaper closes idle sessions every `every` until ctx ends, so a browser
// window does not stay open on the host for days.
func (m *Manager) RunReaper(ctx context.Context, every, idle time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			busy := map[string]bool{}
			for _, h := range m.Active() {
				busy[h.Session] = true
			}
			if closed := m.cfg.Sessions.ReapIdle(ctx, idle, busy); len(closed) > 0 {
				slog.Info("browser sessions: closed idle", "sessions", closed)
			}
		}
	}
}

func token() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
