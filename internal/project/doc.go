// Package project implements the per-project document layer (P2, docs/
// PLAN-PROJECT-WORKSPACE.md): each project owns a small git repository under
// workspace/projects/<slug>/ holding its canonical doc, and every write is a
// commit whose hash is the receipt. Git-over-exec, same pattern as
// internal/worktree.
package project

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// DocFile is the canonical document filename inside a project's repo.
const DocFile = "doc.md"

// Commit identity for agent-made commits. Synthetic on purpose: receipts must
// be attributable to the machinery, never to a person.
const (
	gitAuthorName  = "shell-project"
	gitAuthorEmail = "shell-project@localhost"
)

// git runs one git command in dir, returning trimmed combined output.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

// commitDoc commits ONLY the doc file (pathspec-scoped), with the synthetic
// identity passed per-invocation so the repo works on machines with no global
// git config. Returns the new commit hash.
func commitDoc(dir, msg string) (string, error) {
	if _, err := git(dir, "add", "--", DocFile); err != nil {
		return "", err
	}
	// The pathspec on commit is what keeps a stray staged or untracked file
	// out of the receipt: only doc.md's staged state is committed.
	if _, err := git(dir,
		"-c", "user.name="+gitAuthorName,
		"-c", "user.email="+gitAuthorEmail,
		"-c", "commit.gpgsign=false",
		"commit", "--quiet", "-m", msg, "--", DocFile); err != nil {
		return "", err
	}
	return Head(dir)
}

// EnsureDocRepo creates (or finds) the project's doc directory under
// workspaceDir/projects/<slug>/ and makes it its OWN git repository — one repo
// per project, never a repo over the parent workspace (owner decision, plan
// §Decisions 1). Returns the absolute directory.
func EnsureDocRepo(workspaceDir, slug string) (string, error) {
	if workspaceDir == "" {
		return "", fmt.Errorf("no workspace directory configured")
	}
	if slug == "" {
		return "", fmt.Errorf("slug required")
	}
	dir := filepath.Join(workspaceDir, "projects", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create doc dir: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		if _, err := git(dir, "init", "--quiet"); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// ScaffoldDoc writes the doc template and makes the initial commit, returning
// its hash. A doc that already exists is left untouched — the current HEAD is
// returned so create stays idempotent. Section headings match the plan's
// template (goals/constraints/decisions/status/options/to-decide/log);
// instructions are seeded under the constraints section. The headings carry
// roles the budget and the skill text rely on — see budget.go.
func ScaffoldDoc(dir, title, instructions string) (string, error) {
	path := filepath.Join(dir, DocFile)
	if _, err := os.Stat(path); err == nil {
		return Head(dir)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	b.WriteString("## 目標\n\n")
	b.WriteString("## 限制\n\n")
	if instructions != "" {
		b.WriteString(instructions + "\n\n")
	}
	b.WriteString("## " + DecisionsSection + "\n\n")
	b.WriteString("## 現況\n\n")
	b.WriteString("## 選項\n\n")
	b.WriteString("## 待決定\n\n")
	b.WriteString("## " + LogSection + "\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("write doc: %w", err)
	}
	return commitDoc(dir, "scaffold: initial doc")
}

// WriteDoc replaces the doc's content and commits it, returning the commit
// hash — the receipt.
//
// PRE-WRITE DIRTY CHECK: if the doc has uncommitted changes (a human edited
// the file on disk), those are committed FIRST as their own attributed commit
// with the "human-edit(local):" prefix. Receipts are never forged over human
// edits — the human's version is always in history before the agent's write
// lands on top of it.
//
// An agent write that changes nothing produces no empty commit; the current
// HEAD is returned.
func WriteDoc(dir, content, attribution string) (string, error) {
	status, err := git(dir, "status", "--porcelain", "--", DocFile)
	if err != nil {
		return "", err
	}
	if status != "" {
		if _, err := commitDoc(dir, "human-edit(local): "+DocFile); err != nil {
			return "", err
		}
	}

	if err := os.WriteFile(filepath.Join(dir, DocFile), []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write doc: %w", err)
	}

	// No-op write: nothing to commit, and --allow-empty would mint a receipt
	// for a write that changed nothing.
	status, err = git(dir, "status", "--porcelain", "--", DocFile)
	if err != nil {
		return "", err
	}
	if status == "" {
		return Head(dir)
	}

	msg := "agent-write: " + DocFile
	if attribution != "" {
		msg = "agent-write(" + attribution + "): " + DocFile
	}
	return commitDoc(dir, msg)
}

// ManagedDocDir resolves a project's doc directory and reports whether this
// daemon MANAGES it — i.e. workspaceDir/projects/<slug>/.git exists. A
// project registered with an external doc_path has no managed repo: no
// receipts to mint, no content this layer should read.
func ManagedDocDir(workspaceDir, slug string) (string, bool) {
	if workspaceDir == "" || slug == "" {
		return "", false
	}
	dir := filepath.Join(workspaceDir, "projects", slug)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return "", false
	}
	return dir, true
}

// ReadDoc returns the doc's current content.
func ReadDoc(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, DocFile))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Head returns the current commit hash — the rev a read is served at.
func Head(dir string) (string, error) {
	return git(dir, "rev-parse", "HEAD")
}

// DocLog returns the last n commits as a oneline log, newest first.
func DocLog(dir string, n int) (string, error) {
	if n <= 0 {
		n = 10
	}
	return git(dir, "log", "--oneline", "-n", strconv.Itoa(n))
}
