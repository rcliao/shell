package browserhandoff

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/store"
)

type fakeView struct {
	url    string
	closed chan string
}

func (v *fakeView) ServeHTTP(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }
func (v *fakeView) URL() string                                      { return v.url }
func (v *fakeView) Close(msg string)                                 { v.closed <- msg }

type fakeSessions struct {
	mu      sync.Mutex
	views   []*fakeView
	specs   []ViewSpec
	held    map[string]int64
	touched map[string]int
	failNew error
}

func (f *fakeSessions) NewView(_ context.Context, spec ViewSpec) (View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNew != nil {
		return nil, f.failNew
	}
	v := &fakeView{url: "https://shop.example/checkout", closed: make(chan string, 1)}
	f.views = append(f.views, v)
	f.specs = append(f.specs, spec)
	return v, nil
}
func (f *fakeSessions) Hold(s string, id int64, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[s] = id
	return nil
}
func (f *fakeSessions) Release(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.held, s)
}
func (f *fakeSessions) Touch(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched[s]++
}
func (f *fakeSessions) ReapIdle(context.Context, time.Duration, map[string]bool) []string { return nil }
func (f *fakeSessions) isHeld(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.held[s]
	return ok
}

type fakePublisher struct {
	mu        sync.Mutex
	published map[string]int
	fail      error
	delay     time.Duration
}

func (p *fakePublisher) Publish(_ context.Context, path string, port int) (string, error) {
	time.Sleep(p.delay)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return "", p.fail
	}
	p.published[path] = port
	return "https://mini.example.ts.net" + path + "/", nil
}
func (p *fakePublisher) Unpublish(_ context.Context, path string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.published, path)
	return nil
}
func (p *fakePublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.published)
}

type resumeCall struct {
	chat, thread int64
	prompt       string
}

type harness struct {
	m       *Manager
	st      *store.Store
	sess    *fakeSessions
	pub     *fakePublisher
	resumes chan resumeCall
	posts   chan string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	f, _ := os.CreateTemp("", "handoff-test-*.db")
	f.Close()
	st, err := store.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close(); os.Remove(f.Name()) })
	h := &harness{st: st, sess: &fakeSessions{held: map[string]int64{}, touched: map[string]int{}},
		pub: &fakePublisher{published: map[string]int{}}, resumes: make(chan resumeCall, 4), posts: make(chan string, 4)}
	h.m = New(Config{
		AgentName: "agent", Store: st, Publisher: h.pub, Sessions: h.sess,
		Notify: func(chat, thread int64, text, label, link string) error {
			h.posts <- text + " | " + label + " | " + link
			return nil
		},
		Resume: func(chat, thread int64, prompt string) { h.resumes <- resumeCall{chat, thread, prompt} },
	})
	t.Cleanup(h.m.Shutdown)
	return h
}

func waitResume(t *testing.T, h *harness) resumeCall {
	t.Helper()
	select {
	case r := <-h.resumes:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("no resume turn")
	}
	return resumeCall{}
}

func TestHandoffDoneResumesSameChatAndThread(t *testing.T) {
	h := newHarness(t)
	ho, err := h.m.Open(context.Background(), OpenRequest{Session: "shop", Reason: "solve the captcha",
		Message: "need you", ChatID: -100200300, ThreadID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ho.Link, "https://mini.example.ts.net/h/") || !strings.HasSuffix(ho.Link, "/") {
		t.Fatalf("link = %q", ho.Link)
	}
	if len(strings.TrimPrefix(ho.Path, "/h/")) != 32 {
		t.Fatalf("token too short: %q", ho.Path)
	}
	if post := <-h.posts; !strings.Contains(post, "need you | Open the browser | "+ho.Link) {
		t.Fatalf("post = %q", post)
	}
	if !h.sess.isHeld("shop") {
		t.Fatal("tab not held during handoff")
	}

	// A second view of the same session is refused and names the open one.
	if _, err := h.m.Open(context.Background(), OpenRequest{Session: "shop", ChatID: 42}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second open err = %v; want ErrBusy", err)
	}

	h.sess.specs[0].OnDone("https://shop.example/paid", "Someone", "take the blue one")
	r := waitResume(t, h)
	if r.chat != -100200300 || r.thread != 7 {
		t.Fatalf("resume went to %d/%d", r.chat, r.thread)
	}
	for _, want := range []string{"#1 done", "Someone", "https://shop.example/paid", "--session shop -", "solve the captcha", `"take the blue one"`, "Decide what follows"} {
		if !strings.Contains(r.prompt, want) {
			t.Errorf("resume prompt lacks %q: %s", want, r.prompt)
		}
	}
	if !strings.HasPrefix(r.prompt, "[") {
		t.Error("resume prompt must start with '[' (system-originated turn)")
	}
	if msg := <-h.sess.views[0].closed; !strings.Contains(msg, "Thanks") {
		t.Fatalf("view close message = %q", msg)
	}
	if h.pub.count() != 0 || h.sess.isHeld("shop") {
		t.Fatal("path still published or tab still held after Done")
	}
	row, _ := h.st.GetBrowserHandoff(ho.ID)
	if row.Status != store.HandoffDone || row.EndedBy != "Someone" || row.FinalURL != "https://shop.example/paid" || row.Note != "take the blue one" {
		t.Fatalf("row = %+v", row)
	}

	// Done again (double tap) and expiry after Done are no-ops.
	h.sess.specs[0].OnDone("https://x/", "Other", "")
	h.m.finish(ho.ID, ending{status: store.HandoffExpired})
	select {
	case extra := <-h.resumes:
		t.Fatalf("second resume: %s", extra.prompt)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestHandoffExpiryResumesAsExpired(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.m.cfg.Now = func() time.Time { return now }
	ho, err := h.m.Open(context.Background(), OpenRequest{Session: "s", ChatID: 42, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	<-h.posts
	h.m.finish(ho.ID, ending{status: store.HandoffExpired}) // what the timer does
	r := waitResume(t, h)
	if !strings.Contains(r.prompt, "expired") || !strings.Contains(r.prompt, "https://shop.example/checkout") {
		t.Fatalf("prompt = %s", r.prompt)
	}
}

func TestWatchModeNeverHoldsOrResumes(t *testing.T) {
	h := newHarness(t)
	ho, err := h.m.Open(context.Background(), OpenRequest{Session: "s", Mode: store.HandoffModeWatch, ChatID: 42})
	if err != nil {
		t.Fatal(err)
	}
	if post := <-h.posts; !strings.Contains(post, "Watch live") {
		t.Fatalf("post = %q", post)
	}
	if h.sess.isHeld("s") {
		t.Fatal("watch mode held the tab")
	}
	if err := h.m.Cancel(ho.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Cancel(ho.ID); err == nil {
		t.Fatal("cancelling a finished view succeeded")
	}
	select {
	case r := <-h.resumes:
		t.Fatalf("watch view resumed the agent: %s", r.prompt)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestOpenFailuresLeaveNothingBehind(t *testing.T) {
	h := newHarness(t)
	h.pub.fail = ErrServeDisabled
	if _, err := h.m.Open(context.Background(), OpenRequest{Session: "s", ChatID: 42}); !errors.Is(err, ErrServeDisabled) {
		t.Fatalf("err = %v", err)
	}
	if msg := <-h.sess.views[0].closed; msg != "" {
		t.Fatalf("view not closed cleanly: %q", msg)
	}
	if open, _ := h.st.OpenBrowserHandoffs(); len(open) != 0 || h.sess.isHeld("s") {
		t.Fatalf("left open rows %v or a hold", open)
	}
	if _, err := h.m.Open(context.Background(), OpenRequest{Session: "s"}); err == nil {
		t.Fatal("open without a chat succeeded")
	}
	if _, err := h.m.Open(context.Background(), OpenRequest{Session: "s", ChatID: 42, Mode: "drive"}); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func TestRecoverReservesSamePathOrExpires(t *testing.T) {
	h := newHarness(t)
	future := time.Now().Add(5 * time.Minute)
	liveID, _ := h.st.AddBrowserHandoff(store.BrowserHandoff{Session: "a", Mode: store.HandoffModeHandoff, ChatID: 42, Path: "/h/aaaa", ExpiresAt: future})
	staleID, _ := h.st.AddBrowserHandoff(store.BrowserHandoff{Session: "b", Mode: store.HandoffModeHandoff, ChatID: 42, ThreadID: 9, Path: "/h/bbbb", ExpiresAt: time.Now().Add(-time.Minute)})

	h.m.Recover(context.Background())

	if _, ok := h.pub.published["/h/aaaa"]; !ok {
		t.Fatal("live handoff not re-published at its original path")
	}
	if !h.sess.isHeld("a") {
		t.Fatal("recovered handoff not held")
	}
	r := waitResume(t, h)
	if r.thread != 9 || !strings.Contains(r.prompt, "expired") {
		t.Fatalf("stale resume = %+v", r)
	}
	if row, _ := h.st.GetBrowserHandoff(staleID); row.Status != store.HandoffExpired {
		t.Fatalf("stale row = %s", row.Status)
	}
	if row, _ := h.st.GetBrowserHandoff(liveID); row.Status != store.HandoffOpen {
		t.Fatalf("live row = %s", row.Status)
	}
	select {
	case p := <-h.posts:
		t.Fatalf("recover re-posted the link: %s", p)
	default:
	}
}

func TestRecoverFailureResumesWithReason(t *testing.T) {
	h := newHarness(t)
	h.st.AddBrowserHandoff(store.BrowserHandoff{Session: "gone", Mode: store.HandoffModeHandoff, ChatID: 42, Path: "/h/cccc", ExpiresAt: time.Now().Add(time.Minute)})
	h.sess.failNew = errors.New("no browser running")
	h.m.Recover(context.Background())
	r := waitResume(t, h)
	if !strings.Contains(r.prompt, "no browser running") || !strings.Contains(r.prompt, "stopped") {
		t.Fatalf("prompt = %s", r.prompt)
	}
}

func TestFinishTouchesSessionForReaper(t *testing.T) {
	h := newHarness(t)
	ho, err := h.m.Open(context.Background(), OpenRequest{Session: "s", ChatID: 42})
	if err != nil {
		t.Fatal(err)
	}
	<-h.posts
	h.m.finish(ho.ID, ending{status: store.HandoffDone})
	h.sess.mu.Lock()
	n := h.sess.touched["s"]
	h.sess.mu.Unlock()
	if n == 0 {
		t.Fatal("handoff end did not touch the session: the idle reaper would close it before the agent resumes")
	}
}

func TestConcurrentOpenSameSessionOnlyOneWins(t *testing.T) {
	h := newHarness(t)
	h.pub.delay = 200 * time.Millisecond // publish is slow in real life (tailscale serve)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := h.m.Open(context.Background(), OpenRequest{Session: "s", ChatID: 42})
			errs <- err
		}()
	}
	var ok, busy int
	for i := 0; i < 2; i++ {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, ErrBusy):
			busy++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || busy != 1 {
		t.Fatalf("ok=%d busy=%d; want exactly one view of the session", ok, busy)
	}
	if n := len(h.m.Active()); n != 1 {
		t.Fatalf("%d active views", n)
	}
}

func TestRecoverRowExpiringDuringPublishIsCleanedUp(t *testing.T) {
	h := newHarness(t)
	h.pub.delay = 300 * time.Millisecond
	// Expires while Publish is still running → the timer fires at once.
	id, _ := h.st.AddBrowserHandoff(store.BrowserHandoff{Session: "s", Mode: store.HandoffModeHandoff, ChatID: 42,
		Path: "/h/dddd", ExpiresAt: time.Now().Add(100 * time.Millisecond)})
	h.m.Recover(context.Background())
	r := waitResume(t, h)
	if !strings.Contains(r.prompt, "expired") {
		t.Fatalf("prompt = %s", r.prompt)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(h.m.Active()) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(h.m.Active()); n != 0 {
		t.Fatalf("%d stuck active views after expiry", n)
	}
	if row, _ := h.st.GetBrowserHandoff(id); row.Status != store.HandoffExpired {
		t.Fatalf("status %s", row.Status)
	}
	if _, err := h.m.Open(context.Background(), OpenRequest{Session: "s", ChatID: 42}); err != nil {
		t.Fatalf("session still blocked: %v", err)
	}
}

func TestResumePromptDoesNotAssumeABlockedStep(t *testing.T) {
	h := store.BrowserHandoff{ID: 4, Session: "gifts", Status: store.HandoffDone, FinalURL: "https://shop.example/cart",
		Reason: "here are three options, pick one"}
	p := ResumePrompt(h, "")
	for _, want := range []string{"The person tapped Done", "left no note", "pick one", "or nothing"} {
		if !strings.Contains(p, want) {
			t.Errorf("done prompt lacks %q: %s", want, p)
		}
	}
	h.Status = store.HandoffExpired
	if p := ResumePrompt(h, ""); strings.Contains(p, "blocked") || !strings.Contains(p, "Decide whether a follow-up") {
		t.Errorf("expired prompt frames a blocked step: %s", p)
	}
}
