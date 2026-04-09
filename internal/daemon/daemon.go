package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
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
	"github.com/rcliao/shell/internal/rpc"
	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/skill"
	"github.com/rcliao/shell/internal/store"
	"github.com/rcliao/shell/internal/telegram"
	"github.com/rcliao/shell/internal/tool"
	"github.com/rcliao/shell/internal/transcript"
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
	rpcServer *rpc.Server          // nil if no RPC endpoints
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

	// Compute per-agent directory early (used for skills, socket, MCP, etc.).
	pidDir := filepath.Dir(cfg.Daemon.PIDFile)
	if pidDir == "" {
		pidDir = config.DefaultConfigDir()
	}
	agentSkillsDir := filepath.Join(pidDir, "skills")

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

		// Per-agent skills: ~/.shell/agents/<name>/skills/
		// Derived from PID file directory (e.g. ~/.shell/agents/umbreonmini/skills/)
		agentSkillsDir := filepath.Join(pidDir, "skills")
		if s, err := skill.LoadDir(agentSkillsDir); err == nil {
			allSkills = append(allSkills, s...)
			if len(s) > 0 {
				slog.Info("agent skills loaded", "dir", agentSkillsDir, "count", len(s))
			}
		}

		if len(allSkills) > 0 {
			skillRegistry = skill.NewRegistry(allSkills)
			slog.Info("skills loaded", "count", len(allSkills))
		}
	}

	// Build unified tool registry from all sources.
	toolReg := tool.NewRegistry()

	// Register MCP tools (first-class, bridge-internal).
	toolReg.Register(tool.Tool{Name: "shell_pm", Description: "Process manager", Kind: tool.KindMCP, AllowedTools: []string{"mcp__shell-bridge__shell_pm"}})
	toolReg.Register(tool.Tool{Name: "shell_tunnel", Description: "HTTP tunnels", Kind: tool.KindMCP, AllowedTools: []string{"mcp__shell-bridge__shell_tunnel"}})
	toolReg.Register(tool.Tool{Name: "shell_relay", Description: "Message relay", Kind: tool.KindMCP, AllowedTools: []string{"mcp__shell-bridge__shell_relay"}})

	// Register skill scripts.
	if skillRegistry != nil {
		for _, s := range skillRegistry.All() {
			toolReg.Register(tool.Tool{Name: s.Name, Description: s.Description, Kind: tool.KindSkill, AllowedTools: s.AllowedTools})
		}
	}

	// Merge all allowed-tools.
	allowedTools := cfg.Claude.AllowedTools
	allowedTools = append(allowedTools, toolReg.AllowedTools()...)

	// Derive per-agent socket and MCP config paths from PID file directory.
	bridgeSockPath := filepath.Join(pidDir, "bridge.sock")
	shellBinary, _ := os.Executable()
	mcpConfigPath := filepath.Join(pidDir, "mcp.json")
	mcpServers := map[string]any{
		"shell-bridge": map[string]any{
			"type":    "stdio",
			"command": shellBinary,
			"args":    []string{"mcp"},
		},
	}
	// Add ghost MCP server with per-agent env vars so each agent
	// uses its own ghost namespace and database.
	agentNS := resolveAgentNS(cfg)
	if agentNS != "" {
		ghostEnv := map[string]string{
			"GHOST_NS": agentNS,
		}
		if cfg.Memory.DBPath != "" {
			ghostEnv["GHOST_DB"] = cfg.Memory.DBPath
		}
		ghostBin, err := exec.LookPath("ghost")
		if err != nil {
			ghostBin = "ghost" // fallback; will fail at runtime if not found
		}
		mcpServers["ghost"] = map[string]any{
			"type":    "stdio",
			"command": ghostBin,
			"args":    []string{"mcp-serve"},
			"env":     ghostEnv,
		}
	}
	mcpConfig := map[string]any{
		"mcpServers": mcpServers,
	}
	if mcpData, err := json.MarshalIndent(mcpConfig, "", "  "); err == nil {
		os.WriteFile(mcpConfigPath, mcpData, 0644)
	}

	// Generate per-agent Claude settings with agent-scoped hooks.
	// This ensures hooks use the correct ghost namespace and database
	// for each agent, preventing cross-contamination between agents.
	agentSettingsPath := filepath.Join(pidDir, "settings.json")
	if agentNS != "" {
		if settings := generateAgentSettings(agentNS, cfg.Memory.DBPath); settings != nil {
			if data, err := json.MarshalIndent(settings, "", "  "); err == nil {
				os.WriteFile(agentSettingsPath, data, 0644)
				slog.Info("agent settings generated", "path", agentSettingsPath, "ns", agentNS)
			}
		}
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
		Env:            cfg.Claude.Env,
		SettingSources: cfg.Claude.SettingSources,
		BridgeSockPath: bridgeSockPath,
		MCPConfigPath:  mcpConfigPath,
		SettingsPath:   agentSettingsPath,
		AgentNS:        agentNS,
		GhostDB:        cfg.Memory.DBPath,
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
			ExecuteModel:         cfg.Claude.ResolveModel("planner_execute"),
			ReviewModel:          cfg.Claude.ResolveModel("planner_review"),
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
	br.SetClaudeConfig(cfg.Claude)

	// Track skill directories for hot reload.
	var skillDirs []string
	globalSkillDir := cfg.Skills.Dir
	if globalSkillDir == "" {
		globalSkillDir = filepath.Join(config.DefaultConfigDir(), "skills")
	}
	skillDirs = append(skillDirs, globalSkillDir)
	if cfg.Claude.WorkDir != "" {
		skillDirs = append(skillDirs, filepath.Join(cfg.Claude.WorkDir, ".agent", "skills"))
	}
	skillDirs = append(skillDirs, agentSkillsDir)
	br.SetSkillDirs(skillDirs)

	// Configure session auto-rotation by token count.
	if cfg.Claude.MaxSessionTokens > 0 {
		br.SetMaxSessionTokens(cfg.Claude.MaxSessionTokens)
		slog.Info("session rotation enabled", "max_tokens", cfg.Claude.MaxSessionTokens)
	}

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

	// Set agent identity prompt on bridge.
	if cfg.Agent.SystemPrompt != "" {
		br.SetAgentIdentity(cfg.Agent.SystemPrompt)
	}

	// Discover peer agents (needed for both transcript and name-based routing).
	var peers []config.PeerAgent
	if len(cfg.Agent.PeerBots) > 0 {
		peers = discoverPeerAgents(cfg.Agent.PeerBots)
	}

	// Open shared transcript for multi-agent group awareness.
	if cfg.Agent.GroupMode == "autonomous" && len(cfg.Agent.PeerBots) > 0 {
		transcriptPath := cfg.Agent.TranscriptPath
		if transcriptPath == "" {
			transcriptPath = filepath.Join(config.DefaultConfigDir(), "shared", "transcript.db")
		}
		if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
			slog.Warn("failed to create transcript directory", "error", err)
		} else if ts, err := transcript.Open(transcriptPath); err != nil {
			slog.Warn("failed to open transcript store", "error", err)
		} else {
			budget := cfg.Agent.TranscriptBudget
			if budget <= 0 {
				budget = 2000
			}
			br.SetTranscript(ts, cfg.Agent.BotUsername, budget)
			slog.Info("shared transcript enabled", "path", transcriptPath, "budget", budget)
		}

		if len(peers) > 0 {
			br.SetPeerAgents(peers)
			for _, p := range peers {
				slog.Info("peer agent discovered", "name", p.Name, "bot", p.BotUsername, "skills", p.Skills)
			}
		}
	}

	// Create telegram bot with agent identity for group chat routing.
	token := cfg.TelegramToken()

	// Collect peer aliases from discovered peer agents for name-based routing.
	var peerAliases []string
	for _, p := range peers {
		peerAliases = append(peerAliases, strings.ToLower(p.Name))
		for _, a := range p.Aliases {
			peerAliases = append(peerAliases, strings.ToLower(a))
		}
	}

	agentCfg := telegram.AgentConfig{
		BotUsername:           cfg.Agent.BotUsername,
		Aliases:              cfg.Agent.Aliases,
		BroadcastProbability: cfg.Agent.BroadcastProbability,
		PeerBots:             cfg.Agent.PeerBots,
		PeerAliases:          peerAliases,
		GroupMode:            cfg.Agent.GroupMode,
	}
	bot, err := telegram.NewBot(token, auth, br, agentCfg)
	if err != nil {
		st.Close()
		if mem != nil {
			mem.Close()
		}
		return nil, err
	}

	// Wire transport: plan progress, relay photos → Telegram
	br.SetTransport(&telegramTransport{bot: bot})

	// Register cron parser for bridge schedule commands.
	bridge.SetCronParser(func(expr string) (interface{ Next(time.Time) time.Time }, error) {
		return scheduler.ParseCron(expr)
	})

	// Create RPC server for skill scripts.
	rpcSrv := rpc.New(rpc.Config{
		SocketPath: bridgeSockPath,
		PMMgr:      pmMgr,
		TunnelMgr:  tunnelMgr,
		Store:      st,
		Memory:     mem,
		Notify: func(chatID int64, msg string) {
			bot.SendText(chatID, msg)
		},
		SendPhoto: func(chatID int64, data []byte, caption string) {
			bot.SendPhoto(chatID, data, caption)
		},
		RelayToBridge: func(ctx context.Context, chatID int64, message string) {
			// Log the relay message to the target chat's session so Claude
			// has context when the recipient replies. Don't run a full Claude
			// turn — that would block the session and cause "busy" rejections.
			sess, err := st.GetSession(chatID)
			if err == nil && sess != nil {
				st.LogMessage(sess.ID, "assistant", "[Relay message]\n"+message)
			}
			bot.SendText(chatID, message)
		},
		CronParse: func(expr string) (interface{ Next(time.Time) time.Time }, error) {
			return scheduler.ParseCron(expr)
		},
		SkillsReload: func() (int, error) {
			return br.ReloadSkills()
		},
		SkillsLoad: func(name string) (string, error) {
			reg := br.GetSkillRegistry()
			if reg == nil {
				return "", fmt.Errorf("no skills loaded")
			}
			return reg.FullPrompt(name)
		},
		Timezone: cfg.Scheduler.Timezone,
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
		// Configure adaptive heartbeat: back off to idle interval after noop.
		if cfg.Scheduler.HeartbeatIdleInterval != "" {
			if d, err := time.ParseDuration(cfg.Scheduler.HeartbeatIdleInterval); err == nil && d > 0 {
				sched.SetIdleInterval(d)
			}
		}
		br.SetSchedulerConfig(true, cfg.Scheduler.Timezone)
		if cfg.Scheduler.HeartbeatInterval != "" {
			br.SetHeartbeatInterval(cfg.Scheduler.HeartbeatInterval)
		}
		slog.Info("scheduler initialized",
			"timezone", cfg.Scheduler.Timezone,
			"quiet_hours", fmt.Sprintf("%d:00-%d:00", cfg.Scheduler.QuietHourStart, cfg.Scheduler.QuietHourEnd),
			"heartbeat_interval", cfg.Scheduler.HeartbeatInterval,
			"heartbeat_idle_interval", cfg.Scheduler.HeartbeatIdleInterval,
		)

		// Seed schedule docs into memory so they're always visible in system prompt.
		if mem != nil {
			if err := mem.SeedCapability(context.Background(), resolveAgentNS(cfg), "scheduling",
				"Schedule capabilities: Use scripts/shell-schedule for one-shot or recurring reminders. "+
					"Commands: /schedule add|list|delete|enable|pause. Supports @daily, @hourly, @weekly, @monthly aliases. "+
					"Use --tz flag for per-schedule timezone override."); err != nil {
				slog.Warn("failed to seed schedule docs", "error", err)
			}
			if err := mem.SeedCapability(context.Background(), resolveAgentNS(cfg), "heartbeat-learning",
				"Heartbeat self-improvement: During [Heartbeat] check-ins, recent conversations and previous "+
					"insights are provided. Use scripts/shell-remember --action heartbeat-learning to store reusable "+
					"patterns discovered during heartbeats."); err != nil {
				slog.Warn("failed to seed heartbeat-learning docs", "error", err)
			}
		}
	}

	// Seed skill-authoring docs if skills are enabled.
	if cfg.Skills.Enabled && mem != nil {
		if err := mem.SeedCapability(context.Background(), resolveAgentNS(cfg), "skill-authoring",
			fmt.Sprintf("Skill authoring: You can create new skills to expand your capabilities. "+
				"To create a skill, write a SKILL.md file and optional scripts/ directory to your agent skills directory: %s/. "+
				"SKILL.md format: frontmatter (--- delimited) with name, description, allowed-tools fields, "+
				"followed by markdown instructions. Scripts must be executable (chmod +x). "+
				"After creating a skill, run `scripts/shell-reload` to hot-load it immediately. "+
				"The skill will then appear in your system prompt on the next message. "+
				"Use this when you notice a recurring need that could be automated — "+
				"e.g., API integrations, data processing, custom workflows.",
				agentSkillsDir)); err != nil {
			slog.Warn("failed to seed skill-authoring docs", "error", err)
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
			"HTTP tunnel capabilities: Use scripts/shell-tunnel to expose local ports via Cloudflare quick tunnel. "+
				"Tunnels provide public URLs for local web apps. "+
				"WORKFLOW: 1) Write app files, 2) Start server via scripts/shell-pm, "+
				"3) Use scripts/shell-tunnel to expose it."); err != nil {
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
		rpcServer: rpcSrv,
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

// discoverPeerAgents reads peer agent config files to discover their skills.
func discoverPeerAgents(peerBots []string) []config.PeerAgent {
	agentsDir := filepath.Join(config.DefaultConfigDir(), "agents")
	var peers []config.PeerAgent

	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil
	}

	// Build a set of peer bot usernames (lowercased) for matching.
	peerSet := make(map[string]bool, len(peerBots))
	for _, p := range peerBots {
		peerSet[strings.ToLower(p)] = true
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		cfgPath := filepath.Join(agentsDir, entry.Name(), "config.json")
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			continue
		}
		var peerCfg config.Config
		if err := json.Unmarshal(data, &peerCfg); err != nil {
			continue
		}
		if !peerSet[strings.ToLower(peerCfg.Agent.BotUsername)] {
			continue
		}
		peers = append(peers, config.PeerAgent{
			Name:        peerCfg.Agent.Name,
			Aliases:     peerCfg.Agent.Aliases,
			BotUsername: peerCfg.Agent.BotUsername,
			Skills:      peerCfg.Agent.Skills,
		})
	}
	return peers
}

// Run starts the daemon and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	// Write PID file
	if err := writePID(d.cfg.Daemon.PIDFile); err != nil {
		slog.Warn("failed to write PID file", "path", d.cfg.Daemon.PIDFile, "error", err)
	}

	// Start RPC server for skill scripts.
	if d.rpcServer != nil {
		go func() {
			if err := d.rpcServer.Start(); err != nil {
				slog.Error("rpc server stopped", "error", err)
			}
		}()
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
	if d.rpcServer != nil {
		d.rpcServer.Stop()
	}
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
			ProviderSessionID: sess.ProviderSessionID,
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
	sessionTicker := time.NewTicker(10 * time.Minute)
	defer sessionTicker.Stop()
	dbTicker := time.NewTicker(6 * time.Hour)
	defer dbTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sessionTicker.C:
			if err := d.bridge.CleanupStaleSessions(1 * time.Hour); err != nil {
				slog.Warn("stale session cleanup failed", "error", err)
			}
		case <-dbTicker.C:
			if n, err := d.store.CleanupOldMessages(7 * 24 * time.Hour); err != nil {
				slog.Warn("message cleanup failed", "error", err)
			} else if n > 0 {
				slog.Info("cleaned up old messages", "deleted", n)
			}
			if n, err := d.store.CleanupCompletedTasks(3 * 24 * time.Hour); err != nil {
				slog.Warn("task cleanup failed", "error", err)
			} else if n > 0 {
				slog.Info("cleaned up completed tasks", "deleted", n)
			}
			if n, err := d.store.CleanupDisabledSchedules(); err != nil {
				slog.Warn("schedule cleanup failed", "error", err)
			} else if n > 0 {
				slog.Info("cleaned up disabled one-shot schedules", "deleted", n)
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

// telegramTransport implements bridge.Transport for Telegram.
type telegramTransport struct {
	bot *telegram.Bot
}

func (t *telegramTransport) Notify(chatID int64, msg string) {
	t.bot.SendText(chatID, msg)
}

func (t *telegramTransport) SendPhoto(chatID int64, data []byte, caption string) {
	t.bot.SendPhoto(chatID, data, caption)
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

// generateAgentSettings creates a Claude settings map with agent-scoped hooks.
// It reads the global ~/.claude/settings.json, extracts hooks, and wraps each
// hook command with GHOST_NS and GHOST_DB env vars for the specific agent.
// Returns nil if the global settings cannot be read or have no hooks.
func generateAgentSettings(agentNS, dbPath string) map[string]any {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	globalSettingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	data, err := os.ReadFile(globalSettingsPath)
	if err != nil {
		return nil
	}

	var globalSettings map[string]any
	if err := json.Unmarshal(data, &globalSettings); err != nil {
		return nil
	}

	hooksRaw, ok := globalSettings["hooks"]
	if !ok {
		return nil
	}
	hooks, ok := hooksRaw.(map[string]any)
	if !ok {
		return nil
	}

	// Build env prefix for hook commands.
	envPrefix := fmt.Sprintf("GHOST_NS=%s", agentNS)
	if dbPath != "" {
		envPrefix += fmt.Sprintf(" GHOST_DB=%s", dbPath)
	}

	// Walk the hooks structure and wrap each command with the env prefix.
	agentHooks := make(map[string]any, len(hooks))
	for eventName, eventRaw := range hooks {
		eventList, ok := eventRaw.([]any)
		if !ok {
			continue
		}
		var agentEventList []any
		for _, groupRaw := range eventList {
			group, ok := groupRaw.(map[string]any)
			if !ok {
				continue
			}
			hookListRaw, ok := group["hooks"]
			if !ok {
				continue
			}
			hookList, ok := hookListRaw.([]any)
			if !ok {
				continue
			}
			var agentHookList []any
			for _, hookRaw := range hookList {
				hook, ok := hookRaw.(map[string]any)
				if !ok {
					continue
				}
				cmd, ok := hook["command"].(string)
				if !ok {
					continue
				}
				// Only wrap ghost-related hooks.
				if !strings.Contains(cmd, "ghost") {
					agentHookList = append(agentHookList, hook)
					continue
				}
				agentHook := make(map[string]any, len(hook))
				for k, v := range hook {
					agentHook[k] = v
				}
				agentHook["command"] = envPrefix + " " + cmd
				agentHookList = append(agentHookList, agentHook)
			}
			agentEventList = append(agentEventList, map[string]any{
				"hooks": agentHookList,
			})
		}
		agentHooks[eventName] = agentEventList
	}

	return map[string]any{
		"hooks": agentHooks,
	}
}
