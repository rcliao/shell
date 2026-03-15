package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	secrets "github.com/rcliao/shell-secrets"
)

type Config struct {
	Telegram  TelegramConfig  `json:"telegram"`
	Claude    ClaudeConfig    `json:"claude"`
	Store     StoreConfig     `json:"store"`
	Daemon    DaemonConfig    `json:"daemon"`
	Memory    MemoryConfig    `json:"memory"`
	Planner   PlannerConfig   `json:"planner"`
	Scheduler SchedulerConfig `json:"scheduler"`
	Reload    ReloadConfig    `json:"reload"`
	Google    GoogleConfig    `json:"google"`
	Secrets   SecretsConfig   `json:"secrets"`
	Browser   BrowserConfig   `json:"browser"`
	Tunnel    TunnelConfig    `json:"tunnel"`
	PM        PMConfig        `json:"pm"`
	Skills    SkillsConfig    `json:"skills"`
}

type SkillsConfig struct {
	Enabled bool   `json:"enabled"`
	Dir     string `json:"dir"` // override global dir (default: ~/.shell/skills)
}

type PMConfig struct {
	Enabled  bool `json:"enabled"`
	MaxProcs int  `json:"max_procs"` // max concurrent processes, default 10
	LogLines int  `json:"log_lines"` // tail lines to keep per process, default 50
}

type TunnelConfig struct {
	Enabled         bool   `json:"enabled"`
	CloudflaredBin  string `json:"cloudflared_bin"`  // path to cloudflared binary, default "cloudflared"
	MaxTunnels      int    `json:"max_tunnels"`      // max concurrent tunnels, default 5
	DefaultProtocol string `json:"default_protocol"` // "http" or "https", default "http"
}

type BrowserConfig struct {
	Enabled        bool   `json:"enabled"`
	Headless       bool   `json:"headless"`
	TimeoutSeconds int    `json:"timeout_seconds"` // default: 30
	ChromePath     string `json:"chrome_path"`     // optional custom Chrome binary
}

type SecretsConfig struct {
	Enabled   bool   `json:"enabled"`
	StorePath string `json:"store_path"` // default: ~/.shell-secrets/secrets.enc
}

var globalSecretStore secrets.Store

type TelegramConfig struct {
	TokenEnv          string            `json:"token_env"`
	AllowedUsers      []int64           `json:"allowed_users"`
	DMPolicy          string            `json:"dm_policy"`           // "pairing", "allowlist", or "disabled" (default: "allowlist")
	GroupPolicy       string            `json:"group_policy"`        // "pairing", "allowlist", or "disabled" (default: "disabled")
	GroupAllowedUsers []int64           `json:"group_allowed_users"` // separate allowlist for group chats
	ReactionMap       map[string]string `json:"reaction_map"`        // emoji → action (e.g. "👍":"go", "👎":"stop")
}

// UnmarshalJSON replaces (rather than merges) the ReactionMap when the user
// provides one in JSON config, so custom maps fully override the defaults.
func (t *TelegramConfig) UnmarshalJSON(data []byte) error {
	type Alias TelegramConfig
	aux := (*Alias)(t)
	hadDefaults := len(t.ReactionMap) > 0
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	// json.Unmarshal merges maps. If the user supplied a reaction_map key we
	// need to drop any leftover default entries. Detect this by checking
	// whether the raw JSON contains "reaction_map".
	if hadDefaults {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err == nil {
			if _, provided := raw["reaction_map"]; provided {
				// Re-decode only the user-supplied map.
				var userMap struct {
					ReactionMap map[string]string `json:"reaction_map"`
				}
				json.Unmarshal(data, &userMap)
				t.ReactionMap = userMap.ReactionMap
			}
		}
	}
	return nil
}

type ClaudeConfig struct {
	Binary         string        `json:"binary"`
	Model          string        `json:"model"`
	Timeout        time.Duration `json:"timeout"`
	MaxSessions    int           `json:"max_sessions"`
	WorkDir        string        `json:"work_dir"`
	AllowedTools   []string      `json:"allowed_tools"`
	ExtraArgs      []string      `json:"extra_args"`
	PlaygroundDir  string        `json:"playground_dir"`  // writable sandbox dir, auto-approved for Write/Edit/Bash
	Bidirectional  bool          `json:"bidirectional"`   // use stream-json stdin (bidirectional protocol)
	SettingSources []string      `json:"setting_sources"` // e.g. ["user", "project"] for --setting-sources
}

type StoreConfig struct {
	DBPath string `json:"db_path"`
}

type DaemonConfig struct {
	PIDFile string `json:"pid_file"`
}

type Profile struct {
	AgentNS          string   `json:"agent_ns"`           // agent namespace (e.g. "agent:pikamini")
	SystemNamespaces []string `json:"system_namespaces"`  // deprecated: use agent_ns + pinned memories
	SystemBudget     int      `json:"system_budget"`
	GlobalNamespaces []string `json:"global_namespaces"`  // deprecated: use agent_ns + tag filtering
	GlobalBudget     int      `json:"global_budget"`
	Budget           int      `json:"budget"`
	ExchangeTTL      string   `json:"exchange_ttl"`       // "7d", "30d"
	ExchangeMaxUser  int      `json:"exchange_max_user"`  // 0 = default 200
	ExchangeMaxReply int      `json:"exchange_max_reply"` // 0 = default 300
	MemoryDirectives bool     `json:"memory_directives"`
	DirectiveNS      string   `json:"directive_ns"` // deprecated: use agent_ns + "learning" tag
}

type MemoryConfig struct {
	DBPath           string            `json:"db_path"`
	Enabled          bool              `json:"enabled"`
	Budget           int               `json:"budget"`            // token budget for context injection
	GlobalNamespaces []string          `json:"global_namespaces"` // namespace patterns for background context
	GlobalBudget     int               `json:"global_budget"`     // token budget for global context (default 500)
	SystemNamespaces []string          `json:"system_namespaces"` // always-on via --append-system-prompt (no search)
	SystemBudget     int               `json:"system_budget"`     // token cap for system prompt (default 3000)
	Profiles         map[string]Profile `json:"profiles"`          // name → profile
	ChatProfiles     map[string]string  `json:"chat_profiles"`     // chatID string → profile name
}

// ChatProfileMap converts string chatID keys to int64.
func (m MemoryConfig) ChatProfileMap() map[int64]string {
	out := make(map[int64]string, len(m.ChatProfiles))
	for k, v := range m.ChatProfiles {
		id, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			continue
		}
		out[id] = v
	}
	return out
}

type SchedulerConfig struct {
	Enabled        bool   `json:"enabled"`
	Timezone       string `json:"timezone"`        // default: "UTC"
	QuietHourStart int    `json:"quiet_hour_start"` // hour (0-23) when quiet hours begin, default: 22
	QuietHourEnd   int    `json:"quiet_hour_end"`   // hour (0-23) when quiet hours end, default: 7
}

type ReloadConfig struct {
	Enabled   bool   `json:"enabled"`
	SourceDir string `json:"source_dir"` // auto-detected from go.mod if empty
	Debounce  string `json:"debounce"`   // duration string, default "500ms"
}

type GoogleConfig struct {
	APIKeyEnv string        `json:"api_key_env"` // env var name (default: "GEMINI_API_KEY")
	Model     string        `json:"model"`       // default: "gemini-3.1-flash-image-preview"
	Timeout   time.Duration `json:"timeout"`     // default: 2m
}

// MarshalJSON implements custom JSON marshaling for GoogleConfig duration fields.
func (c GoogleConfig) MarshalJSON() ([]byte, error) {
	type Alias GoogleConfig
	return json.Marshal(&struct {
		Timeout string `json:"timeout,omitempty"`
		*Alias
	}{
		Timeout: c.Timeout.String(),
		Alias:   (*Alias)(&c),
	})
}

// UnmarshalJSON implements custom JSON unmarshaling for GoogleConfig duration fields.
func (c *GoogleConfig) UnmarshalJSON(data []byte) error {
	type Alias GoogleConfig
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
			return fmt.Errorf("parsing google timeout: %w", err)
		}
		c.Timeout = d
	}
	return nil
}

type PlannerConfig struct {
	Enabled              bool          `json:"enabled"`
	TestCmd              string        `json:"test_cmd"`               // test command (e.g. "go test ./...")
	Conventions          string        `json:"conventions"`            // inline conventions text for the reviewer
	VerifyInstructions   string        `json:"verify_instructions"`    // E2E verification commands for reviewer
	MaxRetries           int           `json:"max_retries"`            // retries per task on needs_revision
	AutoApproveThreshold int           `json:"auto_approve_threshold"` // max diff lines to auto-approve (0 = always review)
	Timeout              time.Duration `json:"timeout"`                // per-claude-invocation timeout (default 30m)
	Worktree             bool          `json:"worktree"`               // isolate plan execution in a git worktree
}

// DefaultReactionMap returns the built-in emoji→action mapping.
func DefaultReactionMap() map[string]string {
	return map[string]string{
		"👍": "go",
		"👎": "stop",
		"❌": "cancel",
		"📋": "status",
		"🔄": "regenerate",
		"📌": "remember",
		"🗑": "forget",
		"🔁": "retry",
	}
}

// ReactionAction returns the action string for the given emoji, or "" if unmapped.
func (t TelegramConfig) ReactionAction(emoji string) string {
	if t.ReactionMap == nil {
		return ""
	}
	return t.ReactionMap[emoji]
}

func DefaultConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".shell")
}

func Default() Config {
	configDir := DefaultConfigDir()
	return Config{
		Telegram: TelegramConfig{
			TokenEnv:     "TELEGRAM_BOT_TOKEN",
			AllowedUsers: []int64{},
			ReactionMap:  DefaultReactionMap(),
		},
		Claude: ClaudeConfig{
			Binary:       "claude",
			Model:        "",
			Timeout:      30 * time.Minute,
			MaxSessions:  4,
			WorkDir:      "",
			AllowedTools: []string{},
			ExtraArgs:    []string{},
		},
		Store: StoreConfig{
			DBPath: filepath.Join(configDir, "shell.db"),
		},
		Daemon: DaemonConfig{
			PIDFile: filepath.Join(configDir, "shell.pid"),
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
		Scheduler: SchedulerConfig{
			Enabled:        false,
			Timezone:       "UTC",
			QuietHourStart: 22,
			QuietHourEnd:   7,
		},
		Reload: ReloadConfig{
			Enabled:   false,
			SourceDir: "",
			Debounce:  "500ms",
		},
		Google: GoogleConfig{
			APIKeyEnv: "GEMINI_API_KEY",
			Model:     "gemini-3.1-flash-image-preview",
			Timeout:   2 * time.Minute,
		},
		Browser: BrowserConfig{
			Enabled:        false,
			Headless:       true,
			TimeoutSeconds: 30,
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
	if globalSecretStore != nil {
		if val, err := globalSecretStore.Get(c.Telegram.TokenEnv); err == nil {
			return val
		}
	}
	return os.Getenv(c.Telegram.TokenEnv)
}

func (c Config) GoogleAPIKey() string {
	if globalSecretStore != nil {
		if val, err := globalSecretStore.Get(c.Google.APIKeyEnv); err == nil {
			return val
		}
	}
	return os.Getenv(c.Google.APIKeyEnv)
}

// Secret retrieves a named secret, trying the secret store first, then env var.
func (c Config) Secret(name string) string {
	if globalSecretStore != nil {
		if val, err := globalSecretStore.Get(name); err == nil {
			return val
		}
	}
	return os.Getenv(name)
}

// OpenSecretStore initializes the encrypted secret store if enabled.
func OpenSecretStore(cfg SecretsConfig) {
	if !cfg.Enabled {
		return
	}
	store, err := secrets.NewStore(cfg.StorePath)
	if err != nil {
		slog.Warn("secrets: failed to open store, falling back to env vars", "error", err)
		return
	}
	globalSecretStore = store
	slog.Info("secrets: store opened", "path", cfg.StorePath)
}

// ExportSecrets exports all secrets from the secret store into environment
// variables so child processes (e.g. skill scripts) inherit them.
// Existing env vars are not overwritten.
func ExportSecrets() int {
	if globalSecretStore == nil {
		return 0
	}
	keys, err := globalSecretStore.List()
	if err != nil {
		slog.Warn("secrets: failed to list keys for export", "error", err)
		return 0
	}
	exported := 0
	for _, key := range keys {
		if os.Getenv(key) != "" {
			continue // don't overwrite existing env vars
		}
		val, err := globalSecretStore.Get(key)
		if err != nil {
			continue
		}
		os.Setenv(key, val)
		exported++
	}
	return exported
}

// CloseSecretStore closes the secret store if open.
func CloseSecretStore() {
	if globalSecretStore != nil {
		globalSecretStore.Close()
		globalSecretStore = nil
	}
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
