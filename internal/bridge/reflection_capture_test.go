package bridge

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/beat"
	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/store"
)

func captureBridge(t *testing.T) (*Bridge, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Bridge{store: st}, st
}

// End-to-end capture without waiting on a real deep beat. Deep beats fire every
// 6th heartbeat — hours apart — so relying on production to exercise this path
// means a wiring bug sits undetected until the next one, and a deploy landing
// mid-beat kills the test (observed 2026-08-01: a triggered deep beat was
// killed by a redeploy 3.7 minutes in, capturing nothing).
func TestCaptureReflectionWritesTheJournalRow(t *testing.T) {
	b, st := captureBridge(t)

	ctx := beat.With(context.Background(), beat.Meta{RunID: 77, Count: 210, Deep: true})
	result := process.SendResult{
		ToolCalls: []process.ToolCall{{Name: "ghost_put"}, {Name: "Read"}},
		Timings:   process.Timings{TotalMs: 154_000},
	}
	b.captureReflection(ctx, 0, "I over-explained twice yesterday when short answers were wanted.", result, "claude-opus-5")

	refs, err := st.ListReflections(10)
	if err != nil || len(refs) != 1 {
		t.Fatalf("want exactly one journal row, got %d (err %v)", len(refs), err)
	}
	got := refs[0]
	if got.JobRunID != 77 || got.BeatCount != 210 {
		t.Errorf("ledger linkage lost: job_run=%d beat=%d, want 77/210", got.JobRunID, got.BeatCount)
	}
	if got.ToolCalls != 2 {
		t.Errorf("tool_calls = %d, want 2", got.ToolCalls)
	}
	if got.DurationMS != 154_000 || got.Model != "claude-opus-5" {
		t.Errorf("duration/model lost: %d %q", got.DurationMS, got.Model)
	}
	if got.Noop {
		t.Error("a reflection with text must not be flagged noop")
	}
}

// A beat that produced nothing must still leave a row: a missing row and a
// silent beat would otherwise be indistinguishable.
func TestCaptureReflectionRecordsSilentBeat(t *testing.T) {
	b, st := captureBridge(t)

	b.captureReflection(beat.With(context.Background(), beat.Meta{RunID: 5, Count: 12, Deep: true}),
		0, "", process.SendResult{}, "claude-opus-5")

	refs, _ := st.ListReflections(10)
	if len(refs) != 1 || !refs[0].Noop {
		t.Fatalf("a silent deep beat must be recorded as noop, got %+v", refs)
	}
}

// Capture must never be able to fail a heartbeat. A journal that can break the
// thing it observes is worse than no journal.
func TestCaptureReflectionSurvivesStoreFailure(t *testing.T) {
	b, st := captureBridge(t)
	st.Close() // every write from here on errors

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.captureReflection(context.Background(), 0, "text", process.SendResult{}, "m")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("capture blocked on a failing store")
	}
}

// Metadata is absent for a manually invoked beat; the row is still captured,
// just unlinked.
func TestCaptureReflectionWithoutMetadata(t *testing.T) {
	b, st := captureBridge(t)

	b.captureReflection(context.Background(), 0, "manual", process.SendResult{}, "m")

	refs, _ := st.ListReflections(10)
	if len(refs) != 1 {
		t.Fatalf("want the row captured even unlinked, got %d", len(refs))
	}
	if refs[0].JobRunID != 0 || refs[0].BeatCount != 0 {
		t.Errorf("unlinked row should carry zeros, got job_run=%d beat=%d",
			refs[0].JobRunID, refs[0].BeatCount)
	}
}
