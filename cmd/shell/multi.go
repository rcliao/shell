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
				proc.Stdout = nil
				proc.Stderr = nil
				proc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
				if err := proc.Start(); err != nil {
					slog.Error("failed to start agent", "name", name, "error", err)
					fmt.Printf("  %s: FAILED (%v)\n", name, err)
					continue
				}
				fmt.Printf("  %s: started (pid %d)\n", name, proc.Process.Pid)
				// Detach — don't wait for the child.
				go proc.Wait()
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
				proc.Stdout = nil
				proc.Stderr = nil
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

