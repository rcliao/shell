package bridge

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/store"
)

func drainTestBridge(t *testing.T) (*Bridge, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Bridge{store: st}, st
}

// The regression this exists for. On 2026-08-01 drain reported idle while a
// computed turn was still undelivered, the daemon exec'd, and the reply was
// replayed 39s later in front of the family. turnWG being empty is not enough.
func TestWaitIdleBlocksOnUndeliveredReply(t *testing.T) {
	b, st := drainTestBridge(t)

	if _, err := st.BeginPendingTurn(42, 0, 1001, "someone", "are you there?"); err != nil {
		t.Fatalf("begin: %v", err)
	}

	start := time.Now()
	if b.WaitIdle(300 * time.Millisecond) {
		t.Fatal("WaitIdle reported drained while a received turn was still undelivered")
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Errorf("returned after %s — it should have waited out the timeout, not short-circuited", elapsed)
	}
}

// Once the reply is delivered, drain proceeds immediately.
func TestWaitIdleProceedsAfterDelivery(t *testing.T) {
	b, st := drainTestBridge(t)

	if _, err := st.BeginPendingTurn(42, 0, 1002, "someone", "hello"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := st.CompletePendingTurn(42, 1002); err != nil {
		t.Fatalf("complete: %v", err)
	}

	start := time.Now()
	if !b.WaitIdle(2 * time.Second) {
		t.Fatal("WaitIdle should drain once every received turn is delivered")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s to notice an already-delivered turn", elapsed)
	}
}

// A delivery that completes mid-drain releases the barrier — the normal case,
// where drain waits out a turn that is finishing rather than timing out.
func TestWaitIdleReleasesWhenDeliveryLands(t *testing.T) {
	b, st := drainTestBridge(t)

	if _, err := st.BeginPendingTurn(42, 0, 1003, "someone", "one moment"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		if err := st.CompletePendingTurn(42, 1003); err != nil {
			t.Errorf("complete: %v", err)
		}
	}()

	if !b.WaitIdle(5 * time.Second) {
		t.Fatal("WaitIdle should release once the in-flight reply is delivered")
	}
}

// A ledger read failure must never wedge a restart.
func TestWaitIdleProceedsWithoutStore(t *testing.T) {
	b := &Bridge{}
	if !b.WaitIdle(time.Second) {
		t.Fatal("a bridge with no store must drain immediately")
	}
}
