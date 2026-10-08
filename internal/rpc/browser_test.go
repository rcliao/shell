package rpc

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/browserhandoff"
	"github.com/rcliao/shell/internal/store"
)

func browserCall(s *Server, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handleBrowser(w, httptest.NewRequest(http.MethodPost, "/browser", bytes.NewReader([]byte(body))))
	return w
}

func TestBrowserDisabledSays503(t *testing.T) {
	w := browserCall(&Server{}, `{"action":"handoff"}`)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "browser") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestBrowserBadRequests(t *testing.T) {
	s := &Server{browserMgr: browserhandoff.New(browserhandoff.Config{})}
	for body, want := range map[string]int{
		`{"action":"drive"}`:  http.StatusBadRequest,
		`{"action":"cancel"}`: http.StatusBadRequest, // no id
		`not json`:            http.StatusBadRequest,
	} {
		if w := browserCall(s, body); w.Code != want {
			t.Errorf("%s: got %d want %d (%s)", body, w.Code, want, w.Body.String())
		}
	}
	if w := browserCall(s, `{"action":"status"}`); w.Code != 200 || !strings.Contains(w.Body.String(), "no open live views") {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
}

func TestOpenedTextTellsAgentToStop(t *testing.T) {
	h := &store.BrowserHandoff{ID: 3, Session: "shop", Mode: store.HandoffModeHandoff, Link: "https://m.example/h/x/", ExpiresAt: time.Now()}
	got := openedText(h)
	for _, want := range []string{"#3", "https://m.example/h/x/", "End your turn", `"[Browser handoff #3`} {
		if !strings.Contains(got, want) {
			t.Errorf("handoff text lacks %q: %s", want, got)
		}
	}
	h.Mode = store.HandoffModeWatch
	if got := openedText(h); !strings.Contains(got, "Keep working") || !strings.Contains(got, `action="cancel", id=3`) {
		t.Errorf("watch text: %s", got)
	}
}
