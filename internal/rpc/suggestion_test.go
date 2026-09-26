package rpc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rcliao/shell/internal/store"
)

func postSuggestion(t *testing.T, s *Server, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/suggestion", bytes.NewReader(b))
	w := httptest.NewRecorder()
	s.handleSuggestion(w, req)
	var out map[string]any
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return w.Code, out
}

func TestSuggestionDecideOnlyFromOwnerChat(t *testing.T) {
	s, st := newProjectTestServer(t)
	s.ownerChatID = 42

	code, out := postSuggestion(t, s, map[string]any{"action": "create", "title": "t", "change": "c", "evidence": "e"})
	if code != http.StatusOK {
		t.Fatalf("create: %d %v", code, out)
	}
	id := int64(out["id"].(float64))

	// From the system chat (a review turn), the agent cannot decide.
	if code, _ := postSuggestion(t, s, map[string]any{"action": "decide", "chat_id": 0, "id": id, "status": "accepted"}); code != http.StatusForbidden {
		t.Errorf("decide from chat 0 = %d, want 403", code)
	}
	// Nor from another chat.
	if code, _ := postSuggestion(t, s, map[string]any{"action": "decide", "chat_id": -100200300, "id": id, "status": "accepted"}); code != http.StatusForbidden {
		t.Errorf("decide from another chat = %d, want 403", code)
	}
	// From the owner's chat it lands.
	if code, out := postSuggestion(t, s, map[string]any{"action": "decide", "chat_id": 42, "id": id, "status": "declined", "note": "not now"}); code != http.StatusOK {
		t.Fatalf("owner decide = %d %v", code, out)
	}
	got, _ := st.GetSuggestion(id)
	if got.Status != store.SuggestionDeclined || got.DecidedBy != "owner" || got.Note != "not now" {
		t.Fatalf("stored = %+v", got)
	}
	// Missing change is refused at create.
	if code, _ := postSuggestion(t, s, map[string]any{"action": "create", "title": "t"}); code != http.StatusBadRequest {
		t.Errorf("create without change = %d, want 400", code)
	}
}

func TestSuggestionDecideDisabledWithoutOwner(t *testing.T) {
	s, _ := newProjectTestServer(t)
	_, out := postSuggestion(t, s, map[string]any{"action": "create", "title": "t", "change": "c"})
	id := out["id"]
	if code, _ := postSuggestion(t, s, map[string]any{"action": "decide", "chat_id": 0, "id": id, "status": "accepted"}); code != http.StatusForbidden {
		t.Errorf("decide with no owner configured = %d, want 403", code)
	}
}

func TestChatSuggestionDecidedOnlyInItsChat(t *testing.T) {
	s, st := newProjectTestServer(t)
	s.ownerChatID = 42
	_, out := postSuggestion(t, s, map[string]any{"action": "create", "title": "t", "change": "c", "for_chat": -100200300})
	id := out["id"]
	if code, _ := postSuggestion(t, s, map[string]any{"action": "decide", "chat_id": 42, "id": id, "status": "accepted"}); code != http.StatusForbidden {
		t.Errorf("the owner's chat cannot decide a family chat's suggestion: %d", code)
	}
	if code, _ := postSuggestion(t, s, map[string]any{"action": "decide", "chat_id": -100200300, "id": id, "status": "accepted", "note": "好"}); code != http.StatusOK {
		t.Fatalf("its own chat must decide it: %d", code)
	}
	got, _ := st.GetSuggestion(int64(id.(float64)))
	if got.DecidedBy != "chat" || got.Note != "好" || got.Audience != store.ChatAudience(-100200300) {
		t.Errorf("stored = %+v", got)
	}
}
