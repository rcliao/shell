package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/store"
)

func TestHeartbeatAgenda(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b := &Bridge{store: st}
	ctx := context.Background()

	if a := b.HeartbeatAgenda(ctx); !a.Empty() || a.Render() != "" {
		t.Fatalf("quiet agent: agenda = %+v", a)
	}

	// New conversation since the last beat.
	st.SaveSession(42, 0, "c")
	sess, _ := st.GetSession(42, 0)
	st.LogMessage(sess.ID, "user", "can you remind me about the dentist tomorrow?")
	st.LogMessage(sess.ID, "user", "[Heartbeat] synthetic") // never counts
	a := b.HeartbeatAgenda(ctx)
	if len(a.Items) != 1 || a.Items[0].Kind != "conversation" || !strings.Contains(a.Items[0].Text, "chat 42: 1 new message") {
		t.Fatalf("conversation item = %+v", a.Items)
	}
	if !strings.Contains(a.Render(), "reply [noop]") {
		t.Error("the agenda must tell the agent it may do nothing")
	}
	// Not committed (the turn failed): the next beat sees it again.
	if again := b.HeartbeatAgenda(ctx); len(again.Items) != 1 {
		t.Fatalf("an uncommitted agenda must be offered again, got %+v", again.Items)
	}
	b.CommitAgenda(a)
	if a := b.HeartbeatAgenda(ctx); !a.Empty() {
		t.Fatalf("after commit, nothing new since the last beat, got %+v", a.Items)
	}
	// A restart (fresh bridge, same store) keeps the watermark.
	if a := (&Bridge{store: st}).HeartbeatAgenda(ctx); !a.Empty() {
		t.Fatalf("the watermark must survive a restart, got %+v", a.Items)
	}

	// An auto-paused schedule, a tool failing repeatedly, a new event.
	id, _, _ := st.UpsertScheduleByKey(&store.Schedule{ChatID: 0, Label: "self-check", Message: "m", Schedule: "0 9 * * *",
		Type: "cron", Mode: "prompt", NextRunAt: time.Now().Add(time.Hour), Enabled: true, DedupKey: "t1"})
	st.PauseSchedule(id, "missing_chat_id")
	var calls []process.ToolCall
	for i := 0; i < 3; i++ {
		calls = append(calls, process.ToolCall{Name: "Bash", Input: map[string]any{"command": "x"}, Failed: true})
	}
	st.LogToolUses(42, sess.ID, "interactive", toolUseRows(calls))
	evID, _, _ := st.AddEvent(store.Event{Source: "gmail", Kind: "email.received", DedupID: "m1", Summary: "Flight confirmation"})

	a = b.HeartbeatAgenda(ctx)
	kinds := map[string]string{}
	for _, it := range a.Items {
		kinds[it.Kind] = it.Text
	}
	if !strings.Contains(kinds["schedule"], "missing_chat_id") || !strings.Contains(kinds["schedule"], "self-check") {
		t.Errorf("schedule item = %q", kinds["schedule"])
	}
	if !strings.Contains(kinds["tool"], "Bash failed 3 times") {
		t.Errorf("tool item = %q", kinds["tool"])
	}
	if !strings.Contains(kinds["event"], "Flight confirmation") || !strings.Contains(kinds["event"], "shell_event") {
		t.Errorf("event item = %q", kinds["event"])
	}
	b.CommitAgenda(a)
	evs, _ := st.ListEvents(nil, 0)
	if len(evs) != 1 || evs[0].ID != evID || evs[0].Status != store.EventSeen {
		t.Errorf("a committed event is marked seen: %+v", evs)
	}
	shownAgain := false
	for _, it := range b.HeartbeatAgenda(ctx).Items {
		if it.Kind == "event" && strings.Contains(it.Text, "shown before, still open") {
			shownAgain = true
		}
	}
	if !shownAgain {
		t.Error("an event not yet marked done/ignored stays on the agenda")
	}
	// The agent closes the event during the beat; the commit after the turn
	// must not reopen it (live bug: done was overwritten with seen).
	shown := b.HeartbeatAgenda(ctx)
	st.MarkEvent(evID, store.EventDone, "handled")
	b.CommitAgenda(shown)
	if evs, _ := st.ListEvents(nil, 0); evs[0].Status != store.EventDone {
		t.Fatalf("commit overwrote the agent's decision: %s", evs[0].Status)
	}
	for _, it := range b.HeartbeatAgenda(ctx).Items {
		if it.Kind == "event" {
			t.Error("a done event leaves the agenda")
		}
	}
}

// A dated open question close at hand reaches the agenda once, is repeated
// only after agendaDueRepeat, and only the doc's owner gets a shared one.
func TestAgendaDeadlines(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ws := t.TempDir()
	soon := time.Now().Add(3 * 24 * time.Hour).Format("2006-01-02")
	far := time.Now().Add(40 * 24 * time.Hour).Format("2006-01-02")
	doc := "# Trip\n\n## 待決定\n\n1. Can the trip run into April? (by " + soon + ")\n2. Budget (by " + far + ")\n3. ~~Hotel (by " + soon + ")~~\n"
	os.MkdirAll(filepath.Join(ws, "projects", "trip"), 0o755)
	os.WriteFile(filepath.Join(ws, "projects", "trip", "doc.md"), []byte(doc), 0o644)
	st.CreateProject(store.Project{Slug: "trip", Title: "Trip", ChatID: -100200300, MessageThreadID: 900000000000000011, DocPath: "projects/trip/doc.md"})
	b := &Bridge{store: st, workspaceDir: ws, agentName: "me"}

	a := b.HeartbeatAgenda(context.Background())
	var got []string
	for _, it := range a.Items {
		if it.Kind == "deadline" {
			got = append(got, it.Text)
		}
	}
	if len(got) != 1 || !strings.Contains(got[0], "run into April") || !strings.Contains(got[0], "<#900000000000000011>") {
		t.Fatalf("deadline items = %v", got)
	}
	b.CommitAgenda(a)
	for _, it := range b.HeartbeatAgenda(context.Background()).Items {
		if it.Kind == "deadline" {
			t.Fatal("repeated within agendaDueRepeat")
		}
	}
	// Another agent owns the shared doc: no deadline items here.
	os.WriteFile(filepath.Join(ws, "projects", "trip", "owner"), []byte("other\nresearch\n"), 0o644)
	st2, _ := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	defer st2.Close()
	st2.CreateProject(store.Project{Slug: "trip", Title: "Trip", ChatID: -100200300, DocPath: "projects/trip/doc.md"})
	b2 := &Bridge{store: st2, workspaceDir: ws, agentName: "me"}
	for _, it := range b2.HeartbeatAgenda(context.Background()).Items {
		if it.Kind == "deadline" {
			t.Fatal("non-owner got a shared doc's deadline")
		}
	}
}

// Due today reads "today", not "overdue"; yesterday is 1 day overdue.
func TestAgendaDeadlineDays(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	defer st.Close()
	ws := t.TempDir()
	today := time.Now().Format("2006-01-02")
	yesterday := time.Now().Add(-24 * time.Hour).Format("2006-01-02")
	doc := "## 待決定\n\n1. A (by " + today + ")\n2. B (by " + yesterday + ")\n"
	os.MkdirAll(filepath.Join(ws, "projects", "p"), 0o755)
	os.WriteFile(filepath.Join(ws, "projects", "p", "doc.md"), []byte(doc), 0o644)
	st.CreateProject(store.Project{Slug: "p", Title: "P", ChatID: 42, DocPath: "projects/p/doc.md"})
	b := &Bridge{store: st, workspaceDir: ws}
	var texts []string
	for _, it := range b.HeartbeatAgenda(context.Background()).Items {
		if it.Kind == "deadline" {
			texts = append(texts, it.Text)
		}
	}
	all := strings.Join(texts, " | ")
	if !strings.Contains(all, "(today)") || !strings.Contains(all, "(1 days overdue)") {
		t.Fatalf("items = %v", texts)
	}
}
