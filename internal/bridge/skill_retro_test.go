package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/skill"
)

func TestBuildSkillRetroBlock_NoRegistryReturnsEmpty(t *testing.T) {
	b := &Bridge{}
	if got := b.buildSkillRetroBlock(); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestBuildSkillRetroBlock_EmptyRegistryReturnsEmpty(t *testing.T) {
	reg := skill.NewRegistry(nil)
	b := &Bridge{skills: reg}
	if got := b.buildSkillRetroBlock(); got != "" {
		t.Errorf("expected empty for empty registry, got %q", got)
	}
}

func TestBuildSkillRetroBlock_ShowsHotAndLazy(t *testing.T) {
	skills := []*skill.Skill{
		{Name: "hot-one", Description: "active hot", Tier: skill.TierHot, Body: "body"},
		{Name: "lazy-one", Description: "rare lazy", Tier: skill.TierLazy, Body: "body"},
		// Core skills should NOT appear in the retro — they aren't
		// agent-authored and aren't subject to the hot/lazy budget.
		{Name: "core-one", Description: "platform core", Tier: skill.TierCore, Body: "body"},
	}
	reg := skill.NewRegistry(skills)
	b := &Bridge{skills: reg}

	out := b.buildSkillRetroBlock()
	if !strings.Contains(out, "Skill Inventory Retro") {
		t.Error("missing header")
	}
	if !strings.Contains(out, "hot-one") {
		t.Error("hot skill not listed")
	}
	if !strings.Contains(out, "lazy-one") {
		t.Error("lazy skill not listed")
	}
	if strings.Contains(out, "core-one") {
		t.Error("core skill should not appear in retro")
	}
	if !strings.Contains(out, "Hot budget") {
		t.Error("budget summary missing")
	}
	if !strings.Contains(out, "Action menu") {
		t.Error("action menu missing")
	}
}

func TestBuildSkillRetroBlock_FlagsStaleSkill(t *testing.T) {
	skills := []*skill.Skill{
		{Name: "ghost-skill", Description: "never run", Tier: skill.TierHot, Body: "body"},
	}
	reg := skill.NewRegistry(skills)
	b := &Bridge{skills: reg}
	out := b.buildSkillRetroBlock()
	if !strings.Contains(out, "0 runs ever") {
		t.Error("expected 0-runs annotation")
	}
	if !strings.Contains(out, "stale") {
		t.Error("expected stale marker")
	}
}

func TestBuildSkillRetroBlock_ShowsUsageStats(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "measured")
	os.MkdirAll(skillDir, 0o755)

	// Write a recent USAGE record (exit 0).
	nowTS := time.Now().UTC().Format(time.RFC3339)
	os.WriteFile(filepath.Join(skillDir, "USAGE.jsonl"),
		[]byte(`{"ts":"`+nowTS+`","name":"measured","version":null,"exit_code":0,"duration_ms":42,"args":[]}`),
		0o644)

	skills := []*skill.Skill{
		{Name: "measured", Description: "has history", Tier: skill.TierHot, Body: "body", Dir: skillDir},
	}
	reg := skill.NewRegistry(skills)
	b := &Bridge{skills: reg}
	out := b.buildSkillRetroBlock()
	if !strings.Contains(out, "1 runs total") {
		t.Errorf("expected '1 runs total' in output, got: %s", out)
	}
	if !strings.Contains(out, "100% success") {
		t.Error("expected 100% success rate")
	}
	if !strings.Contains(out, "42ms") {
		t.Error("expected avg duration")
	}
}

func TestBuildSkillInventoryDigest_Empty(t *testing.T) {
	b := &Bridge{}
	if got := b.buildSkillInventoryDigest(); got != "" {
		t.Errorf("expected empty for no registry, got %q", got)
	}
	b.skills = skill.NewRegistry(nil)
	if got := b.buildSkillInventoryDigest(); got != "" {
		t.Errorf("expected empty for empty registry, got %q", got)
	}
}

func TestBuildSkillInventoryDigest_OnlyCoreReturnsEmpty(t *testing.T) {
	skills := []*skill.Skill{
		{Name: "shell-remember", Description: "platform", Tier: skill.TierCore},
	}
	b := &Bridge{skills: skill.NewRegistry(skills)}
	if got := b.buildSkillInventoryDigest(); got != "" {
		t.Errorf("core-only should yield empty digest (nothing agent-authored), got %q", got)
	}
}

func TestBuildSkillInventoryDigest_ListsHotAndLazy(t *testing.T) {
	skills := []*skill.Skill{
		{Name: "core-thing", Description: "should not appear", Tier: skill.TierCore},
		{Name: "dairy-tally", Version: "v2", Description: "tally dairy points", Tier: skill.TierHot},
		{Name: "flonase-log", Description: "log doses", Tier: skill.TierLazy},
	}
	b := &Bridge{skills: skill.NewRegistry(skills)}
	out := b.buildSkillInventoryDigest()

	if strings.Contains(out, "core-thing") {
		t.Error("core skill should NOT be in agent-authored digest")
	}
	if !strings.Contains(out, "dairy-tally@v2") {
		t.Error("hot skill with version missing")
	}
	if !strings.Contains(out, "[hot, unused]") {
		t.Error("hot tier + unused flag missing")
	}
	if !strings.Contains(out, "flonase-log") {
		t.Error("lazy skill missing")
	}
	if !strings.Contains(out, "[lazy, unused]") {
		t.Error("lazy tier + unused flag missing")
	}
	if !strings.HasPrefix(out, "Skills I've authored") {
		t.Errorf("digest should lead with header line, got: %s", out)
	}
}

func TestBuildSkillInventoryDigest_IncludesUsageStats(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "logged")
	os.MkdirAll(skillDir, 0o755)
	nowTS := time.Now().UTC().Format(time.RFC3339)
	lines := []string{
		`{"ts":"` + nowTS + `","name":"logged","version":null,"exit_code":0,"duration_ms":10,"args":[]}`,
		`{"ts":"` + nowTS + `","name":"logged","version":null,"exit_code":0,"duration_ms":15,"args":[]}`,
	}
	os.WriteFile(filepath.Join(skillDir, "USAGE.jsonl"),
		[]byte(strings.Join(lines, "\n")), 0o644)

	skills := []*skill.Skill{
		{Name: "logged", Description: "runs often", Tier: skill.TierHot, Dir: skillDir},
	}
	b := &Bridge{skills: skill.NewRegistry(skills)}
	out := b.buildSkillInventoryDigest()
	// "2r/100%" is the compact form used in the digest.
	if !strings.Contains(out, "2r/100%") {
		t.Errorf("expected usage-stat shorthand in digest, got: %s", out)
	}
}

func TestBuildSkillInventoryDigest_TruncatesLongDescription(t *testing.T) {
	long := strings.Repeat("a very long description ", 20) // >80 chars
	skills := []*skill.Skill{
		{Name: "wordy", Description: long, Tier: skill.TierHot},
	}
	b := &Bridge{skills: skill.NewRegistry(skills)}
	out := b.buildSkillInventoryDigest()
	if !strings.Contains(out, "...") {
		t.Error("expected truncation marker for long description")
	}
}

func TestBuildSkillRetroBlock_ShowsPlaygroundCandidates(t *testing.T) {
	// Simulate a per-agent skills dir so pickPerAgentSkillsDir finds it.
	base := t.TempDir()
	agentsDir := filepath.Join(base, ".shell", "agents", "testbot", "skills")
	os.MkdirAll(agentsDir, 0o755)

	// One loaded skill so pickPerAgentSkillsDir has an anchor.
	realSkill := filepath.Join(agentsDir, "real")
	os.MkdirAll(realSkill, 0o755)

	// Two playground candidates — one with SKILL.md, one without.
	os.MkdirAll(filepath.Join(agentsDir, "playground", "ready"), 0o755)
	os.WriteFile(filepath.Join(agentsDir, "playground", "ready", "SKILL.md"), []byte("draft"), 0o644)
	os.MkdirAll(filepath.Join(agentsDir, "playground", "draft-only"), 0o755)

	skills := []*skill.Skill{
		{Name: "real", Description: "anchor", Tier: skill.TierLazy, Dir: realSkill, SkillRoot: realSkill},
	}
	reg := skill.NewRegistry(skills)
	b := &Bridge{skills: reg}
	out := b.buildSkillRetroBlock()
	if !strings.Contains(out, "ready") {
		t.Error("playground candidate 'ready' missing")
	}
	if !strings.Contains(out, "draft-only") {
		t.Error("playground candidate 'draft-only' missing")
	}
	if !strings.Contains(out, "ready to graduate") {
		t.Error("expected graduation hint for ready/SKILL.md")
	}
}
