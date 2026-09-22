package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnvNeverOverrides(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("# comment\nexport DOTENV_T_A=\"from file\"\nDOTENV_T_B=set-by-file\nnot a pair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd); os.Unsetenv("DOTENV_T_A"); os.Unsetenv("DOTENV_T_B") })
	t.Setenv("DOTENV_T_B", "from shell")

	loadDotEnv()
	if got := os.Getenv("DOTENV_T_A"); got != "from file" {
		t.Errorf("A = %q, want quotes stripped and export prefix ignored", got)
	}
	if got := os.Getenv("DOTENV_T_B"); got != "from shell" {
		t.Errorf("B = %q — the file must never override the shell", got)
	}
}
