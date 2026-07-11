package main

import (
	"path/filepath"
	"testing"
)

func TestDaemonLogPath(t *testing.T) {
	// explicit override wins
	if got := daemonLogPath("/custom/x.log", "/a/b/config.json"); got != "/custom/x.log" {
		t.Errorf("override: got %q", got)
	}
	// else next to the agent config
	if got := daemonLogPath("", "/Users/x/.shell/agents/umbreonmini/config.json"); got != "/Users/x/.shell/agents/umbreonmini/daemon.log" {
		t.Errorf("derived: got %q", got)
	}
	// empty config path → default dir
	got := daemonLogPath("", "")
	if filepath.Base(got) != "daemon.log" {
		t.Errorf("default: got %q", got)
	}
}
