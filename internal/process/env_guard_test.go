package process

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every spawn in this package must build its environment through childEnv:
// a future `os.Environ()` at a spawn site would hand children the secrets
// the policy strips. The persistent fake CLI cannot inspect its env, so
// this guards the source instead.
func TestOnlyChildEnvReadsTheProcessEnvironment(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	hits := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue // comments may mention it; code may not
			}
			if strings.Contains(line, "os.Environ()") {
				hits++
				if f != "manager.go" {
					t.Errorf("%s reads os.Environ() directly: %s", f, strings.TrimSpace(line))
				}
			}
		}
	}
	if hits != 1 {
		t.Errorf("os.Environ() appears %d times in the package, want exactly 1 (inside childEnv)", hits)
	}
}
