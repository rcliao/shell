package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/rcliao/teeny-relay/internal/bridge"
	"github.com/rcliao/teeny-relay/internal/config"
	"github.com/rcliao/teeny-relay/internal/memory"
	"github.com/rcliao/teeny-relay/internal/planner"
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
	memory *memory.Memory // nil if disabled
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

	// Initialize memory store if enabled
	var mem *memory.Memory
	if cfg.Memory.Enabled {
		mem, err = memory.New(cfg.Memory.DBPath, cfg.Memory.Budget, cfg.Memory.GlobalNamespaces, cfg.Memory.GlobalBudget)
		if err != nil {
			st.Close()
			return nil, fmt.Errorf("init memory store: %w", err)
		}
		slog.Info("memory store initialized",
			"db", cfg.Memory.DBPath,
			"budget", cfg.Memory.Budget,
			"global_namespaces", cfg.Memory.GlobalNamespaces,
			"global_budget", cfg.Memory.GlobalBudget,
		)
	}

	// Initialize planner if enabled
	var pl *planner.Planner
	if cfg.Planner.Enabled {
		pl = planner.New(planner.Config{
			ClaudeBinary:         cfg.Claude.Binary,
			Model:                cfg.Claude.Model,
			WorkDir:              cfg.Claude.WorkDir,
			TestCmd:              cfg.Planner.TestCmd,
			Conventions:          cfg.Planner.Conventions,
			MaxRetries:           cfg.Planner.MaxRetries,
			Timeout:              cfg.Planner.Timeout, // 0 → planner defaults to 30m
			AutoApproveThreshold: cfg.Planner.AutoApproveThreshold,
		})
		slog.Info("planner initialized", "test_cmd", cfg.Planner.TestCmd, "max_retries", cfg.Planner.MaxRetries)
	}

	// Create bridge
	br := bridge.New(proc, st, mem, pl)

	// Create auth
	auth := telegram.NewAuth(cfg.Telegram.AllowedUsers)

	// Create telegram bot
	token := cfg.TelegramToken()
	bot, err := telegram.NewBot(token, auth, br)
	if err != nil {
		st.Close()
		if mem != nil {
			mem.Close()
		}
		return nil, err
	}

	// Wire async notifications: plan progress → Telegram
	br.SetNotifier(func(chatID int64, msg string) {
		bot.SendText(chatID, msg)
	})

	return &Daemon{
		cfg:    cfg,
		bot:    bot,
		bridge: br,
		proc:   proc,
		store:  st,
		memory: mem,
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
		"memory_enabled", d.cfg.Memory.Enabled,
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
	if d.memory != nil {
		d.memory.Close()
	}
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
			HasHistory:      true,
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
