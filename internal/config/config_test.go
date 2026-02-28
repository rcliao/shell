package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	cfg := Default()

	if cfg.Telegram.TokenEnv != "TELEGRAM_BOT_TOKEN" {
		t.Errorf("expected token_env TELEGRAM_BOT_TOKEN, got %s", cfg.Telegram.TokenEnv)
	}
	if cfg.Claude.Binary != "claude" {
		t.Errorf("expected binary claude, got %s", cfg.Claude.Binary)
	}
	if cfg.Claude.Timeout != 30*time.Minute {
		t.Errorf("expected timeout 30m, got %s", cfg.Claude.Timeout)
	}
	if cfg.Claude.MaxSessions != 4 {
		t.Errorf("expected max_sessions 4, got %d", cfg.Claude.MaxSessions)
	}
}

func TestLoad_Missing(t *testing.T) {
	cfg, err := Load("/nonexistent/path/config.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Claude.Binary != "claude" {
		t.Errorf("expected defaults when file missing")
	}
}

func TestLoad_Valid(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	data := map[string]any{
		"telegram": map[string]any{
			"token_env":     "MY_TOKEN",
			"allowed_users": []int64{123, 456},
		},
		"claude": map[string]any{
			"binary":  "claude-dev",
			"timeout": "10m",
		},
	}
	b, _ := json.Marshal(data)
	os.WriteFile(cfgPath, b, 0644)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Telegram.TokenEnv != "MY_TOKEN" {
		t.Errorf("expected MY_TOKEN, got %s", cfg.Telegram.TokenEnv)
	}
	if cfg.Claude.Binary != "claude-dev" {
		t.Errorf("expected claude-dev, got %s", cfg.Claude.Binary)
	}
	if cfg.Claude.Timeout != 10*time.Minute {
		t.Errorf("expected 10m, got %s", cfg.Claude.Timeout)
	}
}

func TestDefaultReactionMap(t *testing.T) {
	cfg := Default()
	rm := cfg.Telegram.ReactionMap
	if rm == nil {
		t.Fatal("expected non-nil default ReactionMap")
	}

	expected := map[string]string{
		"👍": "go",
		"👎": "stop",
		"🔄": "retry",
		"❌": "cancel",
		"📋": "status",
	}
	for emoji, action := range expected {
		if got := rm[emoji]; got != action {
			t.Errorf("ReactionMap[%s] = %q, want %q", emoji, got, action)
		}
	}
}

func TestReactionAction(t *testing.T) {
	tc := TelegramConfig{
		ReactionMap: map[string]string{"👍": "go", "🔄": "retry"},
	}
	if got := tc.ReactionAction("👍"); got != "go" {
		t.Errorf("expected go, got %s", got)
	}
	if got := tc.ReactionAction("🔄"); got != "retry" {
		t.Errorf("expected retry, got %s", got)
	}
	if got := tc.ReactionAction("🎉"); got != "" {
		t.Errorf("expected empty for unmapped emoji, got %s", got)
	}

	// nil map should return empty
	tc2 := TelegramConfig{}
	if got := tc2.ReactionAction("👍"); got != "" {
		t.Errorf("expected empty for nil map, got %s", got)
	}
}

func TestLoad_CustomReactionMap(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	data := map[string]any{
		"telegram": map[string]any{
			"token_env": "MY_TOKEN",
			"reaction_map": map[string]string{
				"🚀": "deploy",
				"👍": "approve",
			},
		},
	}
	b, _ := json.Marshal(data)
	os.WriteFile(cfgPath, b, 0644)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Telegram.ReactionAction("🚀"); got != "deploy" {
		t.Errorf("expected deploy, got %s", got)
	}
	if got := cfg.Telegram.ReactionAction("👍"); got != "approve" {
		t.Errorf("expected approve, got %s", got)
	}
	// Default entries should be replaced entirely when user provides their own map
	if got := cfg.Telegram.ReactionAction("👎"); got != "" {
		t.Errorf("expected empty for overridden default, got %s", got)
	}
}

func TestTelegramToken(t *testing.T) {
	cfg := Default()
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token-123")
	if got := cfg.TelegramToken(); got != "test-token-123" {
		t.Errorf("expected test-token-123, got %s", got)
	}
}
