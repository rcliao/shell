package daemon

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/bridge"
	"github.com/rcliao/shell/internal/process"
)

func TestRetryBusySend(t *testing.T) {
	var slept []time.Duration
	origSleep := busySleep
	busySleep = func(d time.Duration) { slept = append(slept, d) }
	defer func() { busySleep = origSleep }()

	// The busy error as manager.Send actually produces it (wrapped sentinel).
	busyErr := fmt.Errorf("claude: %w",
		fmt.Errorf("session for chat -100 thread 0 is busy: %w", process.ErrSessionBusy))

	t.Run("success first try", func(t *testing.T) {
		slept = nil
		calls := 0
		resp, err := retryBusySend(-100, "a2a", func() (bridge.AgentResponse, error) {
			calls++
			return bridge.AgentResponse{Text: "hi"}, nil
		})
		if err != nil || resp.Text != "hi" || calls != 1 || len(slept) != 0 {
			t.Fatalf("err=%v resp=%q calls=%d slept=%v", err, resp.Text, calls, slept)
		}
	})

	t.Run("busy then success", func(t *testing.T) {
		slept = nil
		calls := 0
		resp, err := retryBusySend(-100, "a2a", func() (bridge.AgentResponse, error) {
			calls++
			if calls == 1 {
				return bridge.AgentResponse{}, busyErr
			}
			return bridge.AgentResponse{Text: "ok"}, nil
		})
		if err != nil || resp.Text != "ok" || calls != 2 {
			t.Fatalf("err=%v resp=%q calls=%d", err, resp.Text, calls)
		}
		if len(slept) != 1 || slept[0] != 30*time.Second {
			t.Fatalf("slept=%v, want [30s]", slept)
		}
	})

	t.Run("persistently busy gives up after all delays", func(t *testing.T) {
		slept = nil
		calls := 0
		_, err := retryBusySend(-100, "scheduler", func() (bridge.AgentResponse, error) {
			calls++
			return bridge.AgentResponse{}, busyErr
		})
		if !errors.Is(err, process.ErrSessionBusy) {
			t.Fatalf("want busy error, got %v", err)
		}
		if calls != 4 || len(slept) != 3 {
			t.Fatalf("calls=%d slept=%v, want 4 calls / 3 sleeps", calls, slept)
		}
		want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}
		for i, d := range want {
			if slept[i] != d {
				t.Fatalf("slept=%v, want %v", slept, want)
			}
		}
	})

	t.Run("non-busy error returns immediately", func(t *testing.T) {
		slept = nil
		calls := 0
		wantErr := errors.New("claude exploded")
		_, err := retryBusySend(-100, "a2a", func() (bridge.AgentResponse, error) {
			calls++
			return bridge.AgentResponse{}, wantErr
		})
		if !errors.Is(err, wantErr) || calls != 1 || len(slept) != 0 {
			t.Fatalf("err=%v calls=%d slept=%v", err, calls, slept)
		}
	})
}
