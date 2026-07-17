package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rcliao/shell/internal/config"
	"github.com/rcliao/shell/internal/daemon"
	"github.com/spf13/cobra"
)

func newMultiCmd() *cobra.Command {
	multiCmd := &cobra.Command{
		Use:   "multi",
		Short: "Manage multiple agent daemons",
	}

	multiCmd.AddCommand(
		newMultiStartCmd(),
		newMultiStopCmd(),
		newMultiStatusCmd(),
		newMultiRestartCmd(),
	)

	return multiCmd
}

// discoverAgents finds all agent config directories under ~/.shell/agents/*/config.json.
func discoverAgents() ([]string, error) {
	agentsDir := filepath.Join(config.DefaultConfigDir(), "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("agents directory not found: %s", agentsDir)
		}
		return nil, err
	}

	var agents []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		cfgPath := filepath.Join(agentsDir, entry.Name(), "config.json")
		if _, err := os.Stat(cfgPath); err == nil {
			agents = append(agents, entry.Name())
		}
	}
	return agents, nil
}

// agentConfigPath returns the config.json path for a named agent.
func agentConfigPath(name string) string {
	return filepath.Join(config.DefaultConfigDir(), "agents", name, "config.json")
}

// agentPIDFile returns the PID file path for a named agent by loading its config.
func agentPIDFile(name string) string {
	cfg, err := config.Load(agentConfigPath(name))
	if err != nil {
		return filepath.Join(config.DefaultConfigDir(), "agents", name, "shell.pid")
	}
	if cfg.Daemon.PIDFile != "" {
		return cfg.Daemon.PIDFile
	}
	return filepath.Join(config.DefaultConfigDir(), "agents", name, "shell.pid")
}

func newMultiStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start all agent daemons",
		RunE: func(cmd *cobra.Command, args []string) error {
			agents, err := discoverAgents()
			if err != nil {
				return err
			}
			if len(agents) == 0 {
				return fmt.Errorf("no agents found in %s/agents/", config.DefaultConfigDir())
			}

			binary, err := os.Executable()
			if err != nil {
				return fmt.Errorf("cannot resolve executable: %w", err)
			}

			// Preflight: resolve each agent's telegram token BEFORE spawning.
			// On 7/15 a locked login keychain made every child die 1s after
			// spawn — with stderr discarded, "started (pid X)" printed while
			// nothing survived and both agents were silently down for 1h+.
			for _, name := range agents {
				cfg, cerr := config.Load(agentConfigPath(name))
				if cerr != nil {
					return fmt.Errorf("%s: load config: %w", name, cerr)
				}
				config.OpenSecretStore(cfg.Secrets)
				if cfg.TelegramToken() == "" {
					return fmt.Errorf("%s: telegram token unresolvable (secret %q) — NOT starting anything.\n"+
						"\n"+
						"Likely a locked login keychain (happens after every reboot/logout when the\n"+
						"keychain password differs from the login password). Unlock it:\n"+
						"  security unlock-keychain ~/Library/Keychains/login.keychain-db\n"+
						"  (use the KEYCHAIN's password — may be an older account password)\n"+
						"then re-run 'shell multi start'.\n"+
						"\n"+
						"PERMANENT FIX (one time) — resync so reboots auto-unlock and this error\n"+
						"never recurs:\n"+
						"  security set-keychain-password ~/Library/Keychains/login.keychain-db\n"+
						"  Old Password: the keychain's current (older) password\n"+
						"  New Password: your current macOS login password\n"+
						"\n"+
						"Bypass for right now (no keychain): export the tokens and re-run:\n"+
						"  TELEGRAM_BOT_TOKEN=... UMBREONMINI_BOT_TOKEN=... ./shell multi start",
						name, cfg.Telegram.TokenEnv)
				}
			}

			for _, name := range agents {
				cfgPath := agentConfigPath(name)
				pidFile := agentPIDFile(name)

				// Check if already running.
				if pid, err := daemon.ReadPID(pidFile); err == nil {
					if err := syscall.Kill(pid, 0); err == nil {
						fmt.Printf("  %s: already running (pid %d)\n", name, pid)
						continue
					}
				}

				// Spawn daemon in background.
				proc := exec.Command(binary, "daemon", "--config", cfgPath)
				// Panics and other raw-stderr output must land somewhere
				// diagnosable: a daemon that died silently mid-init on 7/16
				// (55-min outage) left no trace because stderr went to
				// /dev/null. syscall.Exec restarts inherit this fd, so every
				// generation keeps writing here.
				stderrLog := agentStderrLog(name)
				proc.Stdout = stderrLog
				proc.Stderr = stderrLog
				proc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
				if err := proc.Start(); err != nil {
					slog.Error("failed to start agent", "name", name, "error", err)
					fmt.Printf("  %s: FAILED (%v)\n", name, err)
					continue
				}
				pid := proc.Process.Pid
				go proc.Wait()
				// Verify the child survives init (the old code printed
				// "started" unconditionally, masking 1-second deaths).
				time.Sleep(2 * time.Second)
				if err := syscall.Kill(pid, 0); err != nil {
					fmt.Printf("  %s: DIED during init (pid %d) — check %s\n", name, pid,
						filepath.Join(filepath.Dir(agentPIDFile(name)), "daemon.log"))
					continue
				}
				fmt.Printf("  %s: started (pid %d)\n", name, pid)
			}
			return nil
		},
	}
}

func newMultiStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop all agent daemons",
		RunE: func(cmd *cobra.Command, args []string) error {
			agents, err := discoverAgents()
			if err != nil {
				return err
			}

			for _, name := range agents {
				pidFile := agentPIDFile(name)
				pid, err := daemon.ReadPID(pidFile)
				if err != nil {
					fmt.Printf("  %s: not running (no PID file)\n", name)
					continue
				}
				if err := syscall.Kill(pid, 0); err != nil {
					fmt.Printf("  %s: not running (stale PID %d)\n", name, pid)
					continue
				}
				if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
					fmt.Printf("  %s: failed to stop (pid %d): %v\n", name, pid, err)
					continue
				}
				// Wait for exit.
				stopped := false
				for i := 0; i < 50; i++ {
					time.Sleep(100 * time.Millisecond)
					if err := syscall.Kill(pid, 0); err != nil {
						stopped = true
						break
					}
				}
				if stopped {
					fmt.Printf("  %s: stopped\n", name)
				} else {
					fmt.Printf("  %s: sent SIGTERM (pid %d), still shutting down\n", name, pid)
				}
			}
			return nil
		},
	}
}

func newMultiStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show status of all agent daemons",
		RunE: func(cmd *cobra.Command, args []string) error {
			agents, err := discoverAgents()
			if err != nil {
				return err
			}
			if len(agents) == 0 {
				fmt.Println("No agents configured.")
				return nil
			}

			for _, name := range agents {
				pidFile := agentPIDFile(name)
				pid, err := daemon.ReadPID(pidFile)
				if err != nil {
					fmt.Printf("  %s: stopped\n", name)
					continue
				}
				if err := syscall.Kill(pid, 0); err != nil {
					fmt.Printf("  %s: stopped (stale PID %d)\n", name, pid)
					continue
				}
				// Load config to show agent identity.
				cfg, _ := config.Load(agentConfigPath(name))
				botUser := cfg.Agent.BotUsername
				prob := cfg.Agent.BroadcastProbability
				var details []string
				if botUser != "" {
					details = append(details, "@"+botUser)
				}
				details = append(details, fmt.Sprintf("broadcast=%.0f%%", prob*100))
				fmt.Printf("  %s: running (pid %d) [%s]\n", name, pid, strings.Join(details, ", "))
			}
			return nil
		},
	}
}

func newMultiRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart all agent daemons",
		RunE: func(cmd *cobra.Command, args []string) error {
			agents, err := discoverAgents()
			if err != nil {
				return err
			}

			for _, name := range agents {
				pidFile := agentPIDFile(name)
				pid, err := daemon.ReadPID(pidFile)
				if err != nil {
					continue
				}
				if err := syscall.Kill(pid, 0); err != nil {
					continue
				}
				if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
					fmt.Printf("  %s: failed to restart (pid %d): %v\n", name, pid, err)
					continue
				}
				fmt.Printf("  %s: sent SIGHUP (pid %d)\n", name, pid)
			}

			// Start any agents that weren't running.
			binary, err := os.Executable()
			if err != nil {
				return fmt.Errorf("cannot resolve executable: %w", err)
			}
			for _, name := range agents {
				pidFile := agentPIDFile(name)
				pid, _ := daemon.ReadPID(pidFile)
				if pid > 0 {
					if err := syscall.Kill(pid, 0); err == nil {
						continue // already running (or just restarted)
					}
				}
				cfgPath := agentConfigPath(name)
				proc := exec.Command(binary, "daemon", "--config", cfgPath)
				// Panics and other raw-stderr output must land somewhere
				// diagnosable: a daemon that died silently mid-init on 7/16
				// (55-min outage) left no trace because stderr went to
				// /dev/null. syscall.Exec restarts inherit this fd, so every
				// generation keeps writing here.
				stderrLog := agentStderrLog(name)
				proc.Stdout = stderrLog
				proc.Stderr = stderrLog
				proc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
				if err := proc.Start(); err != nil {
					fmt.Printf("  %s: FAILED to start (%v)\n", name, err)
					continue
				}
				fmt.Printf("  %s: started (pid %d)\n", name, proc.Process.Pid)
				go proc.Wait()
			}
			return nil
		},
	}
}

// agentStderrLog opens (append) the agent's daemon.stderr.log for spawned
// children, so panics survive. Returns nil (= /dev/null) if it cannot open.
func agentStderrLog(name string) *os.File {
	f, err := os.OpenFile(filepath.Join(filepath.Dir(agentConfigPath(name)), "daemon.stderr.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	return f
}
