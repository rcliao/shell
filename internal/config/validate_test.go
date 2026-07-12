package config

import (
	"strings"
	"testing"
)

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// A well-formed config (top-level model set, rotation cap above compaction cap)
// produces no warnings.
func TestClaudeConfig_Validate_Clean(t *testing.T) {
	c := ClaudeConfig{
		Model:            "claude-sonnet-5",
		MaxSessionTokens: 200000,
		RotateMaxTokens:  0, // disabled → no starvation warning
	}
	if w := c.Validate(); len(w) != 0 {
		t.Errorf("expected no warnings, got %v", w)
	}
}

// S5: empty conversation model → warn about the silent CLI-default foot-gun.
func TestClaudeConfig_Validate_EmptyModel(t *testing.T) {
	c := ClaudeConfig{Model: ""} // nothing resolves conversation
	w := c.Validate()
	if !hasWarning(w, "resolves to empty") {
		t.Errorf("expected empty-model warning, got %v", w)
	}
}

// A routing override for conversation satisfies the check even with empty top-level model.
func TestClaudeConfig_Validate_ConversationRoutingCoversEmptyModel(t *testing.T) {
	c := ClaudeConfig{Model: "", ModelRouting: &ModelRouting{Conversation: "claude-sonnet-5"}}
	if hasWarning(c.Validate(), "resolves to empty") {
		t.Error("conversation routing should satisfy the model check")
	}
}

// S4: rotate cap below compaction cap → warn that compaction is unreachable.
func TestClaudeConfig_Validate_CompactionStarved(t *testing.T) {
	c := ClaudeConfig{
		Model:            "claude-sonnet-5",
		RotateMaxTokens:  60000,
		MaxSessionTokens: 200000,
	}
	w := c.Validate()
	if !hasWarning(w, "compaction never runs") {
		t.Errorf("expected compaction-starvation warning, got %v", w)
	}
}

// rotate cap above compaction cap → no starvation warning.
func TestClaudeConfig_Validate_NoStarveWhenRotateHigher(t *testing.T) {
	c := ClaudeConfig{
		Model:            "claude-sonnet-5",
		RotateMaxTokens:  300000,
		MaxSessionTokens: 200000,
	}
	if hasWarning(c.Validate(), "compaction never runs") {
		t.Error("no starvation expected when rotate cap is above compaction cap")
	}
}
