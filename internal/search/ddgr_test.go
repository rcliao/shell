package search

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// fakeDDGR makes execCommand run a shell snippet instead of ddgr.
func fakeDDGR(t *testing.T, script string) {
	t.Helper()
	orig := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
	t.Cleanup(func() { execCommand = orig })
}

func TestDDGRRefusalIsAnErrorNotNoResults(t *testing.T) {
	// What DuckDuckGo's anti-bot answer looks like through ddgr today.
	fakeDDGR(t, `echo '[ERROR] HTTP Error 202: Accepted' >&2; echo '[]'`)
	_, err := Search(context.Background(), "", "", Options{Query: "tokyo weather"})
	if err == nil || !strings.Contains(err.Error(), "HTTP Error 202") {
		t.Fatalf("a refused search must be an error naming the cause, got %v", err)
	}
}

func TestDDGRGenuineEmptyIsNotAnError(t *testing.T) {
	fakeDDGR(t, `echo '[]'`)
	resp, err := Search(context.Background(), "", "", Options{Query: "zzqqxx"})
	if err != nil || len(resp.Results) != 0 {
		t.Fatalf("a clean empty result is a result: %v %v", resp, err)
	}
}

func TestDDGRResultsParse(t *testing.T) {
	fakeDDGR(t, `echo '[{"title":"T","url":"https://x","abstract":"A"}]'`)
	resp, err := Search(context.Background(), "", "", Options{Query: "q"})
	if err != nil || len(resp.Results) != 1 || resp.Results[0].URL != "https://x" {
		t.Fatalf("parse: %+v %v", resp, err)
	}
}
