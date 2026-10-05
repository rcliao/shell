package store

import (
	"testing"
	"time"
)

func TestBrowserHandoffLifecycle(t *testing.T) {
	s, done := newTestStore(t)
	defer done()

	exp := time.Now().Add(10 * time.Minute)
	if _, err := s.AddBrowserHandoff(BrowserHandoff{Session: "x", Mode: "bogus", ChatID: 42, Path: "/h/a", ExpiresAt: exp}); err == nil {
		t.Fatal("unknown mode accepted")
	}
	if _, err := s.AddBrowserHandoff(BrowserHandoff{Session: "x", Mode: HandoffModeHandoff, Path: "/h/a", ExpiresAt: exp}); err == nil {
		t.Fatal("missing chat accepted")
	}

	id, err := s.AddBrowserHandoff(BrowserHandoff{Session: "trip", Mode: HandoffModeHandoff, ChatID: -100200300, ThreadID: 7,
		Reason: "captcha", Path: "/h/tok1", ExpiresAt: exp})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddBrowserHandoff(BrowserHandoff{Session: "trip", Mode: HandoffModeWatch, ChatID: 42, Path: "/h/tok1", ExpiresAt: exp}); err == nil {
		t.Fatal("duplicate path accepted")
	}
	if err := s.SetBrowserHandoffLink(id, "https://mini.example.ts.net/h/tok1/"); err != nil {
		t.Fatal(err)
	}

	open, err := s.OpenBrowserHandoffs()
	if err != nil || len(open) != 1 {
		t.Fatalf("open = %v, %v", open, err)
	}
	h := open[0]
	if h.ID != id || h.Session != "trip" || h.ChatID != -100200300 || h.ThreadID != 7 || h.Link == "" || h.Status != HandoffOpen {
		t.Fatalf("row = %+v", h)
	}
	if d := h.ExpiresAt.Sub(exp); d > time.Second || d < -time.Second {
		t.Fatalf("expires_at round-trip off by %v", d)
	}

	if _, err := s.EndBrowserHandoff(id, HandoffOpen, "", ""); err == nil {
		t.Fatal("ending into a non-final status accepted")
	}
	ended, err := s.EndBrowserHandoff(id, HandoffDone, "https://a.example/", "Someone")
	if err != nil || !ended {
		t.Fatalf("first end = %v, %v", ended, err)
	}
	// Expiry firing after Done must not end it again.
	if again, _ := s.EndBrowserHandoff(id, HandoffExpired, "", ""); again {
		t.Fatal("ended twice")
	}
	got, _ := s.GetBrowserHandoff(id)
	if got == nil || got.Status != HandoffDone || got.FinalURL != "https://a.example/" || got.EndedBy != "Someone" || got.EndedAt.IsZero() {
		t.Fatalf("after end = %+v", got)
	}
	if open, _ := s.OpenBrowserHandoffs(); len(open) != 0 {
		t.Fatalf("still open: %v", open)
	}
	if missing, err := s.GetBrowserHandoff(999); missing != nil || err != nil {
		t.Fatalf("missing = %v, %v", missing, err)
	}
	if recent, _ := s.RecentBrowserHandoffs(5); len(recent) != 1 {
		t.Fatalf("recent = %d rows", len(recent))
	}
}
