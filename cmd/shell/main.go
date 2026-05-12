package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rcliao/shell/internal/config"
	"github.com/rcliao/shell/internal/daemon"
	shellmcp "github.com/rcliao/shell/internal/mcp"
	"github.com/rcliao/shell/internal/memory"
	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/rpc"
	"github.com/rcliao/shell/internal/search"
	"github.com/rcliao/shell/internal/skill"
	"github.com/rcliao/shell/internal/store"
	"github.com/rcliao/shell/internal/telegram"
	"github.com/spf13/cobra"
)

func main() {
	var verbose bool

	rootCmd := &cobra.Command{
		Use:   "shell",
		Short: "Telegram Bot to Claude Code CLI bridge",
	}
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Verbose output")

	// init command
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize config directory and default config",
		RunE: func(cmd *cobra.Command, args []string) error {
			configDir := config.DefaultConfigDir()
			os.MkdirAll(configDir, 0755)
			fmt.Printf("Created %s\n", configDir)

			configPath := filepath.Join(configDir, "config.json")
			if _, err := os.Stat(configPath); os.IsNotExist(err) {
				cfg := config.Default()
				data, _ := json.MarshalIndent(cfg, "", "  ")
				os.WriteFile(configPath, data, 0644)
				fmt.Printf("Created %s\n", configPath)
			} else {
				fmt.Printf("Config already exists: %s\n", configPath)
			}

			fmt.Println("\nDone! Next steps:")
			fmt.Println("1. Create a Telegram bot via @BotFather")
			fmt.Println("2. Set TELEGRAM_BOT_TOKEN environment variable")
			fmt.Println("3. Add your Telegram user ID to allowed_users in config.json")
			fmt.Println("4. Run: shell daemon")
			return nil
		},
	}

	// daemon command
	var watch bool
	var configFlag string
	daemonCmd := &cobra.Command{
		Use:   "daemon",
		Short: "Start the Telegram bot daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			if verbose {
				slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
			}

			cfg := loadConfigFrom(configFlag)

			if watch {
				cfg.Reload.Enabled = true
			}

			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			d, err := daemon.New(cfg)
			if err != nil {
				return fmt.Errorf("init daemon: %w", err)
			}

			// SIGHUP → graceful restart (re-exec with same args)
			sighup := make(chan os.Signal, 1)
			signal.Notify(sighup, syscall.SIGHUP)
			go func() {
				<-sighup
				slog.Info("received SIGHUP, restarting")
				d.Shutdown()
				binary, err := os.Executable()
				if err != nil {
					slog.Error("restart: cannot resolve executable", "error", err)
					os.Exit(1)
				}
				if err := syscall.Exec(binary, os.Args, os.Environ()); err != nil {
					slog.Error("restart: exec failed", "error", err)
					os.Exit(1)
				}
			}()

			// Run blocks until ctx is cancelled (SIGINT/SIGTERM).
			// Shutdown synchronously after Run returns to avoid
			// racing context cancellation against pm restart loops.
			err = d.Run(ctx)
			d.Shutdown()
			return err
		},
	}
	daemonCmd.Flags().BoolVarP(&watch, "watch", "w", false, "Enable live reload on source changes")
	daemonCmd.Flags().StringVar(&configFlag, "config", "", "Path to config file (default: ~/.shell/config.json)")

	// send command — one-shot test without Telegram
	sendCmd := &cobra.Command{
		Use:   "send [message]",
		Short: "Send a one-shot message to Claude (no Telegram)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if verbose {
				slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
			}

			cfg := loadConfig()
			message := args[0]

			binary := cfg.Claude.Binary
			if binary == "" {
				binary = "claude"
			}

			cliArgs := []string{"-p", message, "--output-format", "text"}
			if cfg.Claude.Model != "" {
				cliArgs = append(cliArgs, "--model", cfg.Claude.Model)
			}

			c := exec.Command(binary, cliArgs...)
			var stdout, stderr bytes.Buffer
			c.Stdout = &stdout
			c.Stderr = &stderr

			if err := c.Run(); err != nil {
				if stderr.Len() > 0 {
					return fmt.Errorf("claude: %w\n%s", err, stderr.String())
				}
				return fmt.Errorf("claude: %w", err)
			}

			fmt.Print(stdout.String())
			return nil
		},
	}

	// status command
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show active sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfig()
			st, err := store.Open(cfg.Store.DBPath)
			if err != nil {
				return err
			}
			defer st.Close()

			sessions, err := st.ListActiveSessions()
			if err != nil {
				return err
			}

			if len(sessions) == 0 {
				fmt.Println("No active sessions.")
				return nil
			}

			fmt.Printf("Active sessions: %d\n\n", len(sessions))
			for _, s := range sessions {
				topic := ""
				if s.MessageThreadID != 0 {
					topic = fmt.Sprintf("  Topic: %d\n", s.MessageThreadID)
				}
				fmt.Printf("  Chat ID: %d\n%s  Session: %s\n  Status: %s\n  Created: %s\n  Updated: %s\n\n",
					s.ChatID, topic, s.ProviderSessionID[:12]+"...", s.Status,
					s.CreatedAt.Format("2006-01-02 15:04:05"),
					s.UpdatedAt.Format("2006-01-02 15:04:05"),
				)
			}
			return nil
		},
	}

	// session command group — persistent --config flag lets all subcommands
	// target a specific agent's DB (e.g. ~/.shell/agents/pikamini/config.json).
	// Without it, session commands load ~/.shell/config.json which in a
	// multi-agent setup points at an unrelated DB.
	var sessionConfigFlag string
	sessionCmd := &cobra.Command{
		Use:   "session",
		Short: "Manage sessions",
	}
	sessionCmd.PersistentFlags().StringVar(&sessionConfigFlag, "config", "",
		"Path to agent config (default: ~/.shell/config.json)")
	loadSessionConfig := func() config.Config { return loadConfigFrom(sessionConfigFlag) }

	sessionListCmd := &cobra.Command{
		Use:   "list",
		Short: "List all sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadSessionConfig()
			st, err := store.Open(cfg.Store.DBPath)
			if err != nil {
				return err
			}
			defer st.Close()

			sessions, err := st.ListActiveSessions()
			if err != nil {
				return err
			}

			if len(sessions) == 0 {
				fmt.Println("No sessions.")
				return nil
			}

			for _, s := range sessions {
				fmt.Printf("%d\t%d\t%s\t%s\t%s\n",
					s.ChatID, s.MessageThreadID, s.ProviderSessionID[:12], s.Status,
					s.UpdatedAt.Format("2006-01-02 15:04"),
				)
			}
			return nil
		},
	}

	sessionKillCmd := &cobra.Command{
		Use:   "kill <chat-id>",
		Short: "Kill a session by chat ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadSessionConfig()
			st, err := store.Open(cfg.Store.DBPath)
			if err != nil {
				return err
			}
			defer st.Close()

			var chatID int64
			if _, err := fmt.Sscanf(args[0], "%d", &chatID); err != nil {
				return fmt.Errorf("invalid chat ID: %s", args[0])
			}

			// Create a process manager just to track
			proc := process.NewManager(process.ManagerConfig{
				Binary:      cfg.Claude.Binary,
				MaxSessions: cfg.Claude.MaxSessions,
			})
			// Kill the main-thread session. The CLI kill command doesn't
			// accept thread_id today; pass -1 to DeleteSession to wipe all
			// topic sessions for the chat at once.
			proc.Kill(process.SessionKey{ChatID: chatID})

			if err := st.DeleteSession(chatID, -1); err != nil {
				return err
			}

			fmt.Printf("Killed session for chat %d\n", chatID)
			return nil
		},
	}

	// restart command
	restartCmd := &cobra.Command{
		Use:   "restart",
		Short: "Send SIGHUP to running daemon (graceful restart)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfig()
			pid, err := daemon.ReadPID(cfg.Daemon.PIDFile)
			if err != nil {
				return fmt.Errorf("cannot read PID file %s: %w", cfg.Daemon.PIDFile, err)
			}
			// Check if process is alive
			if err := syscall.Kill(pid, 0); err != nil {
				return fmt.Errorf("daemon (pid %d) is not running: %w", pid, err)
			}
			if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
				return fmt.Errorf("failed to send SIGHUP to pid %d: %w", pid, err)
			}
			fmt.Printf("Sent SIGHUP to daemon (pid %d) — restarting\n", pid)
			return nil
		},
	}

	// stop command
	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Send SIGTERM to running daemon (graceful shutdown)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfig()
			pid, err := daemon.ReadPID(cfg.Daemon.PIDFile)
			if err != nil {
				return fmt.Errorf("cannot read PID file %s: %w", cfg.Daemon.PIDFile, err)
			}
			if err := syscall.Kill(pid, 0); err != nil {
				return fmt.Errorf("daemon (pid %d) is not running: %w", pid, err)
			}
			if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
				return fmt.Errorf("failed to send SIGTERM to pid %d: %w", pid, err)
			}
			// Poll for process exit (up to 5 seconds)
			for i := 0; i < 50; i++ {
				time.Sleep(100 * time.Millisecond)
				if err := syscall.Kill(pid, 0); err != nil {
					fmt.Printf("Daemon (pid %d) stopped\n", pid)
					return nil
				}
			}
			fmt.Printf("Sent SIGTERM to daemon (pid %d) — still shutting down\n", pid)
			return nil
		},
	}

	// search command — web search for use by Claude via Bash
	var (
		searchCount     int
		searchFreshness string
		searchCountry   string
		searchJSON      bool
	)
	searchCmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Web search (Brave/Tavily/DuckDuckGo)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.Join(args, " ")

			cfg := loadConfig()
			config.OpenSecretStore(cfg.Secrets)
			defer config.CloseSecretStore()

			braveKey := cfg.Secret("BRAVE_SEARCH_API_KEY")
			tavilyKey := cfg.Secret("TAVILY_API_KEY")

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			resp, err := search.Search(ctx, braveKey, tavilyKey, search.Options{
				Query:     query,
				Count:     searchCount,
				Freshness: searchFreshness,
				Country:   searchCountry,
			})
			if err != nil {
				return err
			}

			if searchJSON {
				data, err := json.MarshalIndent(resp, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
			} else {
				fmt.Print(search.Markdown(resp))
			}
			return nil
		},
	}
	searchCmd.Flags().IntVarP(&searchCount, "num", "n", 5, "Number of results")
	searchCmd.Flags().StringVarP(&searchFreshness, "freshness", "f", "", "Time filter: pd (24h), pw (7d), pm (31d), py (1yr)")
	searchCmd.Flags().StringVar(&searchCountry, "country", "", "Country code (e.g. us, jp)")
	searchCmd.Flags().BoolVar(&searchJSON, "json", false, "Output as JSON")

	// pairing command group
	pairingCmd := &cobra.Command{
		Use:   "pairing",
		Short: "Manage pairing requests and allowlist",
	}

	pairingListCmd := &cobra.Command{
		Use:   "list",
		Short: "List pending pairing requests",
		RunE: func(cmd *cobra.Command, args []string) error {
			configDir := config.DefaultConfigDir()
			pairingPath := filepath.Join(configDir, "pairing.json")

			requests, err := telegram.LoadPendingFromFile(pairingPath)
			if err != nil {
				return fmt.Errorf("load pending requests: %w", err)
			}

			if len(requests) == 0 {
				fmt.Println("No pending pairing requests.")
				return nil
			}

			fmt.Printf("Pending pairing requests: %d\n\n", len(requests))
			for _, req := range requests {
				name := req.FirstName
				if req.LastName != "" {
					name += " " + req.LastName
				}
				username := ""
				if req.Username != "" {
					username = " (@" + req.Username + ")"
				}
				fmt.Printf("  Code: %s\n  User: %s%s (ID: %d)\n  Chat: %d\n  Created: %s\n\n",
					req.Code, name, username, req.UserID, req.ChatID, req.CreatedAt)
			}
			return nil
		},
	}

	pairingApproveCmd := &cobra.Command{
		Use:   "approve <code>",
		Short: "Approve a pending pairing request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			configDir := config.DefaultConfigDir()
			pairingPath := filepath.Join(configDir, "pairing.json")
			allowlistStore := telegram.NewAllowlistStore(filepath.Join(configDir, "allowlist.json"))

			code := strings.ToUpper(strings.TrimSpace(args[0]))
			req, err := telegram.ApproveFromFile(pairingPath, allowlistStore, code)
			if err != nil {
				return err
			}

			name := req.FirstName
			if req.LastName != "" {
				name += " " + req.LastName
			}
			username := ""
			if req.Username != "" {
				username = " (@" + req.Username + ")"
			}
			fmt.Printf("Approved: %s%s (ID: %d)\n", name, username, req.UserID)
			return nil
		},
	}

	pairingAllowlistCmd := &cobra.Command{
		Use:   "allowlist",
		Short: "List approved users from the dynamic allowlist",
		RunE: func(cmd *cobra.Command, args []string) error {
			configDir := config.DefaultConfigDir()
			allowlistStore := telegram.NewAllowlistStore(filepath.Join(configDir, "allowlist.json"))

			users, err := allowlistStore.ListApproved()
			if err != nil {
				return fmt.Errorf("load allowlist: %w", err)
			}

			if len(users) == 0 {
				fmt.Println("No approved users in dynamic allowlist.")
				return nil
			}

			fmt.Printf("Approved users: %d\n\n", len(users))
			for _, u := range users {
				username := ""
				if u.Username != "" {
					username = " (@" + u.Username + ")"
				}
				fmt.Printf("  %s%s (ID: %d) — approved %s\n",
					u.FirstName, username, u.UserID, u.ApprovedAt)
			}
			return nil
		},
	}

	pairingRevokeCmd := &cobra.Command{
		Use:   "revoke <user-id>",
		Short: "Revoke a user from the dynamic allowlist",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			configDir := config.DefaultConfigDir()
			allowlistStore := telegram.NewAllowlistStore(filepath.Join(configDir, "allowlist.json"))

			var userID int64
			if _, err := fmt.Sscanf(args[0], "%d", &userID); err != nil {
				return fmt.Errorf("invalid user ID: %s", args[0])
			}

			if err := allowlistStore.Remove(userID); err != nil {
				return err
			}
			fmt.Printf("Revoked user ID %d\n", userID)
			return nil
		},
	}

	// mcp command — MCP stdio server for Claude CLI
	mcpCmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run MCP server on stdio (used by Claude CLI)",
		RunE: func(cmd *cobra.Command, args []string) error {
			sockPath := os.Getenv("SHELL_BRIDGE_SOCK")
			if sockPath == "" {
				sockPath = rpc.DefaultSocketPath()
			}
			return shellmcp.Serve(context.Background(), sockPath)
		},
	}

	pairingCmd.AddCommand(pairingListCmd, pairingApproveCmd, pairingAllowlistCmd, pairingRevokeCmd)
	sessionRotateCmd := &cobra.Command{
		Use:   "rotate <chat-id> [thread-id]",
		Short: "Flag a session for rotation on its next turn",
		Args:  cobra.RangeArgs(1, 2),
		Long: `Sets the rotate_pending flag on the given session. The next message in that
chat will close the current generation (summarizing the tail and packing
relevant memories into session_summaries) and start a fresh one with a
rebuilt system prompt. See docs/SESSION-LIFECYCLE.md.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadSessionConfig()
			st, err := store.Open(cfg.Store.DBPath)
			if err != nil {
				return err
			}
			defer st.Close()

			var chatID int64
			if _, err := fmt.Sscanf(args[0], "%d", &chatID); err != nil {
				return fmt.Errorf("invalid chat ID: %s", args[0])
			}
			var threadID int64
			if len(args) == 2 {
				if _, err := fmt.Sscanf(args[1], "%d", &threadID); err != nil {
					return fmt.Errorf("invalid thread ID: %s", args[1])
				}
			}

			if err := st.SetRotatePending(chatID, threadID, true); err != nil {
				return err
			}
			fmt.Printf("Flagged chat %d thread %d for rotation on next turn.\n", chatID, threadID)
			return nil
		},
	}

	var sessionInspectFull bool
	sessionInspectCmd := &cobra.Command{
		Use:   "inspect <chat-id> [thread-id]",
		Short: "Show lifecycle details for a session (generation, hash, age, flags)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadSessionConfig()
			st, err := store.Open(cfg.Store.DBPath)
			if err != nil {
				return err
			}
			defer st.Close()

			var chatID int64
			if _, err := fmt.Sscanf(args[0], "%d", &chatID); err != nil {
				return fmt.Errorf("invalid chat ID: %s", args[0])
			}
			var threadID int64
			if len(args) == 2 {
				if _, err := fmt.Sscanf(args[1], "%d", &threadID); err != nil {
					return fmt.Errorf("invalid thread ID: %s", args[1])
				}
			}

			sess, err := st.GetSession(chatID, threadID)
			if err != nil {
				return err
			}
			if sess == nil {
				fmt.Printf("No session for chat %d thread %d.\n", chatID, threadID)
				return nil
			}

			hashShort := sess.PrefixHash
			if len(hashShort) > 12 {
				hashShort = hashShort[:12]
			}
			uuidShort := sess.ProviderSessionID
			if len(uuidShort) > 12 {
				uuidShort = uuidShort[:12]
			}
			age := time.Since(sess.GenerationStartedAt).Round(time.Minute)

			fmt.Printf("Session %d (chat %d, thread %d)\n", sess.ID, sess.ChatID, sess.MessageThreadID)
			fmt.Printf("  Generation:  %d (age %s, started %s)\n",
				sess.Generation, age, sess.GenerationStartedAt.Format("2006-01-02 15:04 MST"))
			fmt.Printf("  Claude UUID: %s\n", uuidShort)
			fmt.Printf("  PrefixHash:  %s\n", hashShort)
			fmt.Printf("  Status:      %s\n", sess.Status)
			fmt.Printf("  Compact:     %s\n", stringOr(sess.CompactState, "idle"))
			fmt.Printf("  RotatePend.: %v\n", sess.RotatePending)
			fmt.Printf("  Updated:     %s\n", sess.UpdatedAt.Format("2006-01-02 15:04 MST"))

			if sm, err := st.GetLatestSessionSummary(chatID, threadID); err == nil && sm != nil {
				fmt.Printf("\nLast carry-forward summary (generation %d, closed %s):\n",
					sm.Generation, sm.ClosedAt.Format("2006-01-02 15:04 MST"))
				// Print first 6 lines to keep inspect output readable.
				lines := strings.SplitN(sm.Summary, "\n", 7)
				if len(lines) > 6 {
					lines = lines[:6]
					lines = append(lines, "... (truncated)")
				}
				for _, line := range lines {
					fmt.Printf("  %s\n", line)
				}
				if sm.MemoryPack != "" {
					fmt.Printf("  Memory pack: %d bytes of JSON\n", len(sm.MemoryPack))
				}
			}

			if sessionInspectFull {
				renderFullPrompt(cfg, st, sess, chatID, threadID)
			}
			return nil
		},
	}
	sessionInspectCmd.Flags().BoolVar(&sessionInspectFull, "full", false,
		"Dry-run render Channel A (system prompt) and Channel B (per-turn prefix) for this chat")

	sessionCmd.AddCommand(sessionListCmd, sessionKillCmd, sessionRotateCmd, sessionInspectCmd)
	rootCmd.AddCommand(initCmd, daemonCmd, sendCmd, statusCmd, sessionCmd, restartCmd, stopCmd, searchCmd, pairingCmd, mcpCmd, newMultiCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func stringOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// renderFullPrompt prints a dry-run of Channel A (system prompt) and Channel B
// (per-turn prefix) as they would be assembled for this chat's next send.
// Mirrors bridge.go's system-prompt assembly order; if the bridge changes
// ordering, this helper will drift and needs updating alongside it.
func renderFullPrompt(cfg config.Config, st *store.Store, sess *store.Session, chatID, threadID int64) {
	ctx := context.Background()

	fmt.Println()
	fmt.Println("=== Channel A (frozen system prompt — what a fresh send would cache) ===")

	// Agent identity (cfg.Agent.SystemPrompt — same thing daemon calls SetAgentIdentity with).
	identity := cfg.Agent.SystemPrompt
	printSection("agent identity", identity)

	// Memory block: pinned memories + ghost-search instruction.
	// Uses the same memory.SystemPrompt() call the bridge makes.
	if cfg.Memory.Enabled {
		mem, err := openMemoryFromConfig(cfg)
		if err != nil {
			fmt.Printf("  (memory section unavailable: %v)\n\n", err)
		} else {
			defer mem.Close()
			block := mem.SystemPrompt(ctx, chatID)
			printSection("memory (pinned + ghost-search instruction)", block)
		}
	}

	// Timestamp guidance (static block; reproduced from bridge/prompt.go).
	if cfg.Scheduler.Enabled {
		tz := cfg.Scheduler.Timezone
		if tz == "" {
			tz = "UTC"
		}
		ts := "\n\n## Current Time\n\n" +
			"Each user message is prefixed with `[Current time: ...]` containing the authoritative " +
			"date, day of week, and time. **ALWAYS read that marker to determine what day it is.** " +
			"Do not trust dates from conversation history, compacted summaries, or your own prior " +
			"responses — only trust the `[Current time: ...]` marker on the current turn.\n" +
			"Timezone: " + tz + "\n"
		printSection("timestamp guidance", ts)
	}

	// Skills catalog.
	if cfg.Skills.Enabled {
		reg := loadSkillRegistryFromConfig(cfg)
		if reg != nil {
			printSection("skills catalog", reg.CatalogPrompt())
		} else {
			fmt.Println("  (no skills loaded)")
			fmt.Println()
		}
	}

	// Channel B dry-run.
	fmt.Println()
	fmt.Println("=== Channel B (per-turn prefix — what would ride the next user message) ===")

	var parts []string

	// Current time block.
	if cfg.Scheduler.Enabled {
		tz := cfg.Scheduler.Timezone
		if tz == "" {
			tz = "UTC"
		}
		loc, _ := time.LoadLocation(tz)
		if loc == nil {
			loc = time.UTC
		}
		now := time.Now().In(loc)
		parts = append(parts, fmt.Sprintf("[Current time: %s | %s]",
			now.Format("Monday 2006-01-02 15:04 MST"), tz))
	}

	// Carry-forward block (only if session is fresh — ProviderSessionID == "").
	if sess.ProviderSessionID == "" {
		if sm, err := st.GetLatestSessionSummary(chatID, threadID); err == nil && sm != nil && sm.Generation == sess.Generation-1 {
			parts = append(parts,
				fmt.Sprintf("[Previously in this chat (generation %d summary):\n%s\n]", sm.Generation, strings.TrimSpace(sm.Summary)))
			if sm.MemoryPack != "" {
				parts = append(parts, fmt.Sprintf("[Relevant memory context: %d bytes of JSON pack]", len(sm.MemoryPack)))
			}
		}
	}

	// Pinned-memory delta.
	if cfg.Memory.Enabled {
		mem, err := openMemoryFromConfig(cfg)
		if err == nil {
			defer mem.Close()
			if sess.PrefixHash == "" && sess.ProviderSessionID != "" {
				parts = append(parts, "[Memory updates since session start: (legacy session, hash will be stamped on next turn)]")
			} else {
				delta, _, tokens := mem.PinnedDelta(ctx, chatID, sess.GenerationStartedAt, sess.PrefixHash)
				switch {
				case tokens > 1000:
					parts = append(parts, fmt.Sprintf("[Memory updates since session start: (%d tokens — would flip rotate_pending)]", tokens))
				case delta != "":
					parts = append(parts, "[Memory updates since session start:\n"+strings.TrimRight(delta, "\n")+"]")
				default:
					parts = append(parts, "[Memory updates since session start: (none)]")
				}
			}
		}
	}

	// Active tasks.
	if chatID != 0 {
		if tasks, err := st.PendingTasks(chatID); err == nil && len(tasks) > 0 {
			var sb strings.Builder
			sb.WriteString("[Active tasks:\n")
			for _, t := range tasks {
				sb.WriteString("- ")
				sb.WriteString(t.Description)
				sb.WriteString("\n")
			}
			sb.WriteString("]")
			parts = append(parts, sb.String())
		} else {
			parts = append(parts, "[Active tasks: (none)]")
		}
	}

	if len(parts) == 0 {
		fmt.Println("  (no Channel B blocks would be prepended)")
	}
	for _, p := range parts {
		fmt.Println()
		fmt.Println(p)
	}
	fmt.Println()
}

// printSection prints a labeled section with token estimate and content.
// Used by renderFullPrompt to keep the Channel A dump scannable.
func printSection(label, content string) {
	content = strings.TrimSpace(content)
	approxTokens := len(content) / 4
	fmt.Printf("\n--- %s (~%d tokens, %d chars) ---\n", label, approxTokens, len(content))
	if content == "" {
		fmt.Println("  (empty)")
		return
	}
	fmt.Println(content)
}

// openMemoryFromConfig constructs a memory.Memory matching the daemon's config,
// so dry-run rendering sees the same pinned set the bridge would.
func openMemoryFromConfig(cfg config.Config) (*memory.Memory, error) {
	dbPath := cfg.Memory.DBPath
	if dbPath == "" {
		dbPath = filepath.Join(config.DefaultConfigDir(), "memory.db")
	}
	profiles := map[string]memory.ProfileConfig{}
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
	return memory.New(
		dbPath, cfg.Memory.Budget,
		cfg.Memory.GlobalNamespaces, cfg.Memory.GlobalBudget,
		cfg.Memory.SystemNamespaces, cfg.Memory.SystemBudget,
		profiles, cfg.Memory.ChatProfileMap(),
	)
}

// loadSkillRegistryFromConfig mirrors daemon.go's skill loading sequence so
// dry-run rendering includes the same skills the running daemon has.
func loadSkillRegistryFromConfig(cfg config.Config) *skill.Registry {
	var all []*skill.Skill

	// Global skills.
	globalDir := cfg.Skills.Dir
	if globalDir == "" {
		globalDir = filepath.Join(config.DefaultConfigDir(), "skills")
	}
	if s, err := skill.LoadDir(globalDir); err == nil {
		all = append(all, s...)
	}

	// Project skills.
	if cfg.Claude.WorkDir != "" {
		projectDir := filepath.Join(cfg.Claude.WorkDir, ".agent", "skills")
		if s, err := skill.LoadDir(projectDir); err == nil {
			all = append(all, s...)
		}
	}

	// Per-agent skills (derived from PID file directory — same rule as daemon.go).
	if cfg.Daemon.PIDFile != "" {
		agentDir := filepath.Join(filepath.Dir(cfg.Daemon.PIDFile), "skills")
		if s, err := skill.LoadDir(agentDir); err == nil {
			all = append(all, s...)
		}
	}

	if len(all) == 0 {
		return nil
	}
	return skill.NewRegistry(all)
}

func loadConfig() config.Config {
	return loadConfigFrom("")
}

func loadConfigFrom(path string) config.Config {
	if path == "" {
		path = filepath.Join(config.DefaultConfigDir(), "config.json")
	}
	cfg, err := config.Load(path)
	if err != nil {
		slog.Warn("failed to load config, using defaults", "error", err)
		cfg = config.Default()
	}
	return cfg
}
