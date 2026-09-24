package bridge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSummarizeSkillChanges(t *testing.T) {
	// -z records: NUL-terminated, unquoted; a rename carries its source next.
	porcelain := strings.Join([]string{
		"?? agents/a/skills/playground/trip/SKILL.md",
		" M agents/a/skills/meal-memo/SKILL.md",
		"?? agents/a/skills/meal-memo/scripts/x.sh",
		" D agents/a/skills/old/SKILL.md",
		"?? agents/a/skills/餐點/SKILL.md",
		"R  agents/a/skills/.archive/gone/SKILL.md", "agents/a/skills/gone/SKILL.md",
		"",
	}, "\x00")
	got := summarizeSkillChanges(porcelain, "agents/a/skills")
	want := []string{"+playground/trip", "+餐點", "-old", "~.archive/gone", "~meal-memo"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %v, want %v", got, want)
	}
}

func TestCommitSkillChanges_CommitsAndNotifies(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	run("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init")

	own := filepath.Join(root, "agents", "a", "skills")
	write := func(rel, body string) {
		p := filepath.Join(own, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Something outside the skills dir must not be swept into the commit.
	os.WriteFile(filepath.Join(root, "unrelated.txt"), []byte("x"), 0o644)

	ft := newFakeTransport()
	b := &Bridge{skillDirs: []string{own}, transport: ft}
	b.SetOwnerChat(42, "testagent")

	// Bookkeeping only: no commit, no notice.
	write("diary/USAGE.jsonl", "{}\n")
	write("diary/scripts/__pycache__/x.cpython-312.pyc", "bytecode")
	b.commitSkillChanges(context.Background())
	if n := run("rev-list", "--count", "HEAD"); n != "1" {
		t.Fatalf("usage log alone produced a commit (count %s)", n)
	}

	write("playground/trip/SKILL.md", "---\nname: trip\ndescription: d\n---\nbody\n")
	b.commitSkillChanges(context.Background())

	if n := run("rev-list", "--count", "HEAD"); n != "2" {
		t.Fatalf("commit count = %s, want 2", n)
	}
	if a := run("log", "-1", "--format=%an %s"); a != "testagent skills(testagent): +playground/trip" {
		t.Fatalf("last commit = %q", a)
	}
	if st := run("status", "--porcelain", "--", "unrelated.txt"); !strings.HasPrefix(st, "??") {
		t.Fatalf("unrelated file was committed: status %q", st)
	}
	if len(ft.notified) != 1 {
		t.Fatalf("notices = %d, want 1: %v", len(ft.notified), ft.notified)
	}
	sha := run("rev-parse", "--short", "HEAD")
	if n := ft.notified[0]; !strings.Contains(n, "testagent changed its own skills: +playground/trip") || !strings.Contains(n, "revert "+sha) {
		t.Fatalf("notice = %q", n)
	}

	// Nothing new: nothing happens.
	b.commitSkillChanges(context.Background())
	if len(ft.notified) != 1 {
		t.Fatalf("a clean tree produced a notice")
	}
}

func TestCommitSkillChanges_NotARepo(t *testing.T) {
	ft := newFakeTransport()
	b := &Bridge{skillDirs: []string{t.TempDir()}, transport: ft}
	b.SetOwnerChat(42, "testagent")
	b.commitSkillChanges(context.Background())
	if len(ft.notified) != 0 {
		t.Fatalf("notice outside a repo: %v", ft.notified)
	}
}
