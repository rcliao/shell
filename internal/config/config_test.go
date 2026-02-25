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
	if cfg.Claude.Timeout != 5*time.Minute {
		t.Errorf("expected timeout 5m, got %s", cfg.Claude.Timeout)
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

func TestTelegramToken(t *testing.T) {
	cfg := Default()
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token-123")
	if got := cfg.TelegramToken(); got != "test-token-123" {
		t.Errorf("expected test-token-123, got %s", got)
	}
}
