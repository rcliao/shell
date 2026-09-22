package decide

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestJevAskRoundTrip(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"model":"jev-1.13.0","answers":{"which_project":{"type":"choice","choice":"japan","confidence":0.9,"probabilities":{"japan":0.93,"none":0.07}},"q2":{"type":"noul","noul":0.96}},"usage":{"input_tokens":547,"output_tokens":101}}`))
	}))
	defer srv.Close()
	j := &Jev{httpc: srv.Client(), url: srv.URL, key: func() string { return "k-test" }}

	res, err := j.Ask(context.Background(), map[string]any{"message": "hi"}, map[string]Question{
		"which_project": {Type: "choice", Instructions: "which?", Criteria: map[string]string{"japan": "trip", "none": "no"}},
		"q2":            {Type: "noul", Instructions: "is it?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer k-test" || gotBody["model"] != "jev-latest" {
		t.Errorf("auth/model = %q / %v", gotAuth, gotBody["model"])
	}
	if a := res.Answers["which_project"]; a.Choice != "japan" || a.Confidence != 0.9 || a.Probabilities["japan"] != 0.93 {
		t.Errorf("choice answer = %+v", a)
	}
	if res.Answers["q2"].Noul != 0.96 || res.Usage.InputTokens != 547 || res.Model != "jev-1.13.0" {
		t.Errorf("result = %+v", res)
	}
}

func TestJevErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":"slow down"}`))
	}))
	defer srv.Close()
	j := &Jev{httpc: srv.Client(), url: srv.URL, key: func() string { return "k" }}
	_, err := j.Ask(context.Background(), "s", map[string]Question{"q": {Type: "noul", Instructions: "x"}})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 429 || !ae.Retryable {
		t.Errorf("want retryable 429 APIError, got %v", err)
	}
	if _, err := (&Jev{key: func() string { return "" }}).Ask(context.Background(), "s", nil); err == nil {
		t.Error("no key must be an error, not a request")
	}
}

// fakeDecider records what it was asked and can stall.
type fakeDecider struct {
	mu    sync.Mutex
	state any
	qs    map[string]Question
	stall time.Duration
	err   error
}

func (f *fakeDecider) Enabled() bool { return true }
func (f *fakeDecider) Ask(ctx context.Context, state any, qs map[string]Question) (Result, error) {
	f.mu.Lock()
	f.state, f.qs = state, qs
	f.mu.Unlock()
	if f.stall > 0 {
		select {
		case <-time.After(f.stall):
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	if f.err != nil {
		return Result{}, f.err
	}
	ans := map[string]Answer{}
	for id, q := range qs {
		if q.Type == "choice" {
			ans[id] = Answer{Type: "choice", Choice: "none", Confidence: 0.5, Probabilities: map[string]float64{"none": 0.5}}
		} else {
			ans[id] = Answer{Type: "noul", Noul: 0.2}
		}
	}
	return Result{Model: "fake", Answers: ans, Usage: Usage{InputTokens: 10}, Latency: 3 * time.Millisecond}, nil
}

type memRecorder struct {
	mu   sync.Mutex
	rows []Row
	done chan struct{}
	want int
}

func (m *memRecorder) LogRouterDecision(r Row) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, r)
	if len(m.rows) == m.want {
		close(m.done)
	}
	return nil
}

func waitRows(t *testing.T, m *memRecorder) []Row {
	t.Helper()
	select {
	case <-m.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("recorder got %d rows, want %d", len(m.rows), m.want)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Row(nil), m.rows...)
}

func TestShadowAsksTheRightQuestionsAndRecords(t *testing.T) {
	d := &fakeDecider{}
	rec := &memRecorder{done: make(chan struct{}), want: 4}
	sh := NewShadow(d, rec)
	msg := "[Umbreon (your fellow agent) said this in the group — reply if you have something genuinely useful to add or a part of the task to take; otherwise [noop]]\n我們二月去京都要住哪？"
	sh.Observe(Turn{ChatID: -100200300, ThreadID: 7, ChatKind: "group", Message: msg, BoundProject: "japan",
		Projects: []Project{{Slug: "japan", Title: "Japan 2027", Instructions: "spring trip"}, {Slug: "health", Title: "Health log"}}})

	rows := waitRows(t, rec)
	byQ := map[string]Row{}
	for _, r := range rows {
		byQ[r.Question] = r
	}
	for _, q := range []string{"which_project", "should_reply", "has_open_question", "has_decision"} {
		if _, ok := byQ[q]; !ok {
			t.Errorf("missing row for %q", q)
		}
	}
	wp := byQ["which_project"]
	if !strings.Contains(wp.Candidates, `"japan"`) || !strings.Contains(wp.Candidates, `"none"`) || wp.BoundProject != "japan" || !wp.PeerTurn {
		t.Errorf("which_project row = %+v", wp)
	}
	if wp.Choice != "none" || wp.Model != "fake" || wp.InputTokens != 10 || wp.Error != "" {
		t.Errorf("answer not recorded: %+v", wp)
	}
	// The relay frame is stripped; only the words go out.
	state := d.state.(map[string]any)
	if m := state["message"].(string); strings.Contains(m, "fellow agent") || !strings.HasPrefix(m, "我們") {
		t.Errorf("relay frame leaked into state: %q", m)
	}
	if _, ok := state["transcript"]; ok || len(state) != 3 {
		t.Errorf("state must carry only chat/thread/message: %v", state)
	}
}

func TestShadowSkipsWhatItCannotAsk(t *testing.T) {
	d := &fakeDecider{}
	rec := &memRecorder{done: make(chan struct{}), want: 2}
	NewShadow(d, rec).Observe(Turn{ChatID: 42, ChatKind: "dm", Message: "hello"})
	rows := waitRows(t, rec)
	for _, r := range rows {
		if r.Question == "which_project" || r.Question == "should_reply" {
			t.Errorf("asked %q with no projects and no peer marker", r.Question)
		}
	}
	// nil shadow / nil decider: no panic, nothing recorded.
	var none *Shadow
	none.Observe(Turn{ChatID: 1, Message: "x"})
	NewShadow(nil, rec).Observe(Turn{ChatID: 1, Message: "x"})
}

func TestShadowNeverBlocksAndRecordsFailures(t *testing.T) {
	d := &fakeDecider{stall: time.Minute}
	rec := &memRecorder{done: make(chan struct{}), want: 2}
	sh := NewShadow(d, rec)
	sh.timeout = 50 * time.Millisecond
	started := time.Now()
	sh.Observe(Turn{ChatID: 42, ChatKind: "dm", Message: "hello"})
	if time.Since(started) > 20*time.Millisecond {
		t.Fatal("Observe must return immediately")
	}
	rows := waitRows(t, rec)
	if rows[0].Error == "" || !strings.Contains(rows[0].Error, "deadline") {
		t.Errorf("a timed-out ask must be recorded as an error row: %+v", rows[0])
	}
}
