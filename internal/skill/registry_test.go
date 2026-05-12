package skill

import (
	"strings"
	"testing"
)

func TestRegistrySystemPrompt(t *testing.T) {
	skills := []*Skill{
		{Name: "hello", Description: "Simple greeting skill", ScriptsDir: "/skills/hello/scripts", Dir: "/skills/hello", Body: "# Hello\n\nUsage: `scripts/hello <name>`"},
		{Name: "deploy-check", Description: "Check deployment status", Dir: "/skills/deploy-check", Body: "# Deploy Check"},
	}
	r := NewRegistry(skills)

	prompt := r.SystemPrompt()
	if !strings.Contains(prompt, "## Available Skills") {
		t.Error("missing header")
	}
	if !strings.Contains(prompt, "### hello") {
		t.Error("missing hello skill heading")
	}
	if !strings.Contains(prompt, "Dir: `/skills/hello`") {
		t.Error("missing hello dir")
	}
	if !strings.Contains(prompt, "Scripts: `/skills/hello/scripts/`") {
		t.Error("missing hello scripts dir")
	}
	if !strings.Contains(prompt, "### deploy-check") {
		t.Error("missing deploy-check skill heading")
	}
	if !strings.Contains(prompt, "# Hello") {
		t.Error("missing skill body")
	}
	// deploy-check has no ScriptsDir, should not show Scripts line
	// Split prompt by deploy-check section and check
	deploySection := prompt[strings.Index(prompt, "### deploy-check"):]
	if strings.Contains(deploySection, "Scripts:") {
		t.Error("deploy-check should not have Scripts line")
	}
}

func TestRegistrySystemPrompt_Empty(t *testing.T) {
	r := NewRegistry(nil)
	if r.SystemPrompt() != "" {
		t.Error("expected empty prompt for no skills")
	}
}

func TestRegistryAllowedTools(t *testing.T) {
	skills := []*Skill{
		{Name: "a", Description: "a", AllowedTools: []string{"Bash", "Read"}},
		{Name: "b", Description: "b", AllowedTools: []string{"Write", "Read"}}, // Read is duplicate
	}
	r := NewRegistry(skills)

	tools := r.AllowedTools()
	if len(tools) != 3 {
		t.Fatalf("AllowedTools = %v, want 3 unique entries", tools)
	}
	expected := map[string]bool{"Bash": true, "Read": true, "Write": true}
	for _, tool := range tools {
		if !expected[tool] {
			t.Errorf("unexpected tool: %s", tool)
		}
	}
}

func TestRegistryGet(t *testing.T) {
	skills := []*Skill{
		{Name: "hello", Description: "greeting"},
	}
	r := NewRegistry(skills)

	if s := r.Get("hello"); s == nil || s.Name != "hello" {
		t.Error("Get(hello) should return the skill")
	}
	if s := r.Get("nope"); s != nil {
		t.Error("Get(nope) should return nil")
	}
}

func TestRegistryHas(t *testing.T) {
	skills := []*Skill{
		{Name: "hello", Description: "greeting"},
	}
	r := NewRegistry(skills)

	if !r.Has("hello") {
		t.Error("Has(hello) should be true")
	}
	if r.Has("nope") {
		t.Error("Has(nope) should be false")
	}
}

func TestCatalogPrompt_ThreeTiers(t *testing.T) {
	skills := []*Skill{
		{Name: "core-skill", Description: "Always on", Tier: TierCore, Body: "CORE BODY"},
		{Name: "hot-skill", Description: "Frequently used", Tier: TierHot, Body: "HOT BODY"},
		{Name: "lazy-skill", Description: "Rare path", Tier: TierLazy, Body: "LAZY BODY", Dir: "/skills/lazy-skill"},
	}
	r := NewRegistry(skills)
	p := r.CatalogPrompt()

	// Core: full body appears.
	if !strings.Contains(p, "CORE BODY") {
		t.Error("core body missing")
	}
	// Hot: full body appears under Hot heading.
	if !strings.Contains(p, "### Hot skills") {
		t.Error("Hot skills heading missing")
	}
	if !strings.Contains(p, "HOT BODY") {
		t.Error("hot body missing")
	}
	// Lazy: body does NOT appear; description + on-disk path does.
	if strings.Contains(p, "LAZY BODY") {
		t.Error("lazy body should NOT be inlined")
	}
	if !strings.Contains(p, "### Lazy skills") {
		t.Error("Lazy skills heading missing")
	}
	if !strings.Contains(p, "/skills/lazy-skill/SKILL.md") {
		t.Error("lazy entry missing on-disk path")
	}
}

func TestCatalogPrompt_HotBudgetOverflowDemotes(t *testing.T) {
	// Build a body that exceeds the hot budget on its own.
	huge := strings.Repeat("x", (HotTierBudget+100)*4)
	skills := []*Skill{
		{Name: "tiny-hot", Description: "small hot skill", Tier: TierHot, Body: "compact body"},
		{Name: "giant-hot", Description: "over budget", Tier: TierHot, Body: huge, Dir: "/skills/giant-hot"},
	}
	r := NewRegistry(skills)
	p := r.CatalogPrompt()

	// tiny-hot fits and appears under Hot.
	if !strings.Contains(p, "compact body") {
		t.Error("tiny hot skill body missing")
	}
	// giant-hot got demoted — body must NOT be in the prompt.
	if strings.Contains(p, huge) {
		t.Error("over-budget hot body should have been demoted, not inlined")
	}
	// But it should still appear as a lazy entry so the agent knows it exists.
	if !strings.Contains(p, "/skills/giant-hot/SKILL.md") {
		t.Error("demoted skill should appear in Lazy catalog")
	}
}

func TestCatalogPrompt_LazyBudgetTruncates(t *testing.T) {
	// Generate enough lazy skills that the catalog budget overflows.
	// Each entry carries an ~80-char description so we hit the cap with ~50 skills.
	var skills []*Skill
	for i := 0; i < 80; i++ {
		skills = append(skills, &Skill{
			Name:        "lazy-" + string(rune('a'+(i%26))) + string(rune('0'+(i/26))),
			Description: strings.Repeat("word ", 20), // ~100 chars
			Tier:        TierLazy,
			Dir:         "/fake/skills/x",
			SkillRoot:   "/fake/skills/x",
		})
	}
	r := NewRegistry(skills)
	p := r.CatalogPrompt()

	if !strings.Contains(p, "### Lazy skills") {
		t.Error("lazy heading missing")
	}
	if !strings.Contains(p, "more skills") {
		t.Error("expected truncation marker when lazy catalog overflows")
	}
}

func TestCatalogPrompt_LegacyCoreFieldStillWorks(t *testing.T) {
	// Simulate a skill loaded via Load() with Core=true — the loader sets
	// Tier=TierCore in that case. Here we simulate the end state directly.
	skills := []*Skill{
		{Name: "legacy", Description: "legacy core", Core: true, Tier: TierCore, Body: "LEGACY BODY"},
	}
	r := NewRegistry(skills)
	p := r.CatalogPrompt()
	if !strings.Contains(p, "LEGACY BODY") {
		t.Error("legacy core skill body should appear in prompt")
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"abcd", 1},
		{"abcde", 2},
		{strings.Repeat("x", 400), 100},
	}
	for _, c := range cases {
		if got := EstimateTokens(c.in); got != c.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
