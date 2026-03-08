package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initGitRepo creates a git repo with one commit at dir.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	// Create an initial commit so HEAD is valid.
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "add", "."},
		{"git", "commit", "-m", "init"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
}

func TestListRepos(t *testing.T) {
	tmp := t.TempDir()

	// Create two nested repos.
	initGitRepo(t, filepath.Join(tmp, "alpha"))
	initGitRepo(t, filepath.Join(tmp, "beta"))

	repos := ListRepos(tmp)
	if len(repos) != 2 {
		t.Fatalf("expected 2 repos, got %d: %v", len(repos), repos)
	}
	if _, ok := repos["alpha"]; !ok {
		t.Error("missing repo 'alpha'")
	}
	if _, ok := repos["beta"]; !ok {
		t.Error("missing repo 'beta'")
	}
}

func TestResolveRepoDir_NestedMatch(t *testing.T) {
	tmp := t.TempDir()

	// Parent is also a git repo (mono-repo), with nested repos.
	initGitRepo(t, tmp)
	initGitRepo(t, filepath.Join(tmp, "shell"))
	initGitRepo(t, filepath.Join(tmp, "ghost"))

	// Intent mentioning "shell" should pick the nested repo, not the parent.
	resolved, err := ResolveRepoDir(tmp, "add sticker support to shell")
	if err != nil {
		t.Fatal(err)
	}

	expected := filepath.Join(tmp, "shell")
	if filepath.Clean(resolved) != filepath.Clean(expected) {
		t.Errorf("expected %s, got %s", expected, resolved)
	}
}

func TestResolveRepoDir_FallbackToParent(t *testing.T) {
	tmp := t.TempDir()

	// Parent is a git repo with nested repos, but intent doesn't match any.
	initGitRepo(t, tmp)
	initGitRepo(t, filepath.Join(tmp, "alpha"))
	initGitRepo(t, filepath.Join(tmp, "beta"))

	resolved, err := ResolveRepoDir(tmp, "do something unrelated")
	if err != nil {
		t.Fatal(err)
	}

	// Should fall back to the parent repo.
	if filepath.Clean(resolved) != filepath.Clean(tmp) {
		t.Errorf("expected parent %s, got %s", tmp, resolved)
	}
}
