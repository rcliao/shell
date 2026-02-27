package planner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Unit tests: extractChangedFiles
// ---------------------------------------------------------------------------

func TestExtractChangedFiles(t *testing.T) {
	diff := `diff --git a/foo.go b/foo.go
index abc..def 100644
--- a/foo.go
+++ b/foo.go
@@ -1,3 +1,4 @@
 package main
+import "fmt"
 func main() {}
diff --git a/bar/baz.go b/bar/baz.go
index 111..222 100644
--- a/bar/baz.go
+++ b/bar/baz.go
@@ -1 +1,2 @@
 package bar
+func Baz() {}

--- new file: newfile.go ---
package main
func New() {}
`
	files := extractChangedFiles(diff)

	want := []string{"foo.go", "bar/baz.go", "newfile.go"}
	if len(files) != len(want) {
		t.Fatalf("got %d files, want %d: %v", len(files), len(want), files)
	}
	for i, f := range files {
		if f != want[i] {
			t.Errorf("file[%d] = %q, want %q", i, f, want[i])
		}
	}
}

func TestExtractChangedFiles_Dedup(t *testing.T) {
	diff := `diff --git a/foo.go b/foo.go
--- a/foo.go
+++ b/foo.go
some changes

--- new file: foo.go ---
duplicate entry
`
	files := extractChangedFiles(diff)
	if len(files) != 1 {
		t.Fatalf("expected dedup to 1 file, got %d: %v", len(files), files)
	}
	if files[0] != "foo.go" {
		t.Errorf("got %q, want foo.go", files[0])
	}
}

func TestExtractChangedFiles_Empty(t *testing.T) {
	files := extractChangedFiles("")
	if len(files) != 0 {
		t.Fatalf("expected 0 files from empty diff, got %d", len(files))
	}
}

// ---------------------------------------------------------------------------
// Integration tests: full RunTask flow
// ---------------------------------------------------------------------------

// setupTestRepo creates a temp dir with an initialised git repo, a committed
// file, and an uncommitted modification so getDiff has something to return.
// Returns (repoDir, auxDir) where auxDir is a SEPARATE directory for the mock
// binary and capture files — kept outside the repo so they don't pollute diffs.
func setupTestRepo(t *testing.T) (string, string) {
	t.Helper()
	repoDir := t.TempDir()
	auxDir := t.TempDir() // separate dir for mock + captures

	if err := os.MkdirAll(filepath.Join(auxDir, "captures"), 0o755); err != nil {
		t.Fatal(err)
	}

	gitCmd(t, repoDir, "init")
	gitCmd(t, repoDir, "config", "user.email", "test@test.com")
	gitCmd(t, repoDir, "config", "user.name", "Test")

	// Initial committed file
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", "-A")
	gitCmd(t, repoDir, "commit", "-m", "initial")

	// Uncommitted change — this is the "work" the execute agent did.
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"),
		[]byte("package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	return repoDir, auxDir
}

// writeMockClaude creates a shell script that behaves like the claude CLI.
// It captures each call's args and prompt to captureDir and returns a
// canned JSON response based on the prompt content.
// reviewResponses is a list of VERDICT lines to return for successive review calls.
// auxDir is a directory OUTSIDE the git repo for the mock binary and captures.
func writeMockClaude(t *testing.T, auxDir string, reviewResponses []string) string {
	t.Helper()

	// Build the case branches for review responses.
	// Each review call increments a counter and picks the next response.
	var reviewBranches strings.Builder
	for i, resp := range reviewResponses {
		reviewBranches.WriteString(fmt.Sprintf(
			"    %d) printf '{\"result\":\"%s\"}' ;;\n", i+1, resp))
	}
	// Fallback for extra calls
	reviewBranches.WriteString(fmt.Sprintf(
		"    *) printf '{\"result\":\"%s\"}' ;;\n",
		"VERDICT: done\\nFallback approval."))

	captureDir := filepath.Join(auxDir, "captures")

	script := fmt.Sprintf(`#!/bin/bash
set -e
CAPTURE_DIR=%q

# Assign a sequential call number.
CALL_NUM=$(ls "$CAPTURE_DIR"/call_*_args.txt 2>/dev/null | wc -l | tr -d ' ')

# Save raw args (one per line).
printf '%%s\n' "$@" > "$CAPTURE_DIR/call_${CALL_NUM}_args.txt"

# Extract -p prompt value.
PROMPT=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -p) PROMPT="$2"; shift 2 ;;
    *)  shift ;;
  esac
done
printf '%%s' "$PROMPT" > "$CAPTURE_DIR/call_${CALL_NUM}_prompt.txt"

# Respond based on prompt content.
if printf '%%s' "$PROMPT" | grep -q "strict code reviewer"; then
  # Track review call count.
  REVIEW_COUNT_FILE="$CAPTURE_DIR/review_count"
  COUNT=0
  if [ -f "$REVIEW_COUNT_FILE" ]; then
    COUNT=$(cat "$REVIEW_COUNT_FILE")
  fi
  COUNT=$((COUNT + 1))
  printf '%%s' "$COUNT" > "$REVIEW_COUNT_FILE"

  case "$COUNT" in
%s  esac
else
  printf '{"result":"Task completed successfully."}'
fi
`, captureDir, reviewBranches.String())

	mockPath := filepath.Join(auxDir, "mock-claude")
	if err := os.WriteFile(mockPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return mockPath
}

// TestRunTask_ReviewUsesReadOnlyTools verifies the happy-path end-to-end:
// execute → test → review, and that the review invocation uses read-only
// tools and instructs the reviewer to read the actual source files.
func TestRunTask_ReviewUsesReadOnlyTools(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	repoDir, auxDir := setupTestRepo(t)
	captureDir := filepath.Join(auxDir, "captures")
	mockBin := writeMockClaude(t, auxDir, []string{
		"VERDICT: done\\nLooks good, implementation matches the task.",
	})

	p := New(Config{
		ClaudeBinary: mockBin,
		WorkDir:      repoDir,
		TestCmd:      "true", // always passes
		MaxRetries:   1,
	})

	result := p.RunTask(context.Background(), "Add hello world print", "", nopProgress)

	if result.Verdict != VerdictDone {
		t.Fatalf("expected VerdictDone, got %s: %s", result.Verdict, result.Summary)
	}

	// We expect at least 2 calls: execute + review.
	calls := countCaptures(t, captureDir)
	if calls < 2 {
		t.Fatalf("expected at least 2 claude calls, got %d", calls)
	}

	// The LAST call should be the review.
	reviewArgs := readCapture(t, captureDir, calls-1, "args")
	reviewPrompt := readCapture(t, captureDir, calls-1, "prompt")

	// Execute call should have full tools.
	execArgs := readCapture(t, captureDir, 0, "args")
	execTools := allowedToolsFromArgs(t, execArgs)
	if execTools != "Bash,Write,Edit,Read" {
		t.Errorf("execute call should have full tools.\nGot --allowedTools: %s", execTools)
	}

	// Review call should have read-only tools.
	reviewTools := allowedToolsFromArgs(t, reviewArgs)
	if reviewTools != "Bash,Read,Glob,Grep" {
		t.Errorf("review call should use read-only tools.\nGot --allowedTools: %s", reviewTools)
	}

	// Review prompt should instruct reading files.
	if !strings.Contains(reviewPrompt, "Read tool") {
		t.Error("review prompt should instruct reviewer to use the Read tool")
	}
	if !strings.Contains(reviewPrompt, "Changed Files") {
		t.Error("review prompt should contain a 'Changed Files' section")
	}
	if !strings.Contains(reviewPrompt, "main.go") {
		t.Error("review prompt should list main.go as a changed file")
	}
}

// TestRunTask_NeedsRevisionRetry verifies the execute → review → revision
// → re-execute → review → done cycle, ensuring every review call is
// read-only.
func TestRunTask_NeedsRevisionRetry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	repoDir, auxDir := setupTestRepo(t)
	captureDir := filepath.Join(auxDir, "captures")
	mockBin := writeMockClaude(t, auxDir, []string{
		"VERDICT: needs_revision\\nMissing error handling in main.",
		"VERDICT: done\\nRevision looks good.",
	})

	p := New(Config{
		ClaudeBinary: mockBin,
		WorkDir:      repoDir,
		TestCmd:      "true",
		MaxRetries:   2,
	})

	var msgs []string
	result := p.RunTask(context.Background(), "Add hello world print", "", func(msg string) {
		msgs = append(msgs, msg)
	})

	if result.Verdict != VerdictDone {
		t.Fatalf("expected VerdictDone after retry, got %s: %s", result.Verdict, result.Summary)
	}
	if result.Attempts < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", result.Attempts)
	}

	// Verify all review calls used read-only tools.
	calls := countCaptures(t, captureDir)
	for i := 0; i < calls; i++ {
		prompt := readCapture(t, captureDir, i, "prompt")
		if !strings.Contains(prompt, "strict code reviewer") {
			continue // not a review call
		}
		args := readCapture(t, captureDir, i, "args")
		tools := allowedToolsFromArgs(t, args)
		if tools != "Bash,Read,Glob,Grep" {
			t.Errorf("review call %d should use read-only tools.\nGot --allowedTools: %s", i, tools)
		}
	}

	// Verify progress included revision messaging.
	found := false
	for _, m := range msgs {
		if strings.Contains(m, "revision") || strings.Contains(m, "Retry") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected progress messages about revision/retry")
	}
}

// TestRunTask_AutoApproveSkipsReview verifies that small diffs with passing
// tests skip the review entirely when AutoApproveThreshold is set.
func TestRunTask_AutoApproveSkipsReview(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	repoDir, auxDir := setupTestRepo(t)
	captureDir := filepath.Join(auxDir, "captures")
	mockBin := writeMockClaude(t, auxDir, []string{
		"VERDICT: done\\nShould not reach here.",
	})

	p := New(Config{
		ClaudeBinary:         mockBin,
		WorkDir:              repoDir,
		TestCmd:              "true",
		MaxRetries:           1,
		AutoApproveThreshold: 500, // well above our small diff
	})

	result := p.RunTask(context.Background(), "Add hello world print", "", nopProgress)

	if result.Verdict != VerdictDone {
		t.Fatalf("expected VerdictDone, got %s: %s", result.Verdict, result.Summary)
	}
	if !strings.Contains(result.Summary, "Auto-approved") {
		t.Errorf("expected auto-approve summary, got: %s", result.Summary)
	}

	// Should only have 1 call (execute), no review.
	calls := countCaptures(t, captureDir)
	if calls != 1 {
		t.Errorf("expected exactly 1 claude call (execute only), got %d", calls)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func nopProgress(string) {}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func countCaptures(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "_args.txt") {
			count++
		}
	}
	return count
}

func readCapture(t *testing.T, dir string, idx int, suffix string) string {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("call_%d_%s.txt", idx, suffix))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading capture %s: %v", path, err)
	}
	return string(data)
}

// allowedToolsFromArgs extracts the --allowedTools value from captured args.
// Args are stored one-per-line by the mock script.
func allowedToolsFromArgs(t *testing.T, args string) string {
	t.Helper()
	lines := strings.Split(args, "\n")
	for i, line := range lines {
		if line == "--allowedTools" && i+1 < len(lines) {
			return lines[i+1]
		}
	}
	t.Fatal("--allowedTools not found in captured args")
	return ""
}
