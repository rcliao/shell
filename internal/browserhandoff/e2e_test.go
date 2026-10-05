package browserhandoff

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	shellbrowser "github.com/rcliao/shell-browser"
	"github.com/rcliao/shell/internal/store"
)

// TestE2ERealChromeAndTailscale drives the real pieces end to end: a Chrome
// session, the live view, tailscale serve and the store. It opens a window on
// the host and adds (then removes) a tailnet path, so it only runs on request:
//
//	SHELL_E2E_HANDOFF=1 go test ./internal/browserhandoff -run E2E -v -timeout 5m
func TestE2ERealChromeAndTailscale(t *testing.T) {
	if os.Getenv("SHELL_E2E_HANDOFF") != "1" {
		t.Skip("set SHELL_E2E_HANDOFF=1 to run (opens Chrome, touches tailscale serve)")
	}
	// Not t.TempDir: Chrome keeps writing its profile for a moment after it
	// is told to close, which fails TempDir's strict cleanup.
	root, err := os.MkdirTemp("", "handoff-e2e-sessions-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	ctx := context.Background()

	// 1. The agent drives a session (as the browser skill would).
	res := shellbrowser.Execute(ctx, shellbrowser.Config{Enabled: true, Session: "e2e", SessionRoot: root, TimeoutSeconds: 60},
		shellbrowser.ParseDirective("https://example.com", ""))
	for _, s := range res.Steps {
		if s.Err != nil {
			t.Fatalf("drive session: %v", s.Err)
		}
	}

	f, _ := os.CreateTemp("", "handoff-e2e-*.db")
	f.Close()
	st, err := store.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer st.Close()

	ts := &Tailscale{}
	resumes := make(chan string, 2)
	m := New(Config{
		AgentName: "e2e", Store: st, Publisher: ts,
		Sessions: &ChromeSessions{Root: root},
		Notify:   func(int64, int64, string, string, string) error { return nil },
		Resume:   func(_, _ int64, p string) { resumes <- p },
	})
	defer m.Shutdown()

	// 2. Handoff: the link must answer on the tailnet, and the skill must be held.
	h, err := m.Open(ctx, OpenRequest{Session: "e2e", Reason: "e2e check", ChatID: 42, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("link %s", h.Link)
	get := func(url string) (int, string) {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(h.Link); code != 200 || !strings.Contains(body, "Done, hand it back") {
		t.Fatalf("link: %d", code)
	}
	host, _ := ts.Host(ctx)
	if code, _ := get("https://" + host + "/h/00000000000000000000000000000000/"); code != 404 {
		t.Fatalf("unknown token: %d, want 404", code)
	}
	if out := serveStatus(t); !strings.Contains(out, h.Path) || !strings.Contains(out, "tailnet only") {
		t.Fatalf("serve status lacks %s (tailnet only):\n%s", h.Path, out)
	}
	held := shellbrowser.Execute(ctx, shellbrowser.Config{Enabled: true, Session: "e2e", SessionRoot: root},
		shellbrowser.ParseDirective("-", "text"))
	if len(held.Steps) == 0 || held.Steps[0].Err == nil || !strings.Contains(held.Steps[0].Err.Error(), "held by a human") {
		t.Fatalf("skill not held during handoff: %+v", held.Steps)
	}

	// 3. Done through the published link, as the phone would.
	req, _ := http.NewRequest(http.MethodPost, h.Link+"done", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("done: %v %v", resp, err)
	}
	resp.Body.Close()
	select {
	case p := <-resumes:
		if !strings.Contains(p, "done") || !strings.Contains(p, "example.com") {
			t.Fatalf("resume prompt: %s", p)
		}
		t.Logf("resume: %s", p)
	case <-time.After(30 * time.Second):
		t.Fatal("no resume after Done")
	}
	if out := serveStatus(t); strings.Contains(out, h.Path) {
		t.Fatalf("path still served after Done:\n%s", out)
	}
	row, _ := st.GetBrowserHandoff(h.ID)
	if row.Status != store.HandoffDone {
		t.Fatalf("status %s", row.Status)
	}
	after := shellbrowser.Execute(ctx, shellbrowser.Config{Enabled: true, Session: "e2e", SessionRoot: root},
		shellbrowser.ParseDirective("-", "text"))
	if len(after.Steps) < 2 || after.Steps[1].Err != nil || !strings.Contains(after.Steps[1].Output, "documentation examples") {
		t.Fatalf("agent could not resume on the same tab: %+v", after.Steps)
	}

	// 4. Expiry: a 1-minute handoff left alone ends as expired, path gone, Chrome alive.
	h2, err := m.Open(ctx, OpenRequest{Session: "e2e", ChatID: 42, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-resumes:
		if !strings.Contains(p, "expired") {
			t.Fatalf("expiry resume: %s", p)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("no expiry")
	}
	if out := serveStatus(t); strings.Contains(out, h2.Path) {
		t.Fatalf("path still served after expiry:\n%s", out)
	}
	if row, _ := st.GetBrowserHandoff(h2.ID); row.Status != store.HandoffExpired {
		t.Fatalf("status %s", row.Status)
	}
	s, _ := (&ChromeSessions{Root: root}).open("e2e")
	if s.Running(ctx) == nil {
		t.Fatal("session Chrome died with the handoff")
	}
	if closed := (&ChromeSessions{Root: root}).ReapIdle(ctx, 0, nil); len(closed) != 1 {
		t.Fatalf("reaper closed %v, want [e2e]", closed)
	}
	for i := 0; i < 40 && s.Running(ctx) != nil; i++ {
		time.Sleep(250 * time.Millisecond)
	}
	if s.Running(ctx) != nil {
		t.Fatal("session Chrome still running 10s after the reaper closed it")
	}
}

func serveStatus(t *testing.T) string {
	t.Helper()
	out, _ := exec.Command("tailscale", "serve", "status").CombinedOutput()
	return string(out)
}
