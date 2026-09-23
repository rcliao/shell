package planner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The planner starts claude invocations and shells with Bash access; every
// one of them must go through childEnv so the daemon's secret policy holds.
func TestPlannerSpawnsUseChildEnv(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	hits := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue // comments may mention it; code may not
			}
			if strings.Contains(line, "os.Environ()") {
				hits++
			}
			if strings.Contains(line, "cmd.Env = ") && !strings.Contains(line, "p.childEnv()") && !strings.Contains(line, "append(") {
				t.Errorf("%s sets cmd.Env without childEnv: %s", f, strings.TrimSpace(line))
			}
		}
	}
	if hits != 1 {
		t.Errorf("os.Environ() appears %d times, want exactly 1 (the childEnv fallback)", hits)
	}
	p := New(Config{ChildEnv: func() []string { return []string{"ONLY=this"} }})
	if env := p.childEnv(); len(env) != 1 || env[0] != "ONLY=this" {
		t.Errorf("configured ChildEnv must be used verbatim, got %v", env)
	}
}
