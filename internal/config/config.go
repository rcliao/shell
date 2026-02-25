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
			Timeout:     5 * time.Minute,
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
