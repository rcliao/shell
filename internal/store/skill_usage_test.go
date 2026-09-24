package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSkillUsageFromToolLog(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	calls := []ToolUse{
		{Name: "Bash", Detail: "command=~/.shell/skills/notion/scripts/notion search x"},
		{Name: "Bash", Detail: "command=~/.shell/skills/notion/scripts/notion get y", Failed: true},
		{Name: "Bash", Detail: "command=cat ~/.shell/agents/pikamini/skills/meal-memo/SKILL.md"},
		{Name: "Bash", Detail: "command=~/.shell/agents/pikamini/skills/meal-memo/scripts/memo add && cat ~/.shell/agents/pikamini/skills/meal-memo/SKILL.md"},
		{Name: "Bash", Detail: "command=~/.shell/agents/pikamini/skills/playground/tidy/scripts/tidy"},
		{Name: "Bash", Detail: "command=ls ~/.shell/skills/"}, // not a skill use
	}
	if err := st.LogToolUses(42, 1, "interactive", calls); err != nil {
		t.Fatal(err)
	}
	u, err := st.SkillUsage(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n := u["notion"]; n.Runs != 2 || n.Failures != 1 || n.Runs7 != 2 {
		t.Errorf("notion = %+v, want 2 runs, 1 failure", n)
	}
	if m := u["meal-memo"]; m.Runs != 1 || m.Reads != 2 {
		t.Errorf("meal-memo = %+v, want 1 run and 2 reads", m)
	}
	if p := u["playground/tidy"]; p.Runs != 1 {
		t.Errorf("playground draft = %+v, want 1 run", p)
	}
	if len(u) != 3 {
		t.Errorf("usage keys = %v, want exactly notion, meal-memo, playground/tidy", u)
	}
	if u["notion"].Last.IsZero() {
		t.Error("last use must be set")
	}
}
