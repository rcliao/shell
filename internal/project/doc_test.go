package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newDocRepo scaffolds a project doc repo in a temp workspace.
func newDocRepo(t *testing.T) (dir, scaffoldRev string) {
	t.Helper()
	dir, err := EnsureDocRepo(t.TempDir(), "synthetic-slug")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := ScaffoldDoc(dir, "Synthetic Project", "keep the options table sorted")
	if err != nil {
		t.Fatal(err)
	}
	return dir, rev
}

func TestScaffoldWriteReadRoundtrip(t *testing.T) {
	dir, scaffoldRev := newDocRepo(t)
	if scaffoldRev == "" {
		t.Fatal("scaffold returned no commit hash")
	}

	content, err := ReadDoc(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Synthetic Project", "## 目標", "## 限制",
		"keep the options table sorted", "## 決定", "## 現況", "## 選項", "## 待決定", "## 更新紀錄"} {
		if !strings.Contains(content, want) {
			t.Errorf("scaffolded doc missing %q", want)
		}
	}

	rev, err := WriteDoc(dir, content+"\n- looked at two options\n", "research pass")
	if err != nil {
		t.Fatal(err)
	}
	if rev == "" || rev == scaffoldRev {
		t.Fatalf("write receipt = %q (scaffold was %q) — must be a NEW commit", rev, scaffoldRev)
	}
	if head, _ := Head(dir); head != rev {
		t.Errorf("returned rev %q is not HEAD %q", rev, head)
	}

	got, _ := ReadDoc(dir)
	if !strings.Contains(got, "looked at two options") {
		t.Error("written content did not read back")
	}

	log, err := DocLog(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log, "agent-write(research pass)") || !strings.Contains(log, "scaffold: initial doc") {
		t.Errorf("log missing expected commits:\n%s", log)
	}
}

// Scaffolding twice must not clobber the doc — create stays idempotent.
func TestScaffoldIsIdempotent(t *testing.T) {
	dir, first := newDocRepo(t)
	again, err := ScaffoldDoc(dir, "A Different Title", "")
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Errorf("re-scaffold minted a new commit %q, want existing %q", again, first)
	}
	content, _ := ReadDoc(dir)
	if strings.Contains(content, "A Different Title") {
		t.Error("re-scaffold overwrote the existing doc")
	}
}

// THE dirty-check contract: a human edit sitting uncommitted on disk is
// committed separately, with the human-edit(local) prefix, BEFORE the agent's
// write — receipts are never forged over human edits.
func TestWriteDocCommitsHumanEditFirst(t *testing.T) {
	dir, _ := newDocRepo(t)

	// The user edits the doc by hand; nothing commits it.
	humanVersion := "# Synthetic Project\n\nthe user fixed a wrong assumption here\n"
	if err := os.WriteFile(filepath.Join(dir, DocFile), []byte(humanVersion), 0o644); err != nil {
		t.Fatal(err)
	}

	rev, err := WriteDoc(dir, "# Synthetic Project\n\nagent revision\n", "revision")
	if err != nil {
		t.Fatal(err)
	}

	log, err := DocLog(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(log, "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 commits (scaffold, human-edit, agent-write), got:\n%s", log)
	}
	if !strings.Contains(lines[0], "agent-write(revision)") {
		t.Errorf("HEAD should be the agent write: %s", lines[0])
	}
	if !strings.HasPrefix(strings.SplitN(lines[1], " ", 2)[1], "human-edit(local):") {
		t.Errorf("the commit before the agent write must be the human edit: %s", lines[1])
	}

	// The human's version is recoverable from history, one commit back.
	prev, err := git(dir, "show", "HEAD~1:"+DocFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prev, "the user fixed a wrong assumption") {
		t.Error("human edit content was not preserved in its own commit")
	}
	if head, _ := Head(dir); head != rev {
		t.Errorf("receipt %q is not HEAD %q", rev, head)
	}
}

// Pathspec isolation: a stray untracked file in the project dir must never be
// swept into a doc commit.
func TestWriteDocNeverCommitsStrayFiles(t *testing.T) {
	dir, _ := newDocRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("temp notes"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := WriteDoc(dir, "# Synthetic Project\n\nupdated\n", ""); err != nil {
		t.Fatal(err)
	}

	tracked, err := git(dir, "ls-files")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tracked, "scratch.txt") {
		t.Errorf("stray file was committed; tracked files:\n%s", tracked)
	}
	if tracked != DocFile {
		t.Errorf("only %s should be tracked, got:\n%s", DocFile, tracked)
	}
}

// A write that changes nothing mints no receipt — the current HEAD is
// returned instead of an empty commit.
func TestWriteDocNoChangeReturnsHead(t *testing.T) {
	dir, _ := newDocRepo(t)
	content, _ := ReadDoc(dir)
	before, _ := Head(dir)
	rev, err := WriteDoc(dir, content, "")
	if err != nil {
		t.Fatal(err)
	}
	if rev != before {
		t.Errorf("no-op write minted commit %q, want existing HEAD %q", rev, before)
	}
}

func TestEnsureDocRepoIsItsOwnRepo(t *testing.T) {
	ws := t.TempDir()
	dir, err := EnsureDocRepo(ws, "synthetic-slug")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(ws, "projects", "synthetic-slug"); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Error("project dir is not a git repo")
	}
	// Idempotent: a second call finds the same repo.
	again, err := EnsureDocRepo(ws, "synthetic-slug")
	if err != nil || again != dir {
		t.Errorf("second EnsureDocRepo = (%q, %v)", again, err)
	}
	// The parent workspace itself must never become a repo.
	if _, err := os.Stat(filepath.Join(ws, ".git")); !os.IsNotExist(err) {
		t.Error("workspace parent must not be a git repo")
	}
}
