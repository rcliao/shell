package search

import (
	"context"
	"os/exec"
)

// execCommand wraps exec.CommandContext for testability.
var execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
