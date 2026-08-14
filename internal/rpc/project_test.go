package rpc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/rcliao/shell/internal/store"
)

func newProjectTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Server{store: st}, st
}

func postProject(t *testing.T, s *Server, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/project", bytes.NewReader(b))
	w := httptest.NewRecorder()
	s.handleProject(w, req)

	var out map[string]any
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return w.Code, out
}

func TestProjectCreateDerivesSlugAndGhostTag(t *testing.T) {
	s, _ := newProjectTestServer(t)

	code, out := postProject(t, s, map[string]any{
		"action": "create", "title": "Housing Search 2026", "chat_id": 42,
		"emoji": "🔥", "export_ref": "notion-abc123",
		"doc_path": "workspace/projects/housing-search-2026/doc.md",
	})
	if code != http.StatusOK {
		t.Fatalf("create returned %d: %v", code, out)
	}
	if out["slug"] != "housing-search-2026" {
		t.Errorf("slug = %v", out["slug"])
	}
	if out["ghost_tag"] != "project:housing-search-2026" {
		t.Errorf("ghost_tag = %v", out["ghost_tag"])
	}
	// export_kind defaults to notion when an export_ref is given.
	if out["export_kind"] != "notion" {
		t.Errorf("export_kind = %v, want notion", out["export_kind"])
	}
	// 🔥 is reaction-capable — no warning.
	if _, has := out["warning"]; has {
		t.Errorf("unexpected warning for reaction-capable emoji: %v", out["warning"])
	}
}

func TestProjectCreateWarnsOnNonReactionEmoji(t *testing.T) {
	s, _ := newProjectTestServer(t)

	code, out := postProject(t, s, map[string]any{
		"action": "create", "title": "Sakura Trip", "chat_id": 42, "emoji": "🌸",
	})
	if code != http.StatusOK {
		t.Fatalf("create returned %d: %v", code, out)
	}
	// Stored anyway — warn, never hard-reject.
	if out["emoji"] != "🌸" {
		t.Errorf("emoji should be stored despite warning, got %v", out["emoji"])
	}
	warning, _ := out["warning"].(string)
	if warning == "" {
		t.Error("expected a warning for a non-reaction-capable emoji")
	}
}

func TestProjectCreateValidation(t *testing.T) {
	s, _ := newProjectTestServer(t)

	if code, _ := postProject(t, s, map[string]any{"action": "create", "chat_id": 42}); code != http.StatusBadRequest {
		t.Errorf("missing title should 400, got %d", code)
	}
	if code, _ := postProject(t, s, map[string]any{"action": "create", "title": "x"}); code != http.StatusBadRequest {
		t.Errorf("missing chat_id should 400, got %d", code)
	}
	if code, _ := postProject(t, s, map[string]any{"action": "bogus"}); code != http.StatusBadRequest {
		t.Errorf("unknown action should 400, got %d", code)
	}
}

func TestProjectGetAndNotFound(t *testing.T) {
	s, _ := newProjectTestServer(t)
	postProject(t, s, map[string]any{"action": "create", "title": "Alpha", "chat_id": 42})

	code, out := postProject(t, s, map[string]any{"action": "get", "slug": "alpha"})
	if code != http.StatusOK || out["title"] != "Alpha" {
		t.Errorf("get returned %d: %v", code, out)
	}

	code, _ = postProject(t, s, map[string]any{"action": "get", "slug": "missing"})
	if code != http.StatusNotFound {
		t.Errorf("unknown slug should 404, got %d", code)
	}
}

func TestProjectListScopedByChat(t *testing.T) {
	s, _ := newProjectTestServer(t)
	postProject(t, s, map[string]any{"action": "create", "title": "Alpha", "chat_id": 42})
	postProject(t, s, map[string]any{"action": "create", "title": "Beta", "chat_id": -100200300})

	_, out := postProject(t, s, map[string]any{"action": "list", "chat_id": 42})
	if got, _ := out["projects"].([]any); len(got) != 1 {
		t.Errorf("chat-scoped list should return 1 project, got %v", out["projects"])
	}

	_, all := postProject(t, s, map[string]any{"action": "list"})
	if got, _ := all["projects"].([]any); len(got) != 2 {
		t.Errorf("chat_id=0 list should return all projects, got %v", all["projects"])
	}
}

func TestProjectStatusUpdateReadsBack(t *testing.T) {
	s, st := newProjectTestServer(t)
	postProject(t, s, map[string]any{"action": "create", "title": "Lifecycle", "chat_id": 42})

	code, out := postProject(t, s, map[string]any{"action": "status", "slug": "lifecycle", "status": "paused"})
	if code != http.StatusOK || out["status"] != "paused" {
		t.Fatalf("status returned %d: %v", code, out)
	}
	p, _ := st.GetProjectBySlug("lifecycle")
	if p.Status != "paused" {
		t.Errorf("stored status = %q, want paused", p.Status)
	}

	if code, _ := postProject(t, s, map[string]any{"action": "status", "slug": "lifecycle", "status": "bogus"}); code != http.StatusBadRequest {
		t.Errorf("invalid status should 400, got %d", code)
	}
	if code, _ := postProject(t, s, map[string]any{"action": "status", "slug": "missing", "status": "paused"}); code != http.StatusBadRequest {
		t.Errorf("unknown slug should 400, got %d", code)
	}
}

func TestReactionCapableStripsVariationSelector(t *testing.T) {
	if !reactionCapable("❤️") {
		t.Error("❤ with VS16 should be reaction-capable")
	}
	if !reactionCapable("👀") {
		t.Error("👀 should be reaction-capable")
	}
	if reactionCapable("🌸") {
		t.Error("🌸 is not in Telegram's reaction set")
	}
}
