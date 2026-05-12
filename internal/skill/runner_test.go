package skill

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnsureRunSkillWrapper_WritesExecutable(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureRunSkillWrapper(dir); err != nil {
		t.Fatalf("EnsureRunSkillWrapper: %v", err)
	}
	target := filepath.Join(dir, "run-skill")
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("expected executable bits, got %v", info.Mode())
	}
	data, _ := os.ReadFile(target)
	if !strings.Contains(string(data), "run-skill <skill-name>") {
		t.Error("wrapper content looks wrong")
	}
}

func TestEnsureRunSkillWrapper_Idempotent(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureRunSkillWrapper(dir); err != nil {
		t.Fatalf("first call: %v", err)
	}
	target := filepath.Join(dir, "run-skill")
	info1, _ := os.Stat(target)

	// Second call should be a no-op — same bytes on disk.
	if err := EnsureRunSkillWrapper(dir); err != nil {
		t.Fatalf("second call: %v", err)
	}
	info2, _ := os.Stat(target)
	if !info1.ModTime().Equal(info2.ModTime()) {
		// Acceptable to rewrite if mtime differs — but content must match.
		data1, _ := os.ReadFile(target)
		if string(data1) != runSkillScript {
			t.Error("content drifted after second call")
		}
	}
}

func TestRunSkillWrapper_FlatLayout(t *testing.T) {
	// End-to-end: write a skill with a trivial run.sh, invoke via the
	// wrapper, verify USAGE.jsonl records the invocation.
	skillsDir := t.TempDir()
	if err := EnsureRunSkillWrapper(skillsDir); err != nil {
		t.Fatalf("install wrapper: %v", err)
	}

	skillDir := filepath.Join(skillsDir, "echoer")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: echoer\ndescription: echo test\n---\nbody"), 0o644)
	runSh := filepath.Join(skillDir, "run.sh")
	os.WriteFile(runSh, []byte("#!/bin/bash\necho hi $1\n"), 0o755)

	wrapper := filepath.Join(skillsDir, "run-skill")
	out, err := exec.Command(wrapper, "echoer", "mami").Output()
	if err != nil {
		t.Fatalf("wrapper exec: %v (output: %s)", err, out)
	}
	if strings.TrimSpace(string(out)) != "hi mami" {
		t.Errorf("run.sh output = %q, want \"hi mami\"", out)
	}

	usageLog := filepath.Join(skillDir, "USAGE.jsonl")
	records, err := ReadUsage(usageLog)
	if err != nil {
		t.Fatalf("ReadUsage: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("USAGE.jsonl has %d records, want 1", len(records))
	}
	r := records[0]
	if r.Name != "echoer" {
		t.Errorf("Name = %q", r.Name)
	}
	if r.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", r.ExitCode)
	}
	if r.Version != nil {
		t.Errorf("Version = %v, want nil for flat layout", *r.Version)
	}
	if len(r.Args) != 1 || r.Args[0] != "mami" {
		t.Errorf("Args = %v, want [mami]", r.Args)
	}
}

func TestRunSkillWrapper_VersionedLayout(t *testing.T) {
	skillsDir := t.TempDir()
	if err := EnsureRunSkillWrapper(skillsDir); err != nil {
		t.Fatalf("install wrapper: %v", err)
	}

	skillRoot := filepath.Join(skillsDir, "versioned")
	v1 := filepath.Join(skillRoot, "v1")
	os.MkdirAll(v1, 0o755)
	os.WriteFile(filepath.Join(v1, "SKILL.md"),
		[]byte("---\nname: versioned\ndescription: x\n---\nbody"), 0o644)
	os.WriteFile(filepath.Join(v1, "run.sh"),
		[]byte("#!/bin/bash\nexit 42\n"), 0o755)
	os.WriteFile(filepath.Join(skillRoot, "ACTIVE"), []byte("v1\n"), 0o644)

	wrapper := filepath.Join(skillsDir, "run-skill")
	cmd := exec.Command(wrapper, "versioned")
	err := cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 42 {
		t.Errorf("wrapper did not propagate exit code 42, got err=%v", err)
	}

	usageLog := filepath.Join(v1, "USAGE.jsonl")
	records, err := ReadUsage(usageLog)
	if err != nil {
		t.Fatalf("ReadUsage: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records=%d, want 1", len(records))
	}
	r := records[0]
	if r.Version == nil || *r.Version != "v1" {
		t.Errorf("Version = %v, want v1", r.Version)
	}
	if r.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", r.ExitCode)
	}
}

func TestRunSkillWrapper_RejectsPathTraversal(t *testing.T) {
	skillsDir := t.TempDir()
	if err := EnsureRunSkillWrapper(skillsDir); err != nil {
		t.Fatalf("install wrapper: %v", err)
	}
	wrapper := filepath.Join(skillsDir, "run-skill")

	for _, bad := range []string{"../etc", "a/b", ".hidden", ""} {
		out, err := exec.Command(wrapper, bad).CombinedOutput()
		if err == nil {
			t.Errorf("expected failure for name %q, got success (output=%q)", bad, out)
		}
	}
}

func TestRollup_AggregatesCorrectly(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "USAGE.jsonl")
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -14).Format(time.RFC3339)
	recent := now.AddDate(0, 0, -1).Format(time.RFC3339)

	lines := []string{
		`{"ts":"` + old + `","name":"x","version":"v1","exit_code":0,"duration_ms":100,"args":[]}`,
		`{"ts":"` + recent + `","name":"x","version":"v1","exit_code":0,"duration_ms":200,"args":[]}`,
		`{"ts":"` + recent + `","name":"x","version":"v1","exit_code":1,"duration_ms":50,"args":[]}`,
	}
	os.WriteFile(logPath, []byte(strings.Join(lines, "\n")), 0o644)

	s := &Skill{Name: "x", Version: "v1", Dir: dir}
	stats, err := Rollup(s)
	if err != nil {
		t.Fatalf("Rollup: %v", err)
	}
	if stats.Runs != 3 {
		t.Errorf("Runs = %d, want 3", stats.Runs)
	}
	if stats.Successes != 2 || stats.Failures != 1 {
		t.Errorf("Successes=%d Failures=%d", stats.Successes, stats.Failures)
	}
	if stats.Invocations7 != 2 {
		t.Errorf("Invocations7 = %d, want 2", stats.Invocations7)
	}
	if stats.AvgDuration == 0 {
		t.Error("AvgDuration not set")
	}
}
