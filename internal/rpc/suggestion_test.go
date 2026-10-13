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

// A steward proposal is filed with its source; any other source, or a
// steward proposal addressed to a chat, is refused.
func TestSuggestionStewardSource(t *testing.T) {
	s, st := newProjectTestServer(t)
	code, out := postSuggestion(t, s, map[string]any{"action": "create", "source": "steward", "title": "garden: plant", "change": "DO: x", "evidence": "e"})
	if code != http.StatusOK {
		t.Fatalf("create: %d %v", code, out)
	}
	got, err := st.GetSuggestion(int64(out["id"].(float64)))
	if err != nil || got.Source != store.SuggestionSourceSteward || got.Audience != "owner" {
		t.Fatalf("stored = %+v, %v", got, err)
	}
	if code, _ := postSuggestion(t, s, map[string]any{"action": "create", "source": "bogus", "title": "t", "change": "c"}); code != http.StatusBadRequest {
		t.Errorf("unknown source = %d, want 400", code)
	}
	if code, _ := postSuggestion(t, s, map[string]any{"action": "create", "source": "steward", "for_chat": 42, "title": "t", "change": "c"}); code != http.StatusBadRequest {
		t.Errorf("steward for a chat = %d, want 400", code)
	}
	if list, _ := st.ProposedFrom(store.SuggestionSourceSteward); len(list) != 1 {
		t.Errorf("ProposedFrom(steward) = %d rows, want 1", len(list))
	}
	// The cap holds server-side: a second is fine, a third waits.
	if code, _ := postSuggestion(t, s, map[string]any{"action": "create", "source": "steward", "title": "t2", "change": "c"}); code != http.StatusOK {
		t.Errorf("second steward proposal = %d, want 200", code)
	}
	if code, _ := postSuggestion(t, s, map[string]any{"action": "create", "source": "steward", "title": "t3", "change": "c"}); code != http.StatusConflict {
		t.Errorf("third steward proposal = %d, want 409", code)
	}
}
