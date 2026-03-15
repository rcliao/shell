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

	"github.com/rcliao/shell/internal/bridge"
	pm "github.com/rcliao/shell-pm"
	tunnel "github.com/rcliao/shell-tunnel"
	"github.com/rcliao/shell/internal/config"
	"github.com/rcliao/shell/internal/memory"
	"github.com/rcliao/shell/internal/planner"
	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/reload"
	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/skill"
	"github.com/rcliao/shell/internal/store"
	"github.com/rcliao/shell/internal/telegram"
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
	tunnelMgr *tunnel.Manager      // nil if disabled
	pmMgr     *pm.Manager          // nil if disabled
}

func New(cfg config.Config) (*Daemon, error) {
	// Open encrypted secret store (if enabled) before anything reads tokens.
	config.OpenSecretStore(cfg.Secrets)

	// Export all secrets into env so child processes (Claude → Bash → skill scripts) inherit them.
	if n := config.ExportSecrets(); n > 0 {
		slog.Info("secrets: exported to env", "count", n)
	}

	// Open store
	st, err := store.Open(cfg.Store.DBPath)
	if err != nil {
		return nil, err
	}

	// Ensure playground directory exists if configured.
	if cfg.Claude.PlaygroundDir != "" {
		if err := os.MkdirAll(cfg.Claude.PlaygroundDir, 0755); err != nil {
			slog.Warn("playground: failed to create dir", "path", cfg.Claude.PlaygroundDir, "error", err)
		} else {
			slog.Info("playground initialized", "path", cfg.Claude.PlaygroundDir)
		}
	}

	// Load skills if enabled.
	var skillRegistry *skill.Registry
	if cfg.Skills.Enabled {
		var allSkills []*skill.Skill

		// Global skills: ~/.shell/skills/
		globalDir := cfg.Skills.Dir
		if globalDir == "" {
			globalDir = filepath.Join(config.DefaultConfigDir(), "skills")
		}
		if s, err := skill.LoadDir(globalDir); err == nil {
			allSkills = append(allSkills, s...)
		}

		// Project skills: <workdir>/.agent/skills/
		if cfg.Claude.WorkDir != "" {
			agentDir := filepath.Join(cfg.Claude.WorkDir, ".agent", "skills")
			if s, err := skill.LoadDir(agentDir); err == nil {
				allSkills = append(allSkills, s...)
			}
		}

		if len(allSkills) > 0 {
			skillRegistry = skill.NewRegistry(allSkills)
			slog.Info("skills loaded", "count", len(allSkills))
		}
	}

	// Merge skill allowed-tools with config allowed-tools.
	allowedTools := cfg.Claude.AllowedTools
	if skillRegistry != nil {
		allowedTools = append(allowedTools, skillRegistry.AllowedTools()...)
	}

	// Create process manager
	proc := process.NewManager(process.ManagerConfig{
		Binary:         cfg.Claude.Binary,
		Model:          cfg.Claude.Model,
		Timeout:        cfg.Claude.Timeout,
		MaxSessions:    cfg.Claude.MaxSessions,
		WorkDir:        cfg.Claude.WorkDir,
		AllowedTools:   allowedTools,
		ExtraArgs:      cfg.Claude.ExtraArgs,
		SettingSources: cfg.Claude.SettingSources,
	})

	// Legacy: inject shell:capabilities into system namespaces for profiles without AgentNS.
	// Profiles with AgentNS use pinned memories instead (capabilities are seeded as pinned).
	if cfg.Scheduler.Enabled {
		capNS := "shell:capabilities"
		if !containsStr(cfg.Memory.SystemNamespaces, capNS) {
			cfg.Memory.SystemNamespaces = append(cfg.Memory.SystemNamespaces, capNS)
		}
		for name, p := range cfg.Memory.Profiles {
			if p.AgentNS == "" && !containsStr(p.SystemNamespaces, capNS) {
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
				AgentNS:          p.AgentNS,
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

	// Initialize tunnel manager if enabled.
	var tunnelMgr *tunnel.Manager
	if cfg.Tunnel.Enabled {
		tunnelMgr = tunnel.NewManager(tunnel.Config{
			Enabled:         true,
			CloudflaredBin:  cfg.Tunnel.CloudflaredBin,
			MaxTunnels:      cfg.Tunnel.MaxTunnels,
			DefaultProtocol: cfg.Tunnel.DefaultProtocol,
		})
		slog.Info("tunnel manager initialized")
	}

	// Initialize process manager if enabled.
	var pmMgr *pm.Manager
	if cfg.PM.Enabled {
		pmMgr = pm.NewManager(pm.Config{
			Enabled:  true,
			MaxProcs: cfg.PM.MaxProcs,
			LogLines: cfg.PM.LogLines,
		})
		slog.Info("process manager initialized")
	}

	br := bridge.New(proc, st, mem, pl, cfg.Planner.Worktree, cfg.Claude.WorkDir, cfg.Telegram.ReactionMap, tunnelMgr, pmMgr, skillRegistry)

	// Create auth with policy engine
	configDir := config.DefaultConfigDir()
	allowlistStore := telegram.NewAllowlistStore(filepath.Join(configDir, "allowlist.json"))
	pairingMgr := telegram.NewPairingManager(allowlistStore, filepath.Join(configDir, "pairing.json"), 10*time.Minute)
	limiter := telegram.NewRateLimiter(5, 60*time.Second)

	auth := telegram.NewAuth(telegram.AuthOptions{
		DMPolicy:          telegram.DMPolicy(cfg.Telegram.DMPolicy),
		GroupPolicy:       telegram.GroupPolicy(cfg.Telegram.GroupPolicy),
		ConfigUsers:       cfg.Telegram.AllowedUsers,
		GroupAllowedUsers: cfg.Telegram.GroupAllowedUsers,
		AllowlistStore:    allowlistStore,
		Pairing:           pairingMgr,
		Limiter:           limiter,
	})

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
			for _, photo := range resp.Photos {
				bot.SendPhoto(chatID, photo.Data, photo.Caption)
			}
			bot.SendText(chatID, resp.Text)
		}
		sched = scheduler.New(adapter, onNotify, onPrompt, cfg.Scheduler.Timezone)
		sched.SetQuietHours(cfg.Scheduler.QuietHourStart, cfg.Scheduler.QuietHourEnd)
		sched.SetHeartbeatPrompt(func(chatID int64, msg string) (string, error) {
			resp, err := br.HandleMessageStreaming(context.Background(), chatID, msg, "heartbeat", nil, nil, nil)
			if err != nil {
				return "", err
			}
			for _, photo := range resp.Photos {
				bot.SendPhoto(chatID, photo.Data, photo.Caption)
			}
			return resp.Text, nil
		})
		br.SetSchedulerConfig(true, cfg.Scheduler.Timezone)
		slog.Info("scheduler initialized", "timezone", cfg.Scheduler.Timezone, "quiet_hours", fmt.Sprintf("%d:00-%d:00", cfg.Scheduler.QuietHourStart, cfg.Scheduler.QuietHourEnd))

		// Seed schedule docs into memory so they're always visible in system prompt.
		if mem != nil {
			if err := mem.SeedCapability(context.Background(), resolveAgentNS(cfg), "scheduling",
				"Schedule capabilities: Use [schedule at=\"...\"] for one-shot or [schedule cron=\"...\"] for recurring reminders. "+
					"Commands: /schedule add|list|delete|enable|pause. Supports @daily, @hourly, @weekly, @monthly aliases. "+
					"Use --tz flag for per-schedule timezone override."); err != nil {
				slog.Warn("failed to seed schedule docs", "error", err)
			}
			if err := mem.SeedCapability(context.Background(), resolveAgentNS(cfg), "heartbeat-learning",
				"Heartbeat self-improvement: During [Heartbeat] check-ins, recent conversations and previous "+
					"insights are provided. Use [heartbeat-learning]...[/heartbeat-learning] to store reusable "+
					"patterns discovered during heartbeats."); err != nil {
				slog.Warn("failed to seed heartbeat-learning docs", "error", err)
			}
		}
	}

	// Always set timezone on bridge so the agent knows current time,
	// even when the scheduler is disabled.
	if !cfg.Scheduler.Enabled && cfg.Scheduler.Timezone != "" {
		br.SetTimezone(cfg.Scheduler.Timezone)
	}

	// Seed playground docs into memory if configured.
	if cfg.Claude.PlaygroundDir != "" && mem != nil {
		if err := mem.SeedCapability(context.Background(), resolveAgentNS(cfg), "playground",
			fmt.Sprintf("Playground directory: %s — You have full write access to this directory. "+
				"Create project subdirectories here (e.g. %s/my-app/) for web apps, experiments, and prototypes. "+
				"Files here can be written and edited without permission prompts. "+
				"IMPORTANT: When running web servers or long-running processes, ALWAYS run them in the background "+
				"(e.g. 'nohup python -m http.server 8080 &' or 'node server.js &') so you can continue with "+
				"other tasks like setting up tunnels. Never run a server in the foreground or your session will block.",
				cfg.Claude.PlaygroundDir, cfg.Claude.PlaygroundDir)); err != nil {
			slog.Warn("failed to seed playground docs", "error", err)
		}
	}

	// Seed tunnel docs into memory if tunnels are enabled.
	if tunnelMgr != nil && mem != nil {
		if err := mem.SeedCapability(context.Background(), resolveAgentNS(cfg), "tunnel",
			"HTTP tunnel capabilities: Use [tunnel port=\"...\"] to expose a local port via Cloudflare quick tunnel. "+
				"Use [tunnel action=\"stop\" port=\"...\"] to stop a tunnel, or [tunnel action=\"list\"] to list active tunnels. "+
				"Optional protocol attribute: protocol=\"https\". Tunnels provide public URLs for local web apps. "+
				"WORKFLOW: 1) Write app files, 2) Start server in background (e.g. 'node server.js &'), "+
				"3) Use [tunnel port=\"...\"] to expose it. The tunnel directive is handled by the bridge, not by Bash."); err != nil {
			slog.Warn("failed to seed tunnel docs", "error", err)
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
		tunnelMgr: tunnelMgr,
		pmMgr:     pmMgr,
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

	// Wire self-restart: when a plan modifies shell source, rebuild and restart.
	if sourceDir != "" {
		binaryPath, _ := os.Executable()
		br.SetSelfRestart(sourceDir, func() {
			slog.Info("self-restart: rebuilding shell...")
			if err := reload.RebuildAndRestart(binaryPath, "./cmd/shell", d.Shutdown); err != nil {
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

	// Start scheduler if enabled — register as a pm-managed process.
	if d.scheduler != nil {
		if d.pmMgr != nil {
			sched := d.scheduler
			d.pmMgr.StartFunc(ctx, "scheduler", func(fctx context.Context) error {
				sched.Run(fctx)
				return nil
			}, "cron/heartbeat scheduler", pm.WithTags(map[string]string{"type": "system"}), pm.WithRestart(pm.RestartAlways))
		} else {
			go d.scheduler.Run(ctx)
		}
	}

	// Start stale session cleanup ticker — register as pm-managed if available.
	if d.pmMgr != nil {
		d.pmMgr.StartFunc(ctx, "cleanup", func(fctx context.Context) error {
			d.cleanupLoop(fctx)
			return nil
		}, "stale session cleanup", pm.WithTags(map[string]string{"type": "system"}), pm.WithRestart(pm.RestartAlways))
	} else {
		go d.cleanupLoop(ctx)
	}

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
	if d.pmMgr != nil {
		d.pmMgr.StopAll()
	}
	if d.tunnelMgr != nil {
		d.tunnelMgr.StopAll()
	}
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

// resolveAgentNS returns the first AgentNS found in config profiles, or "" for legacy mode.
func resolveAgentNS(cfg config.Config) string {
	for _, p := range cfg.Memory.Profiles {
		if p.AgentNS != "" {
			return p.AgentNS
		}
	}
	return ""
}
