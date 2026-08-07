package bridge

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/store"
	_ "modernc.org/sqlite"
)

func digestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := t.TempDir() + "/test.db"
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

// Silence when there is nothing owed and nothing broken. A digest that always
// prints trains the reader to skip it, which is how the queue became invisible
// in the first place.
func TestQueueDigestIsSilentWhenThereIsNothingToSay(t *testing.T) {
	st, _ := digestStore(t)
	b := &Bridge{store: st}
	if got := b.queueDigest(); got != "" {
		t.Fatalf("digest on an empty queue = %q, want empty", got)
	}
}

// Work an agent registered for later must be visible later — that is the only
// reason to have registered it.
func TestQueueDigestShowsWorkTheAgentStillOwes(t *testing.T) {
	st, _ := digestStore(t)
	b := &Bridge{store: st}

	if _, _, err := st.EnqueueTask(store.Task{
		Kind:           agentTaskKind,
		IdempotencyKey: "agent:1",
		Payload:        `{"title":"check the avocado tree","prompt":"look at the fruit"}`,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	got := b.queueDigest()
	if !strings.Contains(got, "check the avocado tree") {
		t.Fatalf("digest did not name the owed work:\n%s", got)
	}
	if !strings.Contains(got, "Task 1") {
		t.Fatalf("digest omitted the id, so it cannot be completed:\n%s", got)
	}
}

// Machinery the agent does not drive must stay out. A person waiting on a reply
// is the daemon's problem, and listing it invites the agent to answer a message
// that is already being answered.
func TestQueueDigestIgnoresMessageTurnsAndFires(t *testing.T) {
	st, _ := digestStore(t)
	b := &Bridge{store: st}

	for _, k := range []string{"message.turn", "schedule.fire"} {
		if _, _, err := st.EnqueueTask(store.Task{
			Kind:           k,
			IdempotencyKey: k + ":1",
			Payload:        `{"text":"hello"}`,
		}); err != nil {
			t.Fatalf("enqueue %s: %v", k, err)
		}
	}
	if got := b.queueDigest(); got != "" {
		t.Fatalf("digest surfaced daemon-driven work:\n%s", got)
	}
}

// A failure is the thing most worth seeing: it is work that will not happen
// unless the agent does something. It must carry its reason.
func TestQueueDigestShowsRecentFailuresWithTheirReason(t *testing.T) {
	st, _ := digestStore(t)
	b := &Bridge{store: st}

	id, _, err := st.EnqueueTask(store.Task{
		Kind:           agentTaskKind,
		IdempotencyKey: "agent:2",
		Payload:        `{"title":"water the garden"}`,
		MaxAttempts:    1,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.FailTaskPermanent(id, "the watering script is gone"); err != nil {
		t.Fatalf("fail: %v", err)
	}

	got := b.queueDigest()
	if !strings.Contains(got, "water the garden") {
		t.Fatalf("digest omitted the failed work:\n%s", got)
	}
	if !strings.Contains(got, "the watering script is gone") {
		t.Fatalf("digest omitted why it failed, leaving nothing to act on:\n%s", got)
	}
}

// Old failures are not news. Without a cutoff the digest accumulates every
// failure the agent has ever had and stops being readable.
func TestQueueDigestDropsStaleFailures(t *testing.T) {
	st, dbPath := digestStore(t)
	b := &Bridge{store: st}

	id, _, err := st.EnqueueTask(store.Task{
		Kind:           agentTaskKind,
		IdempotencyKey: "agent:3",
		Payload:        `{"title":"ancient thing"}`,
		MaxAttempts:    1,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.FailTaskPermanent(id, "long ago"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	// Age it past the lookback window.
	old := time.Now().Add(-failureLookback - time.Hour).UTC()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE tasks SET done_at = ? WHERE id = ?`, old, id); err != nil {
		t.Fatalf("age: %v", err)
	}

	if got := b.queueDigest(); got != "" {
		t.Fatalf("digest still reported a failure older than its window:\n%s", got)
	}
}
