package rpc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rcliao/shell/internal/store"
)

func TestEventRPC(t *testing.T) {
	s, st := newProjectTestServer(t)
	id, _, _ := st.AddEvent(store.Event{Source: "gmail", Kind: "email.received", DedupID: "m1", Summary: "Flight"})
	post := func(body map[string]any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		s.handleEvent(w, httptest.NewRequest(http.MethodPost, "/event", bytes.NewReader(b)))
		var out map[string]any
		json.NewDecoder(w.Body).Decode(&out)
		return w.Code, out
	}
	if code, out := post(map[string]any{"action": "list"}); code != 200 || len(out["events"].([]any)) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	if code, _ := post(map[string]any{"action": "done", "id": id, "note": "added to the trip doc"}); code != 200 {
		t.Fatalf("done = %d", code)
	}
	if code, out := post(map[string]any{"action": "list"}); code != 200 || out["events"] != nil && len(out["events"].([]any)) != 0 {
		t.Fatalf("a done event is not listed as open: %v", out)
	}
	if code, _ := post(map[string]any{"action": "ignore", "id": 999}); code != http.StatusBadRequest {
		t.Errorf("unknown id = %d, want 400", code)
	}
}
