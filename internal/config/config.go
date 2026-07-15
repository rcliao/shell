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
	Tunnel    TunnelConfig    `json:"tunnel"`
	PM        PMConfig        `json:"pm"`
	Skills    SkillsConfig    `json:"skills"`
	Agent     AgentIdentity   `json:"agent"`
	Agents    AgentsConfig    `json:"agents"`
	Notion    NotionConfig    `json:"notion"`
}

// NotionConfig wires the official Notion MCP server (@notionhq/notion-mcp-server)
// into the agent's generated mcp.json, giving the conversational agent a real,
// headless-safe write tool for the food-log / docs (vs. the claude.ai connector
// which only exists in interactive sessions). The integration token is resolved
// via the secret store (or env), so it never lands in config.json.
type NotionConfig struct {
	Enabled     bool     `json:"enabled"`
	TokenSecret string   `json:"token_secret"` // secret/env name holding the Notion integration token (default "NOTION_TOKEN")
	Command     string   `json:"command"`      // launcher, default "npx"
	Args        []string `json:"args"`         // default ["-y", "@notionhq/notion-mcp-server"]
}

// AgentIdentity configures a bot's identity for multi-agent group chats.
type AgentIdentity struct {
	Name                 string   `json:"name"`                  // display name (e.g. "pikamini")
	Aliases              []string `json:"aliases"`               // name variants users may use to address this agent (e.g. "pika")
	BotUsername          string   `json:"bot_username"`          // Telegram bot username without @
	BroadcastProbability float64  `json:"broadcast_probability"` // 0.0-1.0, chance to respond when not @mentioned in groups (legacy mode)
	PeerBots             []string `json:"peer_bots"`             // other bot usernames (to detect "not for me")
	SystemPrompt         string   `json:"system_prompt"`         // personality/identity prompt prepended to all messages
	GroupMode            string   `json:"group_mode"`            // "autonomous" = agent decides via [noop], "" = legacy probability
	GroupDomain          string   `json:"group_domain"`          // "practical" | "companionship" — role-based routing for general group messages (empty = both may answer)
	TranscriptPath       string   `json:"transcript_path"`       // path to shared transcript DB (default: ~/.shell/shared/transcript.db)
	TranscriptBudget     int      `json:"transcript_budget"`     // token budget for shared transcript injection (default: 2000)
	Skills               []string `json:"skills"`                // declared capabilities for task delegation (e.g. "code-review", "research")
}

// PeerAgent describes a peer agent for multi-agent discovery.
type PeerAgent struct {
	Name        string   `json:"name"`
	Aliases     []string `json:"aliases"` // name variants for this peer (e.g. "pika")
	BotUsername string   `json:"bot_username"`
	Skills      []string `json:"skills"`
}

// AgentsConfig controls multi-agent manifests.
type AgentsConfig struct {
	Dir string `json:"dir"` // directory containing agent manifests (default: ~/.shell/agents)
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

// ModelRouting allows different Claude models per task type for cost optimization.
// Empty strings fall back to ClaudeConfig.Model.
type ModelRouting struct {
	Conversation   string `json:"conversation"`    // user-facing chat
	Heartbeat      string `json:"heartbeat"`       // periodic maintenance (sonnet recommended)
	HeartbeatDeep  string `json:"heartbeat_deep"`  // deep reflection heartbeat (opus recommended)
	Compaction     string `json:"compaction"`      // session compaction
	PlannerExecute string `json:"planner_execute"` // code changes with tools
	PlannerReview  string `json:"planner_review"`  // text-only review verdict
	// TopicClassifier (cycle 66): if non-empty, enables Haiku-based topic
	// classification per turn. Empty = disabled (keyword fast-path only).
	TopicClassifier string `json:"topic_classifier"`
	// ConversationEffort sets --effort for conversation turns, bound at each
	// generation's fresh spawn (persistent procs ignore per-turn effort).
	// "low" cuts silent pre-answer thinking on simple turns (V2-H33: a
	// 4-word arithmetic answer spent 40s thinking). Empty = CLI default.
	ConversationEffort string `json:"conversation_effort"`
}

type ClaudeConfig struct {
	Binary             string            `json:"binary"`
	Model              string            `json:"model"`
	ModelRouting       *ModelRouting     `json:"model_routing"` // per-task model overrides (nil = use default)
	Timeout            time.Duration     `json:"timeout"`
	MaxSessions        int               `json:"max_sessions"`
	WorkDir            string            `json:"work_dir"`
	AllowedTools       []string          `json:"allowed_tools"`
	ExtraArgs          []string          `json:"extra_args"`
	Env                map[string]string `json:"env"`                  // extra environment variables for Claude CLI subprocess
	PlaygroundDir      string            `json:"playground_dir"`       // writable sandbox dir, auto-approved for Write/Edit/Bash
	SettingSources     []string          `json:"setting_sources"`      // e.g. ["user", "project"] for --setting-sources
	MaxSessionTokens   int               `json:"max_session_tokens"`   // in-place /compact when total input tokens exceed this (0 = disabled)
	RotateMaxTokens    int               `json:"rotate_max_tokens"`    // full session ROTATION (fresh system prompt: skills+identity+pinned reloaded) once total input tokens exceed this (0 = disabled). Lower = fresher/less drift, at higher cache cost.
	RotateMaxContextTokens int           `json:"rotate_max_context_tokens"` // rotate when TOTAL resumed context (input+cache-creation+cache-read) exceeds this — latency guard so long sessions don't bloat to ~1M tokens and crawl (0 = disabled)
	WriteVerifyEnforce bool              `json:"write_verify_enforce"` // when true, a caught write-claim confabulation triggers a bounded correction turn before delivery
	MediaGateEnforce   bool              `json:"media_gate_enforce"`   // when true, image/video artifacts are dropped on user turns that didn't ask for media (heartbeat turns always drop media regardless)
	TopicKeywordOnly   bool              `json:"topic_keyword_only"`   // when true, topic classification runs cache→keyword→sticky only, no per-turn LLM call (cycle 148: the LLM tier regressed every focus metric while adding ~8.5s latency)
	PermissionMode     string            `json:"permission_mode"`      // --permission-mode for the Claude CLI subprocess (default "bypassPermissions"). "auto" adds a safety classifier but may fall back to prompting (which hangs a headless turn).
}

// ResolveEffort returns the reasoning effort for a task type ("" = CLI default).
func (c ClaudeConfig) ResolveEffort(taskType string) string {
	if c.ModelRouting != nil && taskType == "conversation" {
		return c.ModelRouting.ConversationEffort
	}
	return ""
}

// ResolveModel returns the model for a given task type, falling back to
// the default Model, then empty string (CLI default).
func (c ClaudeConfig) ResolveModel(taskType string) string {
	if c.ModelRouting != nil {
		var m string
		switch taskType {
		case "conversation":
			m = c.ModelRouting.Conversation
		case "heartbeat":
			m = c.ModelRouting.Heartbeat
		case "heartbeat_deep":
			m = c.ModelRouting.HeartbeatDeep
			if m == "" {
				m = c.ModelRouting.Heartbeat // fall back to regular heartbeat model
			}
		case "compaction":
			m = c.ModelRouting.Compaction
		case "topic_classifier":
			m = c.ModelRouting.TopicClassifier
		case "planner_execute":
			m = c.ModelRouting.PlannerExecute
		case "planner_review":
			m = c.ModelRouting.PlannerReview
		}
		if m != "" {
			return m
		}
	}
	return c.Model
}

// Validate returns human-readable warnings about model/session settings that are
// silently wrong or self-defeating. Non-fatal by design — surfaced at startup so
// a mis-tuned config is visible instead of failing quietly. See
// docs/MODEL-SESSION-CONFIG.md (S4, S5).
func (c ClaudeConfig) Validate() []string {
	var warnings []string

	// S5 — empty resolved model → no --model flag → the CLI silently picks its
	// own default. Conversation always runs, so an empty conversation model is
	// the dangerous case.
	if c.ResolveModel("conversation") == "" {
		warnings = append(warnings, "claude.model (conversation) resolves to empty — the CLI will silently pick its own default model; set claude.model or claude.model_routing.conversation")
	}

	// S4 — a rotation cap below the compaction cap means rotation always fires
	// first, so in-place compaction never runs. Not necessarily wrong (rotation
	// is often preferable), but the operator should know compaction is dead.
	if c.RotateMaxTokens > 0 && c.MaxSessionTokens > 0 && c.RotateMaxTokens < c.MaxSessionTokens {
		warnings = append(warnings, fmt.Sprintf("rotate_max_tokens (%d) < max_session_tokens (%d): session rotation always preempts in-place compaction, so compaction never runs", c.RotateMaxTokens, c.MaxSessionTokens))
	}

	return warnings
}

type StoreConfig struct {
	DBPath string `json:"db_path"`
	// MessageRetentionDays bounds the periodic messages-table prune. The old
	// hardcoded 7-day prune silently destroyed conversation history (V2-H25:
	// June 2026 is unrecoverable). 0/absent = default 365. Negative = never
	// prune.
	MessageRetentionDays int `json:"message_retention_days"`
}

// MessageRetention returns the prune age, defaulting to 365 days.
// A negative configured value disables pruning entirely (returns 0, false).
func (s StoreConfig) MessageRetention() (time.Duration, bool) {
	if s.MessageRetentionDays < 0 {
		return 0, false
	}
	days := s.MessageRetentionDays
	if days == 0 {
		days = 365
	}
	return time.Duration(days) * 24 * time.Hour, true
}

type DaemonConfig struct {
	PIDFile string `json:"pid_file"`
	LogFile string `json:"log_file"` // path for daemon logs (default: <agent config dir>/daemon.log). Empty = use default.
	// Outbound dedup (V2-H3): a proactive send (scheduler notify, relay, a2a,
	// prompt-schedule result) whose text matches a send to the same chat within
	// the window is suppressed — deterministic guard against duplicate
	// reminders. On by default; outbound_dedup_disabled is the kill-switch.
	OutboundDedupDisabled   bool `json:"outbound_dedup_disabled"`
	CoalesceDisabled        bool `json:"coalesce_disabled"` // kill switch: V2-H44 queued-message coalescing
	OutboundDedupWindowMins int  `json:"outbound_dedup_window_mins"` // default 60
}

// OutboundDedupWindow returns the dedup window, defaulting to 60 minutes.
func (d DaemonConfig) OutboundDedupWindow() time.Duration {
	if d.OutboundDedupWindowMins > 0 {
		return time.Duration(d.OutboundDedupWindowMins) * time.Minute
	}
	return 60 * time.Minute
}

type Profile struct {
	AgentNS          string   `json:"agent_ns"`          // agent namespace (e.g. "agent:pikamini")
	SystemNamespaces []string `json:"system_namespaces"` // deprecated: use agent_ns + pinned memories
	SystemBudget     int      `json:"system_budget"`
	GlobalNamespaces []string `json:"global_namespaces"` // deprecated: use agent_ns + tag filtering
	GlobalBudget     int      `json:"global_budget"`
	Budget           int      `json:"budget"`
	ExchangeTTL      string   `json:"exchange_ttl"`       // "7d", "30d"
	ExchangeMaxUser  int      `json:"exchange_max_user"`  // 0 = default 200
	ExchangeMaxReply int      `json:"exchange_max_reply"` // 0 = default 300
	MemoryDirectives bool     `json:"memory_directives"`
	DirectiveNS      string   `json:"directive_ns"` // deprecated: use agent_ns + "learning" tag
}

type MemoryConfig struct {
	DBPath           string             `json:"db_path"`
	Enabled          bool               `json:"enabled"`
	Budget           int                `json:"budget"`            // token budget for context injection
	GlobalNamespaces []string           `json:"global_namespaces"` // namespace patterns for background context
	GlobalBudget     int                `json:"global_budget"`     // token budget for global context (default 500)
	SystemNamespaces []string           `json:"system_namespaces"` // always-on via --append-system-prompt (no search)
	SystemBudget     int                `json:"system_budget"`     // token cap for system prompt (default 3000)
	Profiles         map[string]Profile `json:"profiles"`          // name → profile
	ChatProfiles     map[string]string  `json:"chat_profiles"`     // chatID string → profile name
	GhostEnv         map[string]string  `json:"ghost_env"`         // extra env vars passed to ghost MCP server and hooks
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
	Enabled               bool   `json:"enabled"`
	Timezone              string `json:"timezone"`                // default: "UTC"
	QuietHourStart        int    `json:"quiet_hour_start"`        // hour (0-23) when quiet hours begin, default: 22
	QuietHourEnd          int    `json:"quiet_hour_end"`          // hour (0-23) when quiet hours end, default: 7
	HeartbeatInterval     string `json:"heartbeat_interval"`      // active heartbeat interval (default: "1h")
	HeartbeatIdleInterval string `json:"heartbeat_idle_interval"` // interval after noop heartbeat (default: "2h")
	DeepReflectInterval   int    `json:"deep_reflect_interval"`   // every Nth heartbeat uses deep model for reflection (default: 6)
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
			Binary:           "claude",
			Model:            "",
			Timeout:          30 * time.Minute,
			MaxSessions:      4,
			MaxSessionTokens: 200000,
			WorkDir:          "",
			AllowedTools:     []string{},
			ExtraArgs:        []string{},
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
			Enabled:               false,
			Timezone:              "UTC",
			QuietHourStart:        22,
			QuietHourEnd:          7,
			HeartbeatInterval:     "1h",
			HeartbeatIdleInterval: "2h",
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

	for _, w := range cfg.Claude.Validate() {
		slog.Warn("config: "+w, "path", absPath)
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
