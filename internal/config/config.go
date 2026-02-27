package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	Telegram TelegramConfig `json:"telegram"`
	Claude   ClaudeConfig   `json:"claude"`
	Store    StoreConfig    `json:"store"`
	Daemon   DaemonConfig   `json:"daemon"`
	Memory   MemoryConfig   `json:"memory"`
	Planner  PlannerConfig  `json:"planner"`
	Reload   ReloadConfig   `json:"reload"`
}

type TelegramConfig struct {
	TokenEnv     string  `json:"token_env"`
	AllowedUsers []int64 `json:"allowed_users"`
}

type ClaudeConfig struct {
	Binary      string        `json:"binary"`
	Model       string        `json:"model"`
	Timeout     time.Duration `json:"timeout"`
	MaxSessions int           `json:"max_sessions"`
	WorkDir     string        `json:"work_dir"`
	ExtraArgs   []string      `json:"extra_args"`
}

type StoreConfig struct {
	DBPath string `json:"db_path"`
}

type DaemonConfig struct {
	PIDFile string `json:"pid_file"`
}

type MemoryConfig struct {
	DBPath           string   `json:"db_path"`
	Enabled          bool     `json:"enabled"`
	Budget           int      `json:"budget"`            // token budget for context injection
	GlobalNamespaces []string `json:"global_namespaces"` // namespace patterns for background context
	GlobalBudget     int      `json:"global_budget"`     // token budget for global context (default 500)
	SystemNamespaces []string `json:"system_namespaces"` // always-on via --append-system-prompt (no search)
	SystemBudget     int      `json:"system_budget"`     // token cap for system prompt (default 3000)
}

type ReloadConfig struct {
	Enabled   bool   `json:"enabled"`
	SourceDir string `json:"source_dir"` // auto-detected from go.mod if empty
	Debounce  string `json:"debounce"`   // duration string, default "500ms"
}

type PlannerConfig struct {
	Enabled              bool          `json:"enabled"`
	TestCmd              string        `json:"test_cmd"`              // test command (e.g. "go test ./...")
	Conventions          string        `json:"conventions"`           // inline conventions text for the reviewer
	MaxRetries           int           `json:"max_retries"`           // retries per task on needs_revision
	AutoApproveThreshold int           `json:"auto_approve_threshold"` // max diff lines to auto-approve (0 = always review)
	Timeout              time.Duration `json:"timeout"`               // per-claude-invocation timeout (default 30m)
	Worktree             bool          `json:"worktree"`              // isolate plan execution in a git worktree
}

func DefaultConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".teeny-relay")
}

func Default() Config {
	configDir := DefaultConfigDir()
	return Config{
		Telegram: TelegramConfig{
			TokenEnv:     "TELEGRAM_BOT_TOKEN",
			AllowedUsers: []int64{},
		},
		Claude: ClaudeConfig{
			Binary:      "claude",
			Model:       "",
			Timeout:     30 * time.Minute,
			MaxSessions: 4,
			WorkDir:     "",
			ExtraArgs:   []string{},
		},
		Store: StoreConfig{
			DBPath: filepath.Join(configDir, "relay.db"),
		},
		Daemon: DaemonConfig{
			PIDFile: filepath.Join(configDir, "relay.pid"),
		},
		Memory: MemoryConfig{
			DBPath:           filepath.Join(configDir, "memory.db"),
			Enabled:          true,
			Budget:           2000,
			GlobalNamespaces: []string{},
			GlobalBudget:     500,
			SystemNamespaces: []string{},
			SystemBudget:     3000,
		},
		Planner: PlannerConfig{
			Enabled:              false,
			TestCmd:              "",
			MaxRetries:           2,
			AutoApproveThreshold: 80,
		},
		Reload: ReloadConfig{
			Enabled:   false,
			SourceDir: "",
			Debounce:  "500ms",
		},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return cfg, fmt.Errorf("resolving config path: %w", err)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("reading config %s: %w", absPath, err)
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing config %s: %w", absPath, err)
	}

	return cfg, nil
}

func (c Config) TelegramToken() string {
	return os.Getenv(c.Telegram.TokenEnv)
}

// MarshalJSON implements custom JSON marshaling for duration fields.
func (c ClaudeConfig) MarshalJSON() ([]byte, error) {
	type Alias ClaudeConfig
	return json.Marshal(&struct {
		Timeout string `json:"timeout"`
		*Alias
	}{
		Timeout: c.Timeout.String(),
		Alias:   (*Alias)(&c),
	})
}

// UnmarshalJSON implements custom JSON unmarshaling for duration fields.
func (c *ClaudeConfig) UnmarshalJSON(data []byte) error {
	type Alias ClaudeConfig
	aux := &struct {
		Timeout string `json:"timeout"`
		*Alias
	}{
		Alias: (*Alias)(c),
	}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if aux.Timeout != "" {
		d, err := time.ParseDuration(aux.Timeout)
		if err != nil {
			return fmt.Errorf("parsing timeout: %w", err)
		}
		c.Timeout = d
	}
	return nil
}

// MarshalJSON implements custom JSON marshaling for PlannerConfig duration fields.
func (c PlannerConfig) MarshalJSON() ([]byte, error) {
	type Alias PlannerConfig
	return json.Marshal(&struct {
		Timeout string `json:"timeout,omitempty"`
		*Alias
	}{
		Timeout: c.Timeout.String(),
		Alias:   (*Alias)(&c),
	})
}

// UnmarshalJSON implements custom JSON unmarshaling for PlannerConfig duration fields.
func (c *PlannerConfig) UnmarshalJSON(data []byte) error {
	type Alias PlannerConfig
	aux := &struct {
		Timeout string `json:"timeout"`
		*Alias
	}{
		Alias: (*Alias)(c),
	}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if aux.Timeout != "" {
		d, err := time.ParseDuration(aux.Timeout)
		if err != nil {
			return fmt.Errorf("parsing planner timeout: %w", err)
		}
		c.Timeout = d
	}
	return nil
}
