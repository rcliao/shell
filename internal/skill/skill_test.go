package skill

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_WithScriptsDir(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "hello")
	scriptsDir := filepath.Join(skillDir, "scripts")
	os.MkdirAll(scriptsDir, 0755)

	// Write SKILL.md with space-delimited allowed-tools (per spec)
	md := `---
name: hello
description: Simple greeting skill for testing
allowed-tools: Bash(hello:*) Read
---

# Hello Skill

Prints a greeting.
`
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0644)

	// Write executable script in scripts/
	os.WriteFile(filepath.Join(scriptsDir, "hello"), []byte("#!/bin/sh\necho hi"), 0755)

	s, err := Load(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if s.Name != "hello" {
		t.Errorf("Name = %q, want %q", s.Name, "hello")
	}
	if s.Description != "Simple greeting skill for testing" {
		t.Errorf("Description = %q", s.Description)
	}
	if len(s.AllowedTools) != 2 || s.AllowedTools[0] != "Bash(hello:*)" || s.AllowedTools[1] != "Read" {
		t.Errorf("AllowedTools = %v, want [Bash(hello:*) Read]", s.AllowedTools)
	}
	if s.ScriptsDir != scriptsDir {
		t.Errorf("ScriptsDir = %q, want %q", s.ScriptsDir, scriptsDir)
	}
	if s.Dir != skillDir {
		t.Errorf("Dir = %q, want %q", s.Dir, skillDir)
	}
	if s.Body == "" {
		t.Error("Body should not be empty")
	}
}

func TestLoad_NoScriptsDir(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "check")
	os.MkdirAll(skillDir, 0755)

	md := `---
name: check
description: Run checks
---

Body here.
`
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0644)

	s, err := Load(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if s.ScriptsDir != "" {
		t.Errorf("ScriptsDir should be empty, got %q", s.ScriptsDir)
	}
	if len(s.AllowedTools) != 0 {
		t.Errorf("AllowedTools should be empty, got %v", s.AllowedTools)
	}
}

func TestLoad_MissingName(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "bad")
	os.MkdirAll(skillDir, 0755)

	md := `---
description: Missing name
---

Body.
`
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0644)

	_, err := Load(filepath.Join(skillDir, "SKILL.md"))
	if err == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestLoad_NoFrontmatter(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "bad")
	os.MkdirAll(skillDir, 0755)

	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("no frontmatter"), 0644)

	_, err := Load(filepath.Join(skillDir, "SKILL.md"))
	if err == nil {
		t.Fatal("expected error for missing frontmatter")
	}
}

func TestLoad_CommaDelimitedTools(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "multi")
	os.MkdirAll(skillDir, 0755)

	md := `---
name: multi
description: Multiple tools (comma compat)
allowed-tools: Bash(foo:*), Read, Write
---

Body.
`
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0644)

	s, err := Load(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(s.AllowedTools) != 3 {
		t.Fatalf("AllowedTools = %v, want 3 entries", s.AllowedTools)
	}
	if s.AllowedTools[0] != "Bash(foo:*)" || s.AllowedTools[1] != "Read" || s.AllowedTools[2] != "Write" {
		t.Errorf("AllowedTools = %v", s.AllowedTools)
	}
}

func TestLoad_SpaceDelimitedTools(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "spec")
	os.MkdirAll(skillDir, 0755)

	md := `---
name: spec
description: Space-delimited per spec
allowed-tools: Bash(git:*) Bash(jq:*) Read
---

Body.
`
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0644)

	s, err := Load(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(s.AllowedTools) != 3 {
		t.Fatalf("AllowedTools = %v, want 3 entries", s.AllowedTools)
	}
	if s.AllowedTools[0] != "Bash(git:*)" || s.AllowedTools[1] != "Bash(jq:*)" || s.AllowedTools[2] != "Read" {
		t.Errorf("AllowedTools = %v", s.AllowedTools)
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()

	// Create two valid skills
	for _, name := range []string{"alpha", "beta"} {
		skillDir := filepath.Join(dir, name)
		os.MkdirAll(skillDir, 0755)
		md := "---\nname: " + name + "\ndescription: Skill " + name + "\n---\n\nBody."
		os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0644)
	}

	// Create an invalid skill (missing description)
	badDir := filepath.Join(dir, "bad")
	os.MkdirAll(badDir, 0755)
	os.WriteFile(filepath.Join(badDir, "SKILL.md"), []byte("---\nname: bad\n---\n\nBody."), 0644)

	// Create a regular file (not a directory) - should be skipped
	os.WriteFile(filepath.Join(dir, "not-a-dir.txt"), []byte("hi"), 0644)

	skills, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir failed: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("got %d skills, want 2", len(skills))
	}
}

func TestLoadDir_NonExistent(t *testing.T) {
	skills, err := LoadDir("/nonexistent/path")
	if err != nil {
		t.Fatalf("expected nil error for nonexistent dir, got %v", err)
	}
	if skills != nil {
		t.Fatalf("expected nil skills for nonexistent dir, got %v", skills)
	}
}

func TestLoad_ScriptsDirNoExecutable(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "noexec")
	scriptsDir := filepath.Join(skillDir, "scripts")
	os.MkdirAll(scriptsDir, 0755)

	md := `---
name: noexec
description: Scripts dir without executable files
---

Body.
`
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0644)
	// Write script without execute permission
	os.WriteFile(filepath.Join(scriptsDir, "noexec"), []byte("#!/bin/sh\necho hi"), 0644)

	s, err := Load(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if s.ScriptsDir != "" {
		t.Errorf("ScriptsDir should be empty for non-executable scripts, got %q", s.ScriptsDir)
	}
}
