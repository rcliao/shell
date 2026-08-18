package rpc

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/store"
)

// newDocTestServer is newProjectTestServer plus a workspace, enabling the P2
// doc layer.
func newDocTestServer(t *testing.T) (*Server, *store.Store, string) {
	t.Helper()
	s, st := newProjectTestServer(t)
	s.workspaceDir = t.TempDir()
	return s, st, s.workspaceDir
}

func TestProjectCreateScaffoldsDocRepo(t *testing.T) {
	s, st, ws := newDocTestServer(t)

	code, out := postProject(t, s, map[string]any{
		"action": "create", "title": "Synthetic Trip", "chat_id": 42,
		"instructions": "budget-conscious options first",
	})
	if code != http.StatusOK {
		t.Fatalf("create returned %d: %v", code, out)
	}
	if out["doc_path"] != "projects/synthetic-trip/doc.md" {
		t.Errorf("doc_path = %v", out["doc_path"])
	}
	rev, _ := out["doc_rev"].(string)
	if rev == "" {
		t.Fatal("create returned no doc_rev — the scaffold receipt is missing")
	}

	// The doc really exists, in its own repo, seeded from the template.
	data, err := os.ReadFile(filepath.Join(ws, "projects", "synthetic-trip", "doc.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Synthetic Trip", "## 限制", "budget-conscious options first"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("scaffolded doc missing %q", want)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "projects", "synthetic-trip", ".git")); err != nil {
		t.Error("project doc dir is not its own git repo")
	}

	// The registry row carries the same path + rev.
	p, _ := st.GetProjectBySlug("synthetic-trip")
	if p.DocPath != "projects/synthetic-trip/doc.md" || p.DocRev != rev {
		t.Errorf("stored doc_path/doc_rev = %q/%q, want path + rev %q", p.DocPath, p.DocRev, rev)
	}
}

// An explicit --doc-path means an EXISTING external file: the doc layer must
// leave it alone — no repo, no scaffold, path stored verbatim.
func TestProjectCreateExplicitDocPathSkipsScaffold(t *testing.T) {
	s, st, ws := newDocTestServer(t)

	code, out := postProject(t, s, map[string]any{
		"action": "create", "title": "External Doc", "chat_id": 42,
		"doc_path": "workspace/notes/external.md",
	})
	if code != http.StatusOK {
		t.Fatalf("create returned %d: %v", code, out)
	}
	p, _ := st.GetProjectBySlug("external-doc")
	if p.DocPath != "workspace/notes/external.md" || p.DocRev != "" {
		t.Errorf("external doc binding was touched: path=%q rev=%q", p.DocPath, p.DocRev)
	}
	if _, err := os.Stat(filepath.Join(ws, "projects", "external-doc")); !os.IsNotExist(err) {
		t.Error("no repo should be created for an external doc_path")
	}
	// And the doc verbs refuse it rather than minting fake receipts.
	if code, _ := postProject(t, s, map[string]any{"action": "doc-read", "slug": "external-doc"}); code != http.StatusBadRequest {
		t.Errorf("doc-read on an external doc should 400, got %d", code)
	}
}

func TestProjectDocWriteReturnsReceiptAndLogsVerification(t *testing.T) {
	s, st, _ := newDocTestServer(t)
	_, created := postProject(t, s, map[string]any{"action": "create", "title": "Receipts", "chat_id": 42})
	scaffoldRev, _ := created["doc_rev"].(string)

	code, out := postProject(t, s, map[string]any{
		"action": "doc-write", "slug": "receipts",
		"content": "# Receipts\n\n## 現況\n\nfirst research pass done\n",
		"attribution": "research pass",
	})
	if code != http.StatusOK {
		t.Fatalf("doc-write returned %d: %v", code, out)
	}
	rev, _ := out["rev"].(string)
	if rev == "" || rev == scaffoldRev {
		t.Fatalf("doc-write rev = %q (scaffold %q) — must be a new commit hash", rev, scaffoldRev)
	}

	// Read-back serves the new content at the new rev.
	code, read := postProject(t, s, map[string]any{"action": "doc-read", "slug": "receipts"})
	if code != http.StatusOK {
		t.Fatalf("doc-read returned %d: %v", code, read)
	}
	if read["rev"] != rev {
		t.Errorf("doc-read rev = %v, want %v", read["rev"], rev)
	}
	if content, _ := read["content"].(string); !strings.Contains(content, "first research pass done") {
		t.Errorf("doc-read content did not round-trip: %q", content)
	}

	// doc_rev advanced in the registry.
	p, _ := st.GetProjectBySlug("receipts")
	if p.DocRev != rev {
		t.Errorf("stored doc_rev = %q, want %q", p.DocRev, rev)
	}

	// One verified write-hygiene row was logged for the project write.
	sum, err := st.GetWriteHygieneSummary(42, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Verified != 1 {
		t.Errorf("write_verifications verified = %d, want 1", sum.Verified)
	}

	// The write enqueued a Notion render task for exactly this rev (P3 Wave C)
	// — the consumer does the Notion I/O later, off this request path.
	tasks, err := st.ListTasks(store.TaskQueued, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, task := range tasks {
		if task.Kind == project.RenderKind && task.PartitionKey == "render:receipts" {
			found = true
		}
	}
	if !found {
		t.Errorf("no project.render task enqueued after doc-write; queued = %+v", tasks)
	}
}

// get on a rendered project returns the mirrored page URL; a pre-P3
// export_ref (no block map — possibly a database id) must NOT get one.
func TestProjectGetNotionURL(t *testing.T) {
	s, st := newProjectTestServer(t)
	postProject(t, s, map[string]any{
		"action": "create", "title": "Mirrored", "chat_id": 42, "export_ref": "abc-def",
	})

	// export_ref without a block map: no URL.
	code, out := postProject(t, s, map[string]any{"action": "get", "slug": "mirrored"})
	if code != http.StatusOK {
		t.Fatalf("get returned %d", code)
	}
	if _, has := out["notion_url"]; has {
		t.Errorf("unrendered export_ref got a URL: %v", out["notion_url"])
	}

	// With a rendered block map: URL with dashes stripped.
	bm := `{"sections":{"目標":{"hash":"h","blocks":["b1"]}}}`
	if err := st.UpdateProjectFields("mirrored", store.ProjectFieldUpdate{BlockMap: &bm}); err != nil {
		t.Fatal(err)
	}
	_, out = postProject(t, s, map[string]any{"action": "get", "slug": "mirrored"})
	if out["notion_url"] != "https://notion.so/abcdef" {
		t.Errorf("notion_url = %v", out["notion_url"])
	}
}

func TestProjectDocWriteValidation(t *testing.T) {
	s, _, _ := newDocTestServer(t)
	postProject(t, s, map[string]any{"action": "create", "title": "Guard", "chat_id": 42})

	if code, _ := postProject(t, s, map[string]any{"action": "doc-write", "slug": "guard"}); code != http.StatusBadRequest {
		t.Errorf("missing content should 400, got %d", code)
	}
	if code, _ := postProject(t, s, map[string]any{"action": "doc-write", "slug": "missing", "content": "x"}); code != http.StatusNotFound {
		t.Errorf("unknown slug should 404, got %d", code)
	}
	if code, _ := postProject(t, s, map[string]any{"action": "doc-read", "slug": "missing"}); code != http.StatusNotFound {
		t.Errorf("doc-read unknown slug should 404, got %d", code)
	}
}

// Without a workspace (doc layer disabled), create still succeeds as a
// registry-only project — P1 behavior is preserved bit-for-bit.
func TestProjectCreateWithoutWorkspaceStaysRegistryOnly(t *testing.T) {
	s, st := newProjectTestServer(t) // no workspaceDir
	code, _ := postProject(t, s, map[string]any{"action": "create", "title": "Bare", "chat_id": 42})
	if code != http.StatusOK {
		t.Fatalf("create returned %d", code)
	}
	p, _ := st.GetProjectBySlug("bare")
	if p.DocPath != "" || p.DocRev != "" {
		t.Errorf("registry-only create set doc fields: %q %q", p.DocPath, p.DocRev)
	}
}
