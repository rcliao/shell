package bridge

import (
	"context"
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
	if a := b.HeartbeatAgenda(ctx); !a.Empty() {
		t.Fatalf("the window advances: nothing new since the last beat, got %+v", a.Items)
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
	evs, _ := st.ListEvents(nil, 0)
	if len(evs) != 1 || evs[0].ID != evID || evs[0].Status != store.EventSeen {
		t.Errorf("a shown event is marked seen: %+v", evs)
	}
	for _, it := range b.HeartbeatAgenda(ctx).Items {
		if it.Kind == "event" {
			t.Error("a seen event is not shown again")
		}
	}
}
