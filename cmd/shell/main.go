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
	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/rpc"
	"github.com/rcliao/shell/internal/search"
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
	daemonCmd := &cobra.Command{
		Use:   "daemon",
		Short: "Start the Telegram bot daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			if verbose {
				slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
			}

			cfg := loadConfig()

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
				fmt.Printf("  Chat ID: %d\n  Session: %s\n  Status: %s\n  Created: %s\n  Updated: %s\n\n",
					s.ChatID, s.ClaudeSessionID[:12]+"...", s.Status,
					s.CreatedAt.Format("2006-01-02 15:04:05"),
					s.UpdatedAt.Format("2006-01-02 15:04:05"),
				)
			}
			return nil
		},
	}

	// session command group
	sessionCmd := &cobra.Command{
		Use:   "session",
		Short: "Manage sessions",
	}

	sessionListCmd := &cobra.Command{
		Use:   "list",
		Short: "List all sessions",
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
				fmt.Println("No sessions.")
				return nil
			}

			for _, s := range sessions {
				fmt.Printf("%d\t%s\t%s\t%s\n",
					s.ChatID, s.ClaudeSessionID[:12], s.Status,
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
			cfg := loadConfig()
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
			proc.Kill(chatID)

			if err := st.DeleteSession(chatID); err != nil {
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
			sockPath := rpc.DefaultSocketPath()
			return shellmcp.Serve(context.Background(), sockPath)
		},
	}

	pairingCmd.AddCommand(pairingListCmd, pairingApproveCmd, pairingAllowlistCmd, pairingRevokeCmd)
	sessionCmd.AddCommand(sessionListCmd, sessionKillCmd)
	rootCmd.AddCommand(initCmd, daemonCmd, sendCmd, statusCmd, sessionCmd, restartCmd, stopCmd, searchCmd, pairingCmd, mcpCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func loadConfig() config.Config {
	configPath := filepath.Join(config.DefaultConfigDir(), "config.json")
	cfg, err := config.Load(configPath)
	if err != nil {
		slog.Warn("failed to load config, using defaults", "error", err)
		cfg = config.Default()
	}
	return cfg
}
