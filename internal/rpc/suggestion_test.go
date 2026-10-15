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
	for _, slug := range []string{"garden", "trip", "house"} {
		if _, err := st.CreateProject(store.Project{Slug: slug, Title: slug, ChatID: -100200300, MessageThreadID: 77}); err != nil {
			t.Fatal(err)
		}
	}
	steward := func(project, title string) (int, map[string]any) {
		return postSuggestion(t, s, map[string]any{"action": "create", "source": "steward", "project": project, "title": title, "change": "要我做嗎？", "evidence": "DO — e"})
	}

	// Shadow: addressed to the owner, tagged with its project.
	code, out := steward("garden", "plant")
	if code != http.StatusOK {
		t.Fatalf("create: %d %v", code, out)
	}
	got, err := st.GetSuggestion(int64(out["id"].(float64)))
	if err != nil || got.Source != store.SuggestionSourceSteward || got.Audience != "owner" || got.Project != "garden" {
		t.Fatalf("stored = %+v, %v", got, err)
	}
	if code, _ := postSuggestion(t, s, map[string]any{"action": "create", "source": "bogus", "title": "t", "change": "c"}); code != http.StatusBadRequest {
		t.Errorf("unknown source = %d, want 400", code)
	}
	if code, _ := postSuggestion(t, s, map[string]any{"action": "create", "source": "steward", "for_chat": 42, "project": "trip", "title": "t", "change": "c"}); code != http.StatusBadRequest {
		t.Errorf("steward with for_chat = %d, want 400", code)
	}
	if code, _ := steward("", "t"); code != http.StatusBadRequest {
		t.Errorf("steward without project = %d, want 400", code)
	}
	if code, _ := steward("nope", "t"); code != http.StatusBadRequest {
		t.Errorf("steward for an unknown project = %d, want 400", code)
	}
	// A shadow row never holds its project.
	if code, out := steward("garden", "again"); code != http.StatusOK {
		t.Fatalf("second shadow proposal for garden = %d %v", code, out)
	}
	for _, sg := range mustProposed(t, st) {
		st.MarkSuggestionsDelivered([]int64{sg.ID})
	}

	// An owner-only row for trip, which the thread proposal will replace.
	_, out = steward("trip", "old")
	shadowTrip := int64(out["id"].(float64))
	st.MarkSuggestionsDelivered([]int64{shadowTrip})

	// "on": addressed to the project's chat and thread.
	s.stewardInThreads = true
	code, out = steward("trip", "book")
	if code != http.StatusOK {
		t.Fatalf("create in threads: %d %v", code, out)
	}
	got, _ = st.GetSuggestion(int64(out["id"].(float64)))
	if got.Audience != store.ChatAudience(-100200300) || got.ThreadID != 77 || got.Project != "trip" {
		t.Fatalf("in threads stored = %+v", got)
	}
	// The family's proposal replaces the owner's copy.
	if old, _ := st.GetSuggestion(shadowTrip); old.Status != store.SuggestionWithdrawn || old.DecidedBy != "steward" {
		t.Errorf("owner-only trip row after re-file = %+v, want withdrawn by steward", old)
	}
	if old, _ := st.OwnerOnlyStewardFor("garden"); len(old) != 2 {
		t.Errorf("garden's owner-only rows = %d, want 2 (untouched)", len(old))
	}
	// One open question per project.
	if code, _ := steward("trip", "again"); code != http.StatusConflict {
		t.Errorf("second proposal for trip = %d, want 409", code)
	}
	// The per-beat cap holds server-side: two waiting, a third waits.
	if code, _ := steward("house", "t2"); code != http.StatusOK {
		t.Errorf("second waiting proposal = %d, want 200", code)
	}
	if code, _ := steward("garden", "t3"); code != http.StatusConflict {
		t.Errorf("third waiting proposal = %d, want 409", code)
	}
	if list, _ := st.ProposedFrom(store.SuggestionSourceSteward); len(list) != 2 {
		t.Errorf("ProposedFrom(steward) = %d rows, want 2", len(list))
	}

	// The project's people answer it from that chat.
	st.MarkSuggestionsDelivered([]int64{got.ID})
	if code, out := postSuggestion(t, s, map[string]any{"action": "decide", "id": got.ID, "status": "accepted", "chat_id": -100200300, "note": "好"}); code != http.StatusOK {
		t.Errorf("decide from the project chat = %d %v", code, out)
	}
}

func mustProposed(t *testing.T, st *store.Store) []store.Suggestion {
	t.Helper()
	list, err := st.ProposedFrom(store.SuggestionSourceSteward)
	if err != nil {
		t.Fatal(err)
	}
	return list
}
