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
	"syscall"

	"github.com/rcliao/teeny-relay/internal/config"
	"github.com/rcliao/teeny-relay/internal/daemon"
	"github.com/rcliao/teeny-relay/internal/process"
	"github.com/rcliao/teeny-relay/internal/store"
	"github.com/spf13/cobra"
)

func main() {
	var verbose bool

	rootCmd := &cobra.Command{
		Use:   "relay",
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
			fmt.Println("4. Run: relay daemon")
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

			// Ensure clean shutdown
			go func() {
				<-ctx.Done()
				d.Shutdown()
			}()

			return d.Run(ctx)
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

	sessionCmd.AddCommand(sessionListCmd, sessionKillCmd)
	rootCmd.AddCommand(initCmd, daemonCmd, sendCmd, statusCmd, sessionCmd)

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
