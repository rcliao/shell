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
