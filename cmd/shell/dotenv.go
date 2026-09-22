package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/rcliao/shell/internal/config"
)

// loadDotEnv seeds the process environment from .env files, so a secret an
// operator drops in a file reaches the daemon without a re-login: the daemon
// restarts by exec'ing itself in place (same cwd, same env), so a variable
// that is only in the launching shell's environment never changes after the
// first start. Files are read in order and NEVER override a variable that is
// already set — the shell wins, then ~/.shell/.env (where secrets belong per
// the repo's PII rules), then ./.env for a dev checkout.
func loadDotEnv() {
	for _, path := range []string{
		filepath.Join(config.DefaultConfigDir(), ".env"),
		".env",
	} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
				v = v[1 : len(v)-1]
			}
			if k == "" || os.Getenv(k) != "" {
				continue
			}
			os.Setenv(k, v)
		}
		f.Close()
	}
}
