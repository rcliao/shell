package rpc

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rcliao/shell/internal/project"
)

// An over-budget doc may shrink but not grow: the refusal must never block
// the way out, and must leave the doc and its rev untouched.
func TestProjectDocWriteEnforcesBudget(t *testing.T) {
	s, st, _ := newDocTestServer(t)
	postProject(t, s, map[string]any{"action": "create", "title": "Budget", "chat_id": 42})

	write := func(content string) (int, map[string]any) {
		return postProject(t, s, map[string]any{"action": "doc-write", "slug": "budget", "content": content})
	}
	doc := func(n int) string {
		return "# Budget\n\n## Log\n\n" + strings.Repeat("x", n) + "\n"
	}

	code, out := write(doc(1000))
	if code != http.StatusOK {
		t.Fatalf("under-budget write returned %d: %v", code, out)
	}
	okRev, _ := out["rev"].(string)

	// Crossing the budget is refused, with an actionable message.
	code, out = write(doc(project.DefaultDocBudget + 5000))
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("oversize write returned %d, want 422: %v", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "over budget") || !strings.Contains(msg, `"Log"`) {
		t.Errorf("refusal must explain itself and name the biggest section: %q", msg)
	}
	if p, _ := st.GetProjectBySlug("budget"); p.DocRev != okRev {
		t.Errorf("a refused write moved doc_rev: %q -> %q", okRev, p.DocRev)
	}
	if _, read := postProject(t, s, map[string]any{"action": "doc-read", "slug": "budget"}); read["rev"] != okRev {
		t.Errorf("a refused write changed the doc: rev %v, want %v", read["rev"], okRev)
	}

	// The way out: a doc that is ALREADY over budget (it grew before the rule
	// existed) must accept a write that shrinks it, even while still oversize.
	p, _ := st.GetProjectBySlug("budget")
	dir, err := s.projectDocDir(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := project.WriteDoc(dir, doc(project.DefaultDocBudget*3), "legacy growth"); err != nil {
		t.Fatal(err)
	}
	code, out = write(doc(project.DefaultDocBudget * 2))
	if code != http.StatusOK {
		t.Fatalf("shrinking an over-budget doc returned %d, want 200: %v", code, out)
	}
	if code, out = write(doc(project.DefaultDocBudget*2 + 100)); code != http.StatusUnprocessableEntity {
		t.Errorf("growing it again returned %d, want 422: %v", code, out)
	}
}
