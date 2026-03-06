// Package worktree manages git worktrees for isolated plan execution.
// Each plan gets its own worktree branch so that changes don't affect
// the main working directory until the plan completes successfully.
package worktree

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ListRepos returns a map of repo name → absolute path for git repositories
// found under workDir (up to 3 levels deep).
func ListRepos(workDir string) map[string]string {
	repos := findGitRepos(workDir, 3)
	result := make(map[string]string, len(repos))
	for _, r := range repos {
		result[filepath.Base(r)] = r
	}
	return result
}

// ResolveRepoDir determines which git repository to use for worktree creation.
// When workDir contains nested repos, it checks those first so that an intent
// matching a nested repo name is preferred over the parent. Falls back to workDir
// itself if it is a git repo and no nested match is found.
func ResolveRepoDir(workDir, intent string) (string, error) {
	// Always check nested repos first so mono-repo parents don't shadow children.
	repos := findGitRepos(workDir, 3)

	if len(repos) > 0 {
		if len(repos) == 1 {
			return repos[0], nil
		}

		// Try to match intent against repo directory names
		intentLower := strings.ToLower(intent)
		for _, repo := range repos {
			name := strings.ToLower(filepath.Base(repo))
			if strings.Contains(intentLower, name) {
				return repo, nil
			}
		}
	}

	// Fall back to workDir if it is itself a git repo.
	if isGitRepo(workDir) {
		return workDir, nil
	}

	if len(repos) == 0 {
		return "", fmt.Errorf("no git repository found under %s", workDir)
	}

	// Multiple repos found but no intent match and parent isn't a repo.
	names := make([]string, len(repos))
	for i, r := range repos {
		names[i] = filepath.Base(r)
	}
	return "", fmt.Errorf("multiple git repositories found (%s), could not determine target from intent", strings.Join(names, ", "))
}

// isGitRepo checks that dir is the root of a git repository with at least
// one commit. Just having a .git directory is not enough — HEAD must resolve
// to a valid object, otherwise git worktree add will fail.
func isGitRepo(dir string) bool {
	// Check this directory is the toplevel of a repo (not merely inside one)
	toplevelCmd := exec.Command("git", "rev-parse", "--show-toplevel")
	toplevelCmd.Dir = dir
	out, err := toplevelCmd.Output()
	if err != nil {
		return false
	}
	toplevel := strings.TrimSpace(string(out))
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	// Resolve symlinks so that e.g. /var → /private/var on macOS doesn't
	// cause a false mismatch with git's resolved toplevel path.
	if resolved, err := filepath.EvalSymlinks(absDir); err == nil {
		absDir = resolved
	}
	if filepath.Clean(toplevel) != filepath.Clean(absDir) {
		return false
	}

	// Verify HEAD points to a valid commit
	headCmd := exec.Command("git", "rev-parse", "HEAD")
	headCmd.Dir = dir
	return headCmd.Run() == nil
}

// findGitRepos walks dir up to maxDepth levels looking for directories containing .git.
func findGitRepos(dir string, maxDepth int) []string {
	if maxDepth <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var repos []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		child := filepath.Join(dir, e.Name())
		if isGitRepo(child) {
			repos = append(repos, child)
		} else {
			repos = append(repos, findGitRepos(child, maxDepth-1)...)
		}
	}
	return repos
}

// Create creates a new git worktree branched from HEAD.
// baseDir is the directory under which worktrees are stored.
// repoDir is the main repository working directory.
// Returns the worktree path and branch name.
func Create(repoDir, baseDir string, chatID int64) (worktreePath, branchName string, err error) {
	ts := time.Now().Unix()
	branchName = fmt.Sprintf("plan-%d-%d", chatID, ts)

	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return "", "", fmt.Errorf("create worktree base dir: %w", err)
	}

	worktreePath = filepath.Join(baseDir, branchName)

	cmd := exec.Command("git", "worktree", "add", "-b", branchName, worktreePath)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("git worktree add: %w\n%s", err, string(out))
	}

	slog.Info("created worktree", "path", worktreePath, "branch", branchName)
	return worktreePath, branchName, nil
}

// Remove removes a git worktree and prunes stale entries.
func Remove(repoDir, worktreePath string) error {
	cmd := exec.Command("git", "worktree", "remove", worktreePath, "--force")
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree remove: %w\n%s", err, string(out))
	}

	// Prune any stale worktree metadata
	prune := exec.Command("git", "worktree", "prune")
	prune.Dir = repoDir
	prune.CombinedOutput() // best effort

	slog.Info("removed worktree", "path", worktreePath)
	return nil
}

// MergeAndCleanup merges the worktree branch into the current branch
// in the main repo, then removes the worktree and deletes the branch.
func MergeAndCleanup(repoDir, worktreePath, branchName string) error {
	// Remove worktree first (can't delete branch while it's checked out)
	if err := Remove(repoDir, worktreePath); err != nil {
		slog.Warn("worktree remove failed during merge cleanup", "error", err)
		// Continue anyway — try to merge
	}

	// Merge the branch into the current branch
	cmd := exec.Command("git", "merge", branchName, "--no-edit")
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git merge %s: %w\n%s", branchName, err, string(out))
	}

	slog.Info("merged worktree branch", "branch", branchName)

	// Delete the branch (best effort)
	del := exec.Command("git", "branch", "-d", branchName)
	del.Dir = repoDir
	if delOut, err := del.CombinedOutput(); err != nil {
		slog.Warn("failed to delete worktree branch", "branch", branchName, "error", err, "output", string(delOut))
	}

	return nil
}

// Cleanup removes the worktree without merging. The branch is kept
// so the user can inspect partial work if needed.
func Cleanup(repoDir, worktreePath, branchName string) {
	if err := Remove(repoDir, worktreePath); err != nil {
		slog.Warn("worktree cleanup failed", "path", worktreePath, "error", err)
	}
	slog.Info("worktree cleaned up (branch kept)", "branch", branchName)
}
