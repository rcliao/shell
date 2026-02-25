package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/rcliao/teeny-relay/internal/bridge"
	"github.com/rcliao/teeny-relay/internal/config"
	"github.com/rcliao/teeny-relay/internal/process"
	"github.com/rcliao/teeny-relay/internal/store"
	"github.com/rcliao/teeny-relay/internal/telegram"
)

type Daemon struct {
	cfg    config.Config
	bot    *telegram.Bot
	bridge *bridge.Bridge
	proc   *process.Manager
	store  *store.Store
}

func New(cfg config.Config) (*Daemon, error) {
	// Open store
	st, err := store.Open(cfg.Store.DBPath)
	if err != nil {
		return nil, err
	}

	// Create process manager
	proc := process.NewManager(process.ManagerConfig{
		Binary:      cfg.Claude.Binary,
		Model:       cfg.Claude.Model,
		Timeout:     cfg.Claude.Timeout,
		MaxSessions: cfg.Claude.MaxSessions,
		WorkDir:     cfg.Claude.WorkDir,
		ExtraArgs:   cfg.Claude.ExtraArgs,
	})

	// Create bridge
	br := bridge.New(proc, st)

	// Create auth
	auth := telegram.NewAuth(cfg.Telegram.AllowedUsers)

	// Create telegram bot
	token := cfg.TelegramToken()
	bot, err := telegram.NewBot(token, auth, br)
	if err != nil {
		st.Close()
		return nil, err
	}

	return &Daemon{
		cfg:    cfg,
		bot:    bot,
		bridge: br,
		proc:   proc,
		store:  st,
	}, nil
}

// Run starts the daemon and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	// Start stale session cleanup ticker
	go d.cleanupLoop(ctx)

	// Restore active sessions from store
	d.restoreSessions()

	slog.Info("daemon starting",
		"allowed_users", len(d.cfg.Telegram.AllowedUsers),
		"max_sessions", d.cfg.Claude.MaxSessions,
		"timeout", d.cfg.Claude.Timeout,
	)

	// Start bot (blocks until ctx is cancelled)
	d.bot.Start(ctx)
	return nil
}

// Shutdown gracefully stops all components.
func (d *Daemon) Shutdown() {
	slog.Info("daemon shutting down")
	d.proc.KillAll()
	d.store.Close()
	slog.Info("daemon stopped")
}

func (d *Daemon) restoreSessions() {
	sessions, err := d.store.ListActiveSessions()
	if err != nil {
		slog.Warn("failed to restore sessions", "error", err)
		return
	}
	for _, sess := range sessions {
		procSess := &process.Session{
			ID:              fmt.Sprintf("%d", sess.ID),
			ChatID:          sess.ChatID,
			ClaudeSessionID: sess.ClaudeSessionID,
			Status:          process.StatusActive,
			CreatedAt:       sess.CreatedAt,
			UpdatedAt:       sess.UpdatedAt,
		}
		d.proc.Register(procSess)
	}
	if len(sessions) > 0 {
		slog.Info("restored sessions from store", "count", len(sessions))
	}
}

func (d *Daemon) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.bridge.CleanupStaleSessions(1 * time.Hour); err != nil {
				slog.Warn("stale session cleanup failed", "error", err)
			}
		}
	}
}
