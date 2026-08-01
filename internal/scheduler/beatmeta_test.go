package scheduler

import (
	"context"
	"testing"

	"github.com/rcliao/shell/internal/beat"
)

// The scheduler must attach fire metadata to the context the heartbeat handler
// receives. Without it the reflection journal records zeros for job_run_id and
// beat_count, and a journal row can only be tied to its fire by timestamp
// proximity — which is exactly what this replaced.
func TestHeartbeatContextCarriesFireMetadata(t *testing.T) {
	st := newMockStore(nil)
	s := New(st, nil, nil, "UTC")
	s.SetQuietHours(0, 0)

	var got beat.Meta
	s.SetHeartbeatPrompt(func(ctx context.Context, _ int64, _ string) (string, error) {
		got = beat.From(ctx)
		return "", nil
	})

	const runID = 4242
	entry := ScheduleEntry{ID: 8, ChatID: 0, Message: "beat", Schedule: "1h", Type: "heartbeat", Mode: "prompt"}
	if _, _, err := s.execute(context.Background(), entry, runID); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if got.RunID != runID {
		t.Errorf("RunID = %d, want %d — journal rows would not join to the ledger", got.RunID, runID)
	}
	if got.Count != 1 {
		t.Errorf("Count = %d, want 1 (first bump)", got.Count)
	}
}

// The deep flag must track the real cadence, since it is what the journal uses
// to distinguish a self-audit turn from an ordinary beat.
func TestHeartbeatMetaDeepFlagTracksCadence(t *testing.T) {
	st := newMockStore(nil)
	s := New(st, nil, nil, "UTC")
	s.SetQuietHours(0, 0)
	s.SetDeepReflectInterval(3)

	var deepAt []int
	s.SetHeartbeatPrompt(func(ctx context.Context, _ int64, _ string) (string, error) {
		if m := beat.From(ctx); m.Deep {
			deepAt = append(deepAt, m.Count)
		}
		return "", nil
	})

	entry := ScheduleEntry{ID: 9, ChatID: 0, Message: "beat", Schedule: "1h", Type: "heartbeat", Mode: "prompt"}
	for i := 0; i < 6; i++ {
		if _, _, err := s.execute(context.Background(), entry, int64(100+i)); err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
	}

	if len(deepAt) != 2 || deepAt[0] != 3 || deepAt[1] != 6 {
		t.Fatalf("deep beats at %v, want [3 6] for interval 3", deepAt)
	}
}
