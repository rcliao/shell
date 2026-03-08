package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rcliao/teeny-relay/internal/bridge"
	"github.com/rcliao/teeny-relay/internal/browser"
	"github.com/rcliao/teeny-relay/internal/config"
	"github.com/rcliao/teeny-relay/internal/imagen"
	"github.com/rcliao/teeny-relay/internal/memory"
	"github.com/rcliao/teeny-relay/internal/planner"
	"github.com/rcliao/teeny-relay/internal/process"
	"github.com/rcliao/teeny-relay/internal/reload"
	"github.com/rcliao/teeny-relay/internal/scheduler"
	"github.com/rcliao/teeny-relay/internal/store"
	"github.com/rcliao/teeny-relay/internal/telegram"
)

type Daemon struct {
	cfg       config.Config
	bot       *telegram.Bot
	bridge    *bridge.Bridge
	proc      *process.Manager
	store     *store.Store
	memory    *memory.Memory       // nil if disabled
	reloader  *reload.Watcher      // nil if disabled
	scheduler *scheduler.Scheduler // nil if disabled
}

func New(cfg config.Config) (*Daemon, error) {
	// Open encrypted secret store (if enabled) before anything reads tokens.
	config.OpenSecretStore(cfg.Secrets)

	// Export search API keys into env so child processes (Claude → Bash → search) inherit them.
	for _, key := range []string{"BRAVE_SEARCH_API_KEY", "TAVILY_API_KEY"} {
		if val := cfg.Secret(key); val != "" && os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}

	// Open store
	st, err := store.Open(cfg.Store.DBPath)
	if err != nil {
		return nil, err
	}

	// Create process manager
	proc := process.NewManager(process.ManagerConfig{
		Binary:       cfg.Claude.Binary,
		Model:        cfg.Claude.Model,
		Timeout:      cfg.Claude.Timeout,
		MaxSessions:  cfg.Claude.MaxSessions,
		WorkDir:      cfg.Claude.WorkDir,
		AllowedTools: cfg.Claude.AllowedTools,
		ExtraArgs:    cfg.Claude.ExtraArgs,
	})

	// If scheduler is enabled, inject relay:capabilities into system namespaces
	// so schedule docs are always visible in the system prompt.
	if cfg.Scheduler.Enabled {
		capNS := "relay:capabilities"
		if !containsStr(cfg.Memory.SystemNamespaces, capNS) {
			cfg.Memory.SystemNamespaces = append(cfg.Memory.SystemNamespaces, capNS)
		}
		for name, p := range cfg.Memory.Profiles {
			if !containsStr(p.SystemNamespaces, capNS) {
				p.SystemNamespaces = append(p.SystemNamespaces, capNS)
				cfg.Memory.Profiles[name] = p
			}
		}
	}

	// Initialize memory store if enabled
	var mem *memory.Memory
	if cfg.Memory.Enabled {
		// Convert config profiles to memory ProfileConfig
		profiles := make(map[string]memory.ProfileConfig, len(cfg.Memory.Profiles))
		for name, p := range cfg.Memory.Profiles {
			profiles[name] = memory.ProfileConfig{
				SystemNamespaces: p.SystemNamespaces,
				SystemBudget:     p.SystemBudget,
				GlobalNamespaces: p.GlobalNamespaces,
				GlobalBudget:     p.GlobalBudget,
				Budget:           p.Budget,
				ExchangeTTL:      p.ExchangeTTL,
				ExchangeMaxUser:  p.ExchangeMaxUser,
				ExchangeMaxReply: p.ExchangeMaxReply,
				MemoryDirectives: p.MemoryDirectives,
				DirectiveNS:      p.DirectiveNS,
			}
		}
		chatProfiles := cfg.Memory.ChatProfileMap()

		mem, err = memory.New(cfg.Memory.DBPath, cfg.Memory.Budget, cfg.Memory.GlobalNamespaces, cfg.Memory.GlobalBudget, cfg.Memory.SystemNamespaces, cfg.Memory.SystemBudget, profiles, chatProfiles)
		if err != nil {
			st.Close()
			return nil, fmt.Errorf("init memory store: %w", err)
		}
		slog.Info("memory store initialized",
			"db", cfg.Memory.DBPath,
			"budget", cfg.Memory.Budget,
			"global_namespaces", cfg.Memory.GlobalNamespaces,
			"global_budget", cfg.Memory.GlobalBudget,
			"system_namespaces", cfg.Memory.SystemNamespaces,
			"system_budget", cfg.Memory.SystemBudget,
			"profiles", len(profiles),
			"chat_profiles", len(chatProfiles),
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
			VerifyInstructions:   cfg.Planner.VerifyInstructions,
			MaxRetries:           cfg.Planner.MaxRetries,
			Timeout:              cfg.Planner.Timeout, // 0 → planner defaults to 30m
			AutoApproveThreshold: cfg.Planner.AutoApproveThreshold,
		})
		slog.Info("planner initialized", "test_cmd", cfg.Planner.TestCmd, "max_retries", cfg.Planner.MaxRetries)
	}

	// Initialize image generator if Google API key is configured.
	var ig *imagen.Generator
	if apiKey := cfg.GoogleAPIKey(); apiKey != "" {
		var err error
		ig, err = imagen.New(apiKey, cfg.Google.Model, cfg.Google.Timeout)
		if err != nil {
			slog.Warn("imagen: failed to initialize", "error", err)
		} else {
			slog.Info("imagen initialized", "model", cfg.Google.Model)
		}
	}

	// Create bridge
	braveKey := cfg.Secret("BRAVE_SEARCH_API_KEY")
	tavilyKey := cfg.Secret("TAVILY_API_KEY")
	browserCfg := browser.Config{
		Enabled:        cfg.Browser.Enabled,
		Headless:       cfg.Browser.Headless,
		TimeoutSeconds: cfg.Browser.TimeoutSeconds,
		ChromePath:     cfg.Browser.ChromePath,
	}
	br := bridge.New(proc, st, mem, pl, cfg.Planner.Worktree, cfg.Claude.WorkDir, cfg.Telegram.ReactionMap, ig, braveKey, tavilyKey, browserCfg)
	if braveKey != "" {
		slog.Info("search initialized", "provider", "brave")
	} else if tavilyKey != "" {
		slog.Info("search initialized", "provider", "tavily")
	}

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

	// Wire image sender: generate-image directive → Telegram photo
	br.SetImageSender(func(chatID int64, imageData []byte, caption string) {
		bot.SendPhoto(chatID, imageData, caption)
	})

	// Wire chat action sender: upload_photo indicators for relay image generation
	br.SetChatAction(func(chatID int64, action string) {
		bot.SendChatAction(chatID, action)
	})

	// Wire async notifications: plan progress → Telegram
	br.SetNotifier(func(chatID int64, msg string) {
		bot.SendText(chatID, msg)
	})

	// Register cron parser for bridge schedule commands/directives.
	bridge.SetCronParser(func(expr string) (interface{ Next(time.Time) time.Time }, error) {
		return scheduler.ParseCron(expr)
	})

	// Initialize scheduler if enabled.
	var sched *scheduler.Scheduler
	if cfg.Scheduler.Enabled {
		adapter := scheduler.NewStoreAdapter(st)
		onNotify := func(chatID int64, msg string) {
			bot.SendText(chatID, msg)
		}
		onPrompt := func(chatID int64, msg string) {
			// Route through bridge as if user sent it
			resp, err := br.HandleMessageStreaming(context.Background(), chatID, msg, "scheduler", nil, nil, nil)
			if err != nil {
				slog.Error("scheduler prompt failed", "chat_id", chatID, "error", err)
				return
			}
			bot.SendText(chatID, resp)
		}
		sched = scheduler.New(adapter, onNotify, onPrompt, cfg.Scheduler.Timezone)
		sched.SetQuietHours(cfg.Scheduler.QuietHourStart, cfg.Scheduler.QuietHourEnd)
		sched.SetHeartbeatPrompt(func(chatID int64, msg string) (string, error) {
			return br.HandleMessageStreaming(context.Background(), chatID, msg, "heartbeat", nil, nil, nil)
		})
		br.SetSchedulerConfig(true, cfg.Scheduler.Timezone)
		slog.Info("scheduler initialized", "timezone", cfg.Scheduler.Timezone, "quiet_hours", fmt.Sprintf("%d:00-%d:00", cfg.Scheduler.QuietHourStart, cfg.Scheduler.QuietHourEnd))

		// Seed schedule docs into memory so they're always visible in system prompt.
		if mem != nil {
			if err := mem.SeedNamespace(context.Background(), "relay:capabilities", "scheduling",
				"Schedule capabilities: Use [schedule at=\"...\"] for one-shot or [schedule cron=\"...\"] for recurring reminders. "+
					"Commands: /schedule add|list|delete|enable|pause. Supports @daily, @hourly, @weekly, @monthly aliases. "+
					"Use --tz flag for per-schedule timezone override."); err != nil {
				slog.Warn("failed to seed schedule docs", "error", err)
			}
			if err := mem.SeedNamespace(context.Background(), "relay:capabilities", "heartbeat-learning",
				"Heartbeat self-improvement: During [Heartbeat] check-ins, recent conversations and previous "+
					"insights are provided. Use [heartbeat-learning]...[/heartbeat-learning] to store reusable "+
					"patterns discovered during heartbeats."); err != nil {
				slog.Warn("failed to seed heartbeat-learning docs", "error", err)
			}
		}
	}

	d := &Daemon{
		cfg:       cfg,
		bot:       bot,
		bridge:    br,
		proc:      proc,
		store:     st,
		memory:    mem,
		scheduler: sched,
	}

	// Resolve source directory for reload and self-restart.
	sourceDir := cfg.Reload.SourceDir
	if sourceDir == "" {
		exe, err := os.Executable()
		if err == nil {
			sourceDir, err = reload.FindSourceDir(filepath.Dir(exe))
		}
		if err != nil {
			slog.Debug("could not auto-detect source dir", "error", err)
		}
	}

	// Initialize live reloader if enabled.
	if cfg.Reload.Enabled && sourceDir != "" {
		debounce := 500 * time.Millisecond
		if cfg.Reload.Debounce != "" {
			if parsed, err := time.ParseDuration(cfg.Reload.Debounce); err == nil {
				debounce = parsed
			}
		}
		rw, err := reload.New(reload.Config{
			SourceDir:  sourceDir,
			Debounce:   debounce,
			OnShutdown: d.Shutdown,
		})
		if err != nil {
			slog.Warn("reload: failed to create watcher", "error", err)
		} else {
			d.reloader = rw
			slog.Info("reload: live reload enabled", "source_dir", sourceDir, "debounce", debounce)
		}
	}

	// Wire self-restart: when a plan modifies relay source, rebuild and restart.
	if sourceDir != "" {
		binaryPath, _ := os.Executable()
		br.SetSelfRestart(sourceDir, func() {
			slog.Info("self-restart: rebuilding relay...")
			if err := reload.RebuildAndRestart(binaryPath, "./cmd/relay", d.Shutdown); err != nil {
				slog.Error("self-restart: failed", "error", err)
			}
		})
		slog.Info("self-restart enabled", "source_dir", sourceDir)
	}

	return d, nil
}

// Run starts the daemon and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	// Write PID file
	if err := writePID(d.cfg.Daemon.PIDFile); err != nil {
		slog.Warn("failed to write PID file", "path", d.cfg.Daemon.PIDFile, "error", err)
	}

	// Start live reloader if enabled.
	if d.reloader != nil {
		go func() {
			if err := d.reloader.Run(ctx.Done()); err != nil {
				slog.Error("reload: watcher stopped", "error", err)
			}
		}()
	}

	// Start scheduler if enabled.
	if d.scheduler != nil {
		go d.scheduler.Run(ctx)
	}

	// Start stale session cleanup ticker
	go d.cleanupLoop(ctx)

	// Restore active sessions from store
	d.restoreSessions()

	// Ensure default heartbeats for existing sessions
	d.bridge.EnsureDefaultHeartbeats()

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
	config.CloseSecretStore()
	removePID(d.cfg.Daemon.PIDFile)
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

// writePID writes the current process ID to the given path.
func writePID(path string) error {
	if path == "" {
		return nil
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0644)
}

// removePID removes the PID file at the given path.
func removePID(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to remove PID file", "path", path, "error", err)
	}
}

// ReadPID reads a PID from the given file path.
func ReadPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// containsStr checks if a string slice contains a given value.
func containsStr(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
