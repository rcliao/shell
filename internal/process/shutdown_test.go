package process

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// The graceful path: a process that exits when its stdin closes must be allowed
// to do so, without a signal. This is what the CLI does when it sees EOF on the
// stream protocol — it finishes and leaves.
func TestStdinCloseLetsProcessExitCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// `cat` exits 0 on EOF — a stand-in for a well-behaved child.
	cmd := exec.CommandContext(ctx, "cat")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = sigtermGrace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	p := &persistentProc{cmd: cmd, stdin: stdin, cancel: cancel}

	start := time.Now()
	p.kill()
	elapsed := time.Since(start)

	if elapsed >= stdinCloseGrace {
		t.Errorf("took %s — a process that exits on EOF should not need the grace period", elapsed)
	}
	if err := cmd.ProcessState; err == nil {
		t.Fatal("process state missing after kill")
	}
	if !cmd.ProcessState.Success() {
		t.Errorf("clean EOF exit reported failure: %v — this is what produced \"signal: killed\"", cmd.ProcessState)
	}
}

// Escalation: a process that ignores stdin close must still be terminated,
// bounded. Shutdown can never hang on a wedged child.
func TestUnresponsiveProcessIsTerminatedWithinBounds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// `sleep` ignores stdin entirely.
	cmd := exec.CommandContext(ctx, "sleep", "300")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = sigtermGrace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	p := &persistentProc{cmd: cmd, stdin: stdin, cancel: cancel}

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		p.kill()
		done <- time.Since(start)
	}()

	select {
	case elapsed := <-done:
		if elapsed < stdinCloseGrace {
			t.Errorf("terminated after %s — the grace period was not honored", elapsed)
		}
		if elapsed > stdinCloseGrace+sigtermGrace+2*time.Second {
			t.Errorf("took %s — escalation is not bounded", elapsed)
		}
	case <-time.After(stdinCloseGrace + sigtermGrace + 5*time.Second):
		t.Fatal("kill() hung on an unresponsive process — shutdown must always be bounded")
	}
}

// The grace budget must stay well inside the drain budget, or a graceful
// shutdown would be cut short by the restart it is trying to serve.
func TestGraceBudgetsFitInsideDrain(t *testing.T) {
	const drainTimeout = 15 * time.Minute // cmd/shell/main.go
	if total := stdinCloseGrace + sigtermGrace; total >= drainTimeout {
		t.Fatalf("shutdown grace %s exceeds the drain budget %s", total, drainTimeout)
	}
}
