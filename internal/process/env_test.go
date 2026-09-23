package process

import (
	"strings"
	"testing"
)

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			m[k] = v
		}
	}
	return m
}

// The child env policy: managed names and every *_BOT_TOKEN are gone,
// passthrough names arrive with the store's value even when the daemon's
// environment carries a different one, overrides still apply.
func TestChildEnvStripsSecretsAndPassesAllowlist(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "bot-secret")
	t.Setenv("PEER_BOT_TOKEN", "peer-secret")
	t.Setenv("NOTION_TOKEN", "notion-secret")
	t.Setenv("GEMINI_API_KEY", "stale-env-value")
	t.Setenv("HARMLESS", "keep")
	t.Setenv("CLAUDECODE", "1")

	m := NewManager(ManagerConfig{
		Env:      map[string]string{"SHELL_AGENT_HOME": "/tmp/agent"},
		StripEnv: []string{"NOTION_TOKEN", "TYPESAFE_API_KEY"},
		PassEnv:  map[string]string{"GEMINI_API_KEY": "from-store"},
	})
	got := envMap(m.childEnv())

	for _, absent := range []string{"TELEGRAM_BOT_TOKEN", "PEER_BOT_TOKEN", "NOTION_TOKEN", "CLAUDECODE"} {
		if v, ok := got[absent]; ok {
			t.Errorf("%s reached the child env (%q)", absent, v)
		}
	}
	if got["GEMINI_API_KEY"] != "from-store" {
		t.Errorf("passthrough must carry the store value, got %q", got["GEMINI_API_KEY"])
	}
	if got["HARMLESS"] != "keep" || got["SHELL_AGENT_HOME"] != "/tmp/agent" {
		t.Errorf("ordinary vars and overrides must survive: %v", got)
	}
	// No duplicates for a passthrough name.
	n := 0
	for _, e := range m.childEnv() {
		if strings.HasPrefix(e, "GEMINI_API_KEY=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("GEMINI_API_KEY appears %d times, want 1", n)
	}
}
