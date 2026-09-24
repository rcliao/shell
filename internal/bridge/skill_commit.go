package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Self-authored skills (design: docs/DESIGN-CONTEXT-AND-SKILLS.md). An agent
// changes its own skills directory freely — drafts in playground/, graduated
// skills, tier changes, retirements. The harness makes every change visible
// and reversible instead of asking permission: after each heartbeat, any
// change is committed to the agent-layer git repo (~/.shell) with the agent
// as author, and the owner gets one line with the revert command.

var skillCommitMu sync.Mutex

// SetOwnerChat sets where skill-change notices go, and the name used as
// commit author and in notices.
func (b *Bridge) SetOwnerChat(chatID int64, agentName string) {
	b.ownerChatID = chatID
	b.agentName = agentName
}

// ownSkillsDir is the agent's own skills directory: always the last entry
// of the skill dirs the daemon configures.
func (b *Bridge) ownSkillsDir() string {
	if len(b.skillDirs) == 0 {
		return ""
	}
	return b.skillDirs[len(b.skillDirs)-1]
}

// commitSkillChanges commits and announces any change to the agent's own
// skills directory. Best effort: a failure is logged, never surfaced to a
// family chat, and retried naturally after the next heartbeat.
func (b *Bridge) commitSkillChanges(ctx context.Context) {
	dir := b.ownSkillsDir()
	if dir == "" {
		return
	}
	skillCommitMu.Lock()
	defer skillCommitMu.Unlock()

	sha, summary, err := commitDir(ctx, dir, b.agentName)
	if err != nil {
		slog.Warn("skills: commit of own skill changes failed", "dir", dir, "error", err)
		return
	}
	if sha == "" {
		return // nothing changed
	}
	slog.Info("skills: committed own skill changes", "agent", b.agentName, "sha", sha, "changes", summary)

	// The catalog is part of the system prompt: reload so a graduated or
	// re-tiered skill takes effect (sessions rotate onto the new prompt).
	if _, err := b.ReloadSkills(); err != nil {
		slog.Warn("skills: reload after commit failed", "error", err)
	}
	if b.ownerChatID != 0 && b.transport != nil {
		b.transport.Notify(b.ownerChatID, 0, fmt.Sprintf("🛠 %s changed its own skills: %s\nRevert: git -C %s revert %s",
			b.agentName, strings.Join(summary, " · "), tildePath(repoRoot(ctx, dir)), sha))
	}
}

// commitDir commits every change under dir in the git repo that contains it.
// Returns the short sha and a per-skill summary, or "" when nothing changed.
// A dir outside any repo is not an error: there is nothing to commit to.
func commitDir(ctx context.Context, dir, author string) (string, []string, error) {
	root := repoRoot(ctx, dir)
	if root == "" {
		return "", nil, nil
	}
	// Ask git for dir relative to the top: it resolves symlinks (/tmp vs
	// /private/tmp, a symlinked ~/.shell) the way filepath.Rel cannot.
	prefix, err := git(ctx, dir, "rev-parse", "--show-prefix")
	if err != nil {
		return "", nil, err
	}
	rel := strings.TrimSuffix(strings.TrimSpace(prefix), "/")
	if rel == "" {
		rel = "."
	}
	spec := skillPathspec(rel)
	out, err := git(ctx, root, append([]string{"-c", "core.quotePath=false", "status", "--porcelain", "-z", "--untracked-files=all", "--"}, spec...)...)
	if err != nil {
		return "", nil, err
	}
	summary := summarizeSkillChanges(out, rel)
	if len(summary) == 0 {
		return "", nil, nil
	}
	if _, err := git(ctx, root, append([]string{"add", "-A", "--"}, spec...)...); err != nil {
		return "", nil, err
	}
	if author == "" {
		author = "agent"
	}
	msg := fmt.Sprintf("skills(%s): %s", author, strings.Join(summary, ", "))
	// Pathspec on commit = --only: files staged elsewhere in the repo (the
	// owner's own work in ~/.shell) are never swept into the agent's commit.
	if _, err := git(ctx, root, append([]string{"-c", "user.name=" + author, "-c", "user.email=" + author + "@shell.local",
		"commit", "-q", "-m", msg, "--"}, spec...)...); err != nil {
		return "", nil, err
	}
	sha, err := git(ctx, root, "rev-parse", "--short", "HEAD")
	return strings.TrimSpace(sha), summary, err
}

// skillNoise is bookkeeping a skill's own scripts write next to it —
// usage logs, bytecode, caches. Never authoring; never committed.
var skillNoise = []string{"**/USAGE.jsonl", "**/__pycache__/**", "**/*.pyc", "**/.DS_Store", "**/node_modules/**", "**/.cache/**", "run-skill*/**", "run-skill*"}

// skillPathspec is the agent's skills dir minus the noise.
func skillPathspec(rel string) []string {
	spec := []string{rel}
	for _, n := range skillNoise {
		spec = append(spec, ":(exclude,glob)"+filepath.ToSlash(filepath.Join(rel, n)))
	}
	return spec
}

// summarizeSkillChanges turns `git status --porcelain -z` records under rel
// into one entry per skill: "+name" added, "-name" removed, "~name" changed.
// playground/ and .archive/ entries are named with their skill.
func summarizeSkillChanges(porcelainZ, rel string) []string {
	kinds := map[string]string{}
	recs := strings.Split(porcelainZ, "\x00")
	for i := 0; i < len(recs); i++ {
		rec := recs[i]
		if len(rec) < 4 {
			continue
		}
		code, path := rec[:2], rec[3:]
		if code[0] == 'R' || code[0] == 'C' {
			i++ // -z puts a rename's source path in the next record
		}
		p := strings.TrimPrefix(filepath.ToSlash(path), filepath.ToSlash(rel)+"/")
		if rel == "." {
			p = filepath.ToSlash(path)
		}
		parts := strings.Split(p, "/")
		name := parts[0]
		if strings.HasPrefix(name, ".") && name != ".archive" {
			continue
		}
		if (name == "playground" || name == ".archive") && len(parts) > 1 {
			name += "/" + parts[1]
		}
		kind := "~"
		switch {
		case strings.Contains(code, "?") || strings.Contains(code, "A"):
			kind = "+"
		case strings.Contains(code, "D"):
			kind = "-"
		}
		if prev, ok := kinds[name]; ok && prev != kind {
			kind = "~"
		}
		kinds[name] = kind
	}
	out := make([]string, 0, len(kinds))
	for name, k := range kinds {
		out = append(out, k+name)
	}
	sort.Strings(out)
	return out
}

func repoRoot(ctx context.Context, dir string) string {
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	out, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func tildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}
