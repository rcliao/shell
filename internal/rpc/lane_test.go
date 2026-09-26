package rpc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLaneLabelsTheCurrentMessage(t *testing.T) {
	s, st := newProjectTestServer(t)
	post := func(body map[string]any) int {
		b, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		s.handleLane(w, httptest.NewRequest(http.MethodPost, "/lane", bytes.NewReader(b)))
		return w.Code
	}
	// No message yet: nothing to label.
	if code := post(map[string]any{"chat_id": 42, "lane": "general"}); code != http.StatusNotFound {
		t.Errorf("no message = %d, want 404", code)
	}
	if err := st.SaveSession(42, 0, "claude-sess"); err != nil {
		t.Fatal(err)
	}
	sess, err := st.GetSession(42, 0)
	if err != nil || sess == nil {
		t.Fatalf("session: %v", err)
	}
	if err := st.LogMessage(sess.ID, "user", "那飯店呢"); err != nil {
		t.Fatal(err)
	}
	if code := post(map[string]any{"chat_id": 42, "lane": "japan"}); code != http.StatusBadRequest {
		t.Errorf("unknown lane = %d, want 400", code)
	}
	if code := post(map[string]any{"chat_id": 42, "lane": "general", "note": "small talk"}); code != http.StatusOK {
		t.Fatalf("label = %d", code)
	}
	best, _ := st.BestRouteLabels()
	if len(best) != 1 {
		t.Fatalf("labels = %v", best)
	}
	for _, l := range best {
		if l.Source != "agent" || l.Lane != "general" || l.Note != "small talk" {
			t.Errorf("label = %+v", l)
		}
	}
	if code := post(map[string]any{"chat_id": 0, "lane": "general"}); code != http.StatusBadRequest {
		t.Errorf("system chat = %d, want 400", code)
	}
}
