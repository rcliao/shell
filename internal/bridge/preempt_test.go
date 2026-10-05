package bridge

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/scheduler"
)

// A system turn is reported as lost to a preempt only when the user message
// caused the cancel AND the turn is not still running. A turn that lives on
// (ErrTurnAbandoned) follows up by itself; a re-run would post twice.
func TestPreemptLost(t *testing.T) {
	preempted := func() context.Context {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(ErrPreempted)
		return ctx
	}
	plain, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"killed with its process", preempted(), fmt.Errorf("%w: context canceled", process.ErrTurnKilled), true},
		{"stopped before start (9/5 shape)", preempted(), errors.New("start claude: context canceled"), true},
		{"still running, will follow up", preempted(), fmt.Errorf("%w: context canceled", process.ErrTurnAbandoned), false},
		{"cancelled for another reason", plain, fmt.Errorf("%w: context canceled", process.ErrTurnKilled), false},
	}
	for _, c := range cases {
		if got := preemptLost(c.ctx, c.err); got != c.want {
			t.Errorf("%s: preemptLost = %v, want %v", c.name, got, c.want)
		}
	}
}

// The scheduler matches the preempt by text (it cannot import bridge). Keep
// the two in step: the error the bridge returns must read as preempted and
// retryable there.
func TestPreemptErrorIsRetryableInScheduler(t *testing.T) {
	err := fmt.Errorf("claude: %w: %v", ErrPreempted, process.ErrTurnKilled)
	if !scheduler.IsPreempted(err) || !scheduler.IsRetryable(err) {
		t.Fatalf("scheduler does not recognise %q", err)
	}
}

// preemptSystemSession cancels with ErrPreempted as the cause.
func TestPreemptSystemSessionSetsCause(t *testing.T) {
	b := &Bridge{systemCancel: map[process.SessionKey]context.CancelCauseFunc{}}
	ctx, cancel := context.WithCancelCause(context.Background())
	key := process.SessionKey{ChatID: -100200300}
	defer b.registerSystemCancel(key, cancel)()
	b.proc = process.NewManager(process.ManagerConfig{})
	b.preemptSystemSession(key)
	if !errors.Is(context.Cause(ctx), ErrPreempted) {
		t.Fatalf("cause = %v, want ErrPreempted", context.Cause(ctx))
	}
}
