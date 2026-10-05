package browserhandoff

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	shellbrowser "github.com/rcliao/shell-browser"
	"github.com/rcliao/shell-browser/liveview"
	"github.com/rcliao/shell-browser/session"
)

// ChromeSessions is Sessions over the agent's real browser sessions
// (one Chrome per session name under Root).
type ChromeSessions struct {
	Root   string               // the agent's sessions dir (SHELL_BROWSER_SESSIONS for its children)
	Policy *shellbrowser.Policy // gates the live view's address bar; nil = DefaultPolicy
}

func (c *ChromeSessions) open(name string) (*session.Session, error) {
	return session.Open(c.Root, name)
}

// NewView implements Sessions.
func (c *ChromeSessions) NewView(ctx context.Context, spec ViewSpec) (View, error) {
	s, err := c.open(spec.Session)
	if err != nil {
		return nil, err
	}
	ep := s.Running(ctx)
	if ep == nil {
		return nil, fmt.Errorf("session %q has no browser running — open the page with `browser --session %s <url>` first", spec.Session, spec.Session)
	}
	mode := liveview.ModeHandoff
	if spec.Mode == "watch" {
		mode = liveview.ModeWatch
	}
	onDone := spec.OnDone
	v, err := liveview.New(ctx, liveview.Options{
		Session: s, Endpoint: ep, Mode: mode, Title: spec.Title, Reason: spec.Reason,
		ExpiresAt: spec.ExpiresAt, Policy: c.Policy,
		OnDone: func(r liveview.Result) {
			if onDone != nil {
				onDone(r.URL, r.By)
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return v, nil
}

// Hold implements Sessions.
func (c *ChromeSessions) Hold(name string, id int64, reason string, until time.Time) error {
	s, err := c.open(name)
	if err != nil {
		return err
	}
	return s.SetLock(session.Lock{HandoffID: id, Reason: reason, ExpiresAt: until})
}

// Release implements Sessions.
func (c *ChromeSessions) Release(name string) {
	if s, err := c.open(name); err == nil {
		s.ClearLock()
	}
}

// Touch implements Sessions.
func (c *ChromeSessions) Touch(name string) {
	if s, err := c.open(name); err == nil {
		s.Touch()
	}
}

// ReapIdle implements Sessions.
func (c *ChromeSessions) ReapIdle(ctx context.Context, idle time.Duration, busy map[string]bool) []string {
	all, err := session.List(c.Root)
	if err != nil {
		slog.Warn("browser sessions: list failed", "error", err)
		return nil
	}
	var closed []string
	now := time.Now()
	for _, s := range all {
		if busy[s.Name] {
			continue
		}
		if _, held := s.HeldBy(now); held {
			continue
		}
		last := s.LastUsed()
		if last.IsZero() || now.Sub(last) < idle {
			continue
		}
		ep := s.Running(ctx)
		if ep == nil {
			continue
		}
		if err := session.Close(ctx, ep); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("browser sessions: close failed", "session", s.Name, "error", err)
			continue
		}
		closed = append(closed, s.Name)
	}
	return closed
}
