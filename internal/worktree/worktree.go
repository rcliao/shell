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
	"time"
)

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
