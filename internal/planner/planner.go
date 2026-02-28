// Package planner runs a plan file through an execute → test → review loop.
// It reports progress via a callback and stops on needs_human for human input.
package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Verdict is the result of a review.
type Verdict string

const (
	VerdictDone          Verdict = "done"
	VerdictNeedsRevision Verdict = "needs_revision"
	VerdictNeedsHuman    Verdict = "needs_human"
)

// TaskResult describes what happened with a single task.
type TaskResult struct {
	Task       string
	Verdict    Verdict
	Summary    string // review output or error message
	Attempts   int
	Diff       string // git diff at time of failure
	TestOutput string // test output at time of failure
}

// ReviewPackage bundles everything the reviewer needs to make a verdict.
type ReviewPackage struct {
	Task         string
	ExecSummary  string   // what the execute agent said it did (truncated to 2000 chars)
	Diff         string
	ChangedFiles []string
	TestsPassed  bool
	TestSummary  string // condensed: "All tests passed (showing last 10)" or failure details
	Attempt      int
}

// ProgressFunc is called to report progress. The planner does not know about
// Telegram — the caller wires this to whatever output channel they want.
type ProgressFunc func(msg string)

// Config holds planner settings.
type Config struct {
	ClaudeBinary         string        // path to claude CLI
	Model                string        // optional model flag
	WorkDir              string        // project working directory
	TestCmd              string        // test command (e.g. "go test ./...")
	Conventions          string        // conventions text for the reviewer
	MaxRetries           int           // retries per task on needs_revision
	Timeout              time.Duration // per-claude-invocation timeout
	AutoApproveThreshold int           // max diff lines to auto-approve without review
}

// Planner orchestrates plan execution.
type Planner struct {
	cfg      Config
	taskBase string // HEAD SHA before the current task started; reset per task
}

// New creates a planner with the given config.
func New(cfg Config) *Planner {
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 2
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Minute
	}
	return &Planner{cfg: cfg}
}

// WorkDir returns the planner's configured working directory.
func (p *Planner) WorkDir() string {
	return p.cfg.WorkDir
}

// CloneWithWorkDir returns a new Planner with the same config but a different WorkDir.
func (p *Planner) CloneWithWorkDir(workDir string) *Planner {
	cfg := p.cfg
	cfg.WorkDir = workDir
	return &Planner{cfg: cfg}
}

// snapshotBase records the current HEAD SHA so getDiff can later show all
// changes made during a task, even if the execute agent stages or commits.
func (p *Planner) snapshotBase(ctx context.Context) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	if p.cfg.WorkDir != "" {
		cmd.Dir = p.cfg.WorkDir
	}
	out, err := cmd.Output()
	if err != nil {
		p.taskBase = ""
		return
	}
	p.taskBase = strings.TrimSpace(string(out))
}

// ParsePlan extracts unchecked tasks from plan text.
// Each task is a line matching "- [ ] <description>".
func ParsePlan(planText string) []string {
	var tasks []string
	re := regexp.MustCompile(`(?m)^- \[ \] (.+)$`)
	for _, m := range re.FindAllStringSubmatch(planText, -1) {
		tasks = append(tasks, strings.TrimSpace(m[1]))
	}
	return tasks
}

// RunTask executes a single task through the full loop:
// execute → test → review → decide. Returns the result.
// completedContext summarises previously completed tasks for continuity.
func (p *Planner) RunTask(ctx context.Context, task, completedContext string, progress ProgressFunc) TaskResult {
	p.snapshotBase(ctx)
	feedback := ""

	for attempt := 0; attempt <= p.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			progress(fmt.Sprintf("Retry %d/%d (addressing review feedback)...", attempt, p.cfg.MaxRetries))
		}

		// Step 1: Execute
		progress(fmt.Sprintf("Executing: %s", task))
		execOutput, err := p.execute(ctx, task, feedback, completedContext)
		if err != nil {
			progress(fmt.Sprintf("Execution error: %v", err))
			return TaskResult{Task: task, Verdict: VerdictNeedsHuman, Summary: err.Error(), Attempts: attempt + 1, Diff: p.getDiff(ctx)}
		}

		// Step 2: Test
		progress("Running tests...")
		testOutput, testOk := p.runTests(ctx)
		if !testOk {
			diff := p.getDiff(ctx)
			testSummary := summarizeTestOutput(testOutput, false)
			feedback = buildTestFailureFeedback(testSummary, diff, attempt+1)
			progress(fmt.Sprintf("Tests FAILED:\n%s", lastNLines(testOutput, 20)))
			continue
		}
		progress("Tests passed.")

		// Step 3: Check for auto-approve
		diffOutput := p.getDiff(ctx)
		if diffOutput == "" {
			progress("No changes detected. Skipping review.")
			return TaskResult{Task: task, Verdict: VerdictDone, Summary: "No changes needed", Attempts: attempt + 1}
		}

		if p.shouldSkipReview(diffOutput, true) {
			progress(fmt.Sprintf("Auto-approved (%d lines, threshold %d).", diffLineCount(diffOutput), p.cfg.AutoApproveThreshold))
			return TaskResult{Task: task, Verdict: VerdictDone, Summary: "Auto-approved: tests pass, diff within threshold", Attempts: attempt + 1}
		}

		// Step 4: Review
		changedFiles := extractChangedFiles(diffOutput)
		pkg := ReviewPackage{
			Task:         task,
			ExecSummary:  truncate(execOutput, 2000),
			Diff:         diffOutput,
			ChangedFiles: changedFiles,
			TestsPassed:  true,
			TestSummary:  summarizeTestOutput(testOutput, true),
			Attempt:      attempt + 1,
		}

		progress("Reviewing changes...")
		verdict, reviewText := p.review(ctx, pkg)
		progress(fmt.Sprintf("Review verdict: %s", verdict))

		switch verdict {
		case VerdictDone:
			return TaskResult{Task: task, Verdict: VerdictDone, Summary: reviewText, Attempts: attempt + 1}
		case VerdictNeedsHuman:
			return TaskResult{Task: task, Verdict: VerdictNeedsHuman, Summary: reviewText, Attempts: attempt + 1, Diff: diffOutput, TestOutput: testOutput}
		case VerdictNeedsRevision:
			feedback = buildRetryFeedback(reviewText, diffOutput, changedFiles, attempt+1)
			progress("Reviewer requested revision.")
		}
	}

	// Exhausted retries
	return TaskResult{Task: task, Verdict: VerdictNeedsHuman, Summary: "Exhausted retries. Last feedback:\n" + feedback, Attempts: p.cfg.MaxRetries + 1, Diff: p.getDiff(ctx), TestOutput: feedback}
}

// RunPlan executes all tasks from a plan. Stops on first needs_human or failure.
// Returns results for each attempted task.
func (p *Planner) RunPlan(ctx context.Context, planText string, progress ProgressFunc) []TaskResult {
	tasks := ParsePlan(planText)
	if len(tasks) == 0 {
		progress("No pending tasks found in plan.")
		return nil
	}

	progress(fmt.Sprintf("Plan has %d tasks.", len(tasks)))
	var results []TaskResult
	var completedContext string

	for i, task := range tasks {
		progress(fmt.Sprintf("\n=== Task %d/%d: %s ===", i+1, len(tasks), task))

		result := p.RunTask(ctx, task, completedContext, progress)
		results = append(results, result)

		if result.Verdict != VerdictDone {
			progress(fmt.Sprintf("Task %d stopped: %s", i+1, result.Verdict))
			break
		}

		// Git checkpoint after each successful task
		p.GitCheckpoint(ctx, task)

		// Accumulate context for subsequent tasks
		completedContext += fmt.Sprintf("- %s: %s\n", task, result.Summary)

		progress(fmt.Sprintf("Task %d/%d: DONE", i+1, len(tasks)))

		// Brief pause between tasks
		if i < len(tasks)-1 {
			time.Sleep(3 * time.Second)
		}
	}

	return results
}

// RunTaskWithGuidance runs a task with user guidance seeded as initial feedback,
// plus the current git diff for context. Used when resuming from a blocked state.
func (p *Planner) RunTaskWithGuidance(ctx context.Context, task, guidance, completedContext string, progress ProgressFunc) TaskResult {
	p.snapshotBase(ctx)
	diff := p.getDiff(ctx)
	initialFeedback := fmt.Sprintf("Human guidance: %s\n\nYour current changes on disk:\n%s", guidance, diff)

	// Same logic as RunTask but with pre-seeded feedback.
	feedback := initialFeedback

	for attempt := 0; attempt <= p.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			progress(fmt.Sprintf("Retry %d/%d (addressing review feedback)...", attempt, p.cfg.MaxRetries))
		}

		progress(fmt.Sprintf("Executing: %s", task))
		execOutput, err := p.execute(ctx, task, feedback, completedContext)
		if err != nil {
			progress(fmt.Sprintf("Execution error: %v", err))
			return TaskResult{Task: task, Verdict: VerdictNeedsHuman, Summary: err.Error(), Attempts: attempt + 1, Diff: p.getDiff(ctx)}
		}

		progress("Running tests...")
		testOutput, testOk := p.runTests(ctx)
		if !testOk {
			d := p.getDiff(ctx)
			testSummary := summarizeTestOutput(testOutput, false)
			feedback = buildTestFailureFeedback(testSummary, d, attempt+1)
			progress(fmt.Sprintf("Tests FAILED:\n%s", lastNLines(testOutput, 20)))
			continue
		}
		progress("Tests passed.")

		diffOutput := p.getDiff(ctx)
		if diffOutput == "" {
			progress("No changes detected. Skipping review.")
			return TaskResult{Task: task, Verdict: VerdictDone, Summary: "No changes needed", Attempts: attempt + 1}
		}

		if p.shouldSkipReview(diffOutput, true) {
			progress(fmt.Sprintf("Auto-approved (%d lines, threshold %d).", diffLineCount(diffOutput), p.cfg.AutoApproveThreshold))
			return TaskResult{Task: task, Verdict: VerdictDone, Summary: "Auto-approved: tests pass, diff within threshold", Attempts: attempt + 1}
		}

		changedFiles := extractChangedFiles(diffOutput)
		pkg := ReviewPackage{
			Task:         task,
			ExecSummary:  truncate(execOutput, 2000),
			Diff:         diffOutput,
			ChangedFiles: changedFiles,
			TestsPassed:  true,
			TestSummary:  summarizeTestOutput(testOutput, true),
			Attempt:      attempt + 1,
		}

		progress("Reviewing changes...")
		verdict, reviewText := p.review(ctx, pkg)
		progress(fmt.Sprintf("Review verdict: %s", verdict))

		switch verdict {
		case VerdictDone:
			return TaskResult{Task: task, Verdict: VerdictDone, Summary: reviewText, Attempts: attempt + 1}
		case VerdictNeedsHuman:
			return TaskResult{Task: task, Verdict: VerdictNeedsHuman, Summary: reviewText, Attempts: attempt + 1, Diff: diffOutput, TestOutput: testOutput}
		case VerdictNeedsRevision:
			feedback = buildRetryFeedback(reviewText, diffOutput, changedFiles, attempt+1)
			progress("Reviewer requested revision.")
		}
	}

	return TaskResult{Task: task, Verdict: VerdictNeedsHuman, Summary: "Exhausted retries. Last feedback:\n" + feedback, Attempts: p.cfg.MaxRetries + 1, Diff: p.getDiff(ctx), TestOutput: feedback}
}

// RunPlanFrom continues a plan from startIdx (0-based), skipping earlier tasks.
// completedContext carries summaries of tasks completed before startIdx.
// Used when resuming after a blocked task is resolved.
func (p *Planner) RunPlanFrom(ctx context.Context, planText string, startIdx int, completedContext string, progress ProgressFunc) []TaskResult {
	tasks := ParsePlan(planText)
	if startIdx >= len(tasks) {
		progress("No remaining tasks to run.")
		return nil
	}

	remaining := tasks[startIdx:]
	progress(fmt.Sprintf("Resuming plan from task %d/%d (%d remaining).", startIdx+1, len(tasks), len(remaining)))
	var results []TaskResult

	for i, task := range remaining {
		taskNum := startIdx + i + 1
		progress(fmt.Sprintf("\n=== Task %d/%d: %s ===", taskNum, len(tasks), task))

		result := p.RunTask(ctx, task, completedContext, progress)
		results = append(results, result)

		if result.Verdict != VerdictDone {
			progress(fmt.Sprintf("Task %d stopped: %s", taskNum, result.Verdict))
			break
		}

		// Git checkpoint after each successful task
		p.GitCheckpoint(ctx, task)

		// Accumulate context for subsequent tasks
		completedContext += fmt.Sprintf("- %s: %s\n", task, result.Summary)

		progress(fmt.Sprintf("Task %d/%d: DONE", taskNum, len(tasks)))

		if i < len(remaining)-1 {
			time.Sleep(3 * time.Second)
		}
	}

	return results
}

// execute runs Claude to implement the task.
func (p *Planner) execute(ctx context.Context, task, feedback, completedContext string) (string, error) {
	feedbackBlock := ""
	if feedback != "" {
		feedbackBlock = fmt.Sprintf(`
## Feedback From Review
A reviewer found issues with your previous attempt. You MUST address this:
%s`, feedback)
	}

	conventionsBlock := ""
	if p.cfg.Conventions != "" {
		conventionsBlock = fmt.Sprintf(`
## Project Conventions
%s`, p.cfg.Conventions)
	}

	contextBlock := ""
	if completedContext != "" {
		contextBlock = fmt.Sprintf(`
## Previously Completed Tasks
%s`, completedContext)
	}

	testRule := "- Before finishing, find the project that your changes belong to (look for go.mod, package.json, Cargo.toml, Makefile, etc. in the directory of your changed files), cd into that project directory, and run its tests to make sure nothing is broken"
	if p.cfg.TestCmd != "" {
		testRule = fmt.Sprintf("- Run '%s' before finishing to make sure nothing is broken", p.cfg.TestCmd)
	}

	prompt := fmt.Sprintf(`You are an autonomous coding agent. Complete this task fully in this session.

## Your Task
%s
%s%s%s
## Rules
- Complete the ENTIRE task in this session
%s
- Keep changes minimal and focused on the task
- If the task is ambiguous or would require violating a convention, explain what you need clarified instead of guessing`, task, feedbackBlock, conventionsBlock, contextBlock, testRule)

	return p.runClaude(ctx, prompt)
}

// review runs a second Claude invocation to evaluate the diff.
// The reviewer is text-only (no tools) — it evaluates the diff and test
// results provided inline, keeping it fast and deterministic.
func (p *Planner) review(ctx context.Context, pkg ReviewPackage) (Verdict, string) {
	conventionsBlock := ""
	if p.cfg.Conventions != "" {
		conventionsBlock = fmt.Sprintf(`
## Project Conventions
%s`, p.cfg.Conventions)
	}

	execSummaryBlock := ""
	if pkg.ExecSummary != "" {
		execSummaryBlock = fmt.Sprintf(`
## What the Agent Did
%s
`, pkg.ExecSummary)
	}

	prompt := fmt.Sprintf(`You are reviewing whether an automated coding agent completed a task correctly.
Tests have already passed. Your job is to check that the diff matches the task intent.

## Task
%s
%s%s
## Git Diff (%d files changed)
%s

## Test Results
%s

## Instructions
Look at the diff and decide: does this implement the task?

Accept (VERDICT: done) if:
- The diff implements what the task asked for
- Tests pass
- No obvious bugs that would cause runtime failures

Reject (VERDICT: needs_revision) ONLY if:
- The diff has clear bugs (e.g. wrong variable, broken logic)
- The diff is missing a key part of what the task asked for
- Give specific, actionable feedback the agent can fix

Escalate (VERDICT: needs_human) ONLY if:
- The approach is fundamentally wrong and needs human guidance
- There are clear convention violations (schema changes, new CLI commands, new interfaces not in the task)

Do NOT reject for:
- Style preferences or minor idiom differences
- Uncommitted changes (the planner commits after approval)
- Things working correctly but done differently than you would

Respond with EXACTLY one verdict on the first line:
VERDICT: done
VERDICT: needs_revision
VERDICT: needs_human

Then a brief explanation.`, pkg.Task, conventionsBlock, execSummaryBlock, len(pkg.ChangedFiles), pkg.Diff, pkg.TestSummary)

	output, err := p.runClaudeTextOnly(ctx, prompt)
	if err != nil {
		return VerdictNeedsHuman, fmt.Sprintf("Review failed: %v", err)
	}

	// Extract verdict
	re := regexp.MustCompile(`VERDICT:\s*(done|needs_revision|needs_human)`)
	match := re.FindStringSubmatch(output)
	if match == nil {
		return VerdictNeedsHuman, "Could not parse reviewer verdict.\n\n" + output
	}

	return Verdict(match[1]), output
}

// extractChangedFiles parses a git diff to extract the list of changed file paths.
func extractChangedFiles(diff string) []string {
	var files []string
	seen := make(map[string]bool)

	// Match standard diff headers: +++ b/path/to/file
	re := regexp.MustCompile(`(?m)^\+\+\+ b/(.+)$`)
	for _, m := range re.FindAllStringSubmatch(diff, -1) {
		f := m[1]
		if !seen[f] {
			seen[f] = true
			files = append(files, f)
		}
	}

	// Match new untracked files: --- new file: path/to/file ---
	reNew := regexp.MustCompile(`(?m)^--- new file: (.+) ---$`)
	for _, m := range reNew.FindAllStringSubmatch(diff, -1) {
		f := m[1]
		if !seen[f] {
			seen[f] = true
			files = append(files, f)
		}
	}

	return files
}

// runTests executes tests and returns output + success.
// If TestCmd is configured, it runs that command directly.
// Otherwise, it asks Claude to determine and run the appropriate tests.
func (p *Planner) runTests(ctx context.Context) (string, bool) {
	if p.cfg.TestCmd != "" {
		return p.runStaticTests(ctx)
	}
	return p.runAgentTests(ctx)
}

// runStaticTests executes a fixed test command.
func (p *Planner) runStaticTests(ctx context.Context) (string, bool) {
	testCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(testCtx, "sh", "-c", p.cfg.TestCmd)
	if p.cfg.WorkDir != "" {
		cmd.Dir = p.cfg.WorkDir
	}

	output, err := cmd.CombinedOutput()
	return string(output), err == nil
}

// runAgentTests asks Claude to determine and run the appropriate tests.
func (p *Planner) runAgentTests(ctx context.Context) (string, bool) {
	diff := p.getDiff(ctx)

	prompt := fmt.Sprintf(`You are a test runner. Your job is to run the appropriate tests for the changes shown below.

## Changes
%s

## Instructions
1. Look at the file paths in the diff to identify which project or subdirectory was modified
2. Navigate to that project's root directory (where you find go.mod, package.json, Cargo.toml, pyproject.toml, Makefile, etc.)
3. Run the standard test command for that project from within its directory
4. Do NOT run tests from the workspace root if it is not itself a project

## Output
After running the tests, you MUST end your response with EXACTLY one of these lines:
TEST_RESULT: pass
TEST_RESULT: fail

Include the full test output before this line.`, diff)

	output, err := p.runClaude(ctx, prompt)
	if err != nil {
		return fmt.Sprintf("Agent test runner failed: %v", err), false
	}

	passed := strings.Contains(output, "TEST_RESULT: pass")
	return output, passed
}

// getDiff returns all changes in the working directory relative to the task
// starting point. It captures staged changes, unstaged changes, committed
// changes (if the execute agent committed), and new untracked files.
func (p *Planner) getDiff(ctx context.Context) string {
	run := func(args ...string) string {
		cmd := exec.CommandContext(ctx, "git", args...)
		if p.cfg.WorkDir != "" {
			cmd.Dir = p.cfg.WorkDir
		}
		out, _ := cmd.Output()
		return string(out)
	}

	// Use the task base commit stored before execution started.
	// Falls back to HEAD for staged+unstaged changes.
	base := p.taskBase
	if base == "" {
		base = "HEAD"
	}

	// git diff <base> shows all changes (staged + unstaged) vs the base.
	// If the execute agent also committed, this captures those commits too.
	// Use 10 lines of context so the reviewer can judge changes without
	// needing to read the full files.
	diff := run("diff", "-U10", base)

	// Include new untracked files
	untracked := strings.TrimSpace(run("ls-files", "--others", "--exclude-standard"))
	if untracked != "" {
		for _, f := range strings.Split(untracked, "\n") {
			path := f
			if p.cfg.WorkDir != "" {
				path = p.cfg.WorkDir + "/" + f
			}
			content, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			// Limit to first 100 lines
			lines := strings.SplitN(string(content), "\n", 101)
			if len(lines) > 100 {
				lines = append(lines[:100], "... (truncated)")
			}
			diff += fmt.Sprintf("\n--- new file: %s ---\n%s\n", f, strings.Join(lines, "\n"))
		}
	}

	return diff
}

// diffLineCount counts the number of lines in a diff string.
func diffLineCount(diff string) int {
	if diff == "" {
		return 0
	}
	return strings.Count(diff, "\n") + 1
}

// shouldSkipReview returns true if the diff is small enough to auto-approve.
func (p *Planner) shouldSkipReview(diff string, testsPassed bool) bool {
	if p.cfg.AutoApproveThreshold <= 0 {
		return false
	}
	return testsPassed && diffLineCount(diff) <= p.cfg.AutoApproveThreshold
}

// GitCheckpoint commits all current changes with a plan-prefixed message.
// Errors are logged but not fatal — this is best-effort.
func (p *Planner) GitCheckpoint(ctx context.Context, taskSummary string) {
	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		if p.cfg.WorkDir != "" {
			cmd.Dir = p.cfg.WorkDir
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			slog.Warn("git checkpoint failed", "args", args, "output", string(out), "error", err)
		}
		return err
	}

	if err := run("add", "-A"); err != nil {
		return
	}
	msg := fmt.Sprintf("plan: %s", taskSummary)
	if len(msg) > 72 {
		msg = msg[:72]
	}
	run("commit", "-m", msg, "--allow-empty-message")
}

// runClaudeTextOnly spawns a Claude CLI subprocess for text generation only
// (no tools). Used for drafting plans where no file/bash access is needed.
func (p *Planner) runClaudeTextOnly(ctx context.Context, prompt string) (string, error) {
	return p.runClaudeWithArgs(ctx, prompt, []string{
		"--disallowedTools", "Bash,Write,Edit",
	})
}

// runClaude spawns a Claude CLI subprocess with full tool access and
// permissions bypassed. Used for task execution where the planner's
// own test+review gates provide safety.
func (p *Planner) runClaude(ctx context.Context, prompt string) (string, error) {
	return p.runClaudeWithArgs(ctx, prompt, []string{
		"--allowedTools", "Bash,Write,Edit,Read",
		"--permission-mode", "bypassPermissions",
	})
}

func (p *Planner) runClaudeWithArgs(ctx context.Context, prompt string, extraArgs []string) (string, error) {
	procCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	args := []string{
		"-p", prompt,
		"--output-format", "json",
	}
	args = append(args, extraArgs...)
	if p.cfg.Model != "" {
		args = append(args, "--model", p.cfg.Model)
	}

	binary := p.cfg.ClaudeBinary
	if binary == "" {
		binary = "claude"
	}

	slog.Debug("planner: spawning claude", "binary", binary)

	cmd := exec.CommandContext(procCtx, binary, args...)
	cmd.Env = filterEnv(os.Environ(), "CLAUDECODE")
	if p.cfg.WorkDir != "" {
		cmd.Dir = p.cfg.WorkDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if procCtx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("claude timed out after %s", p.cfg.Timeout)
		}
		stderrStr := strings.TrimSpace(stderr.String())
		stdoutStr := strings.TrimSpace(stdout.String())
		switch {
		case stderrStr != "" && stdoutStr != "":
			return "", fmt.Errorf("claude failed: %w\nstderr: %s\nstdout: %s", err, stderrStr, stdoutStr)
		case stderrStr != "":
			return "", fmt.Errorf("claude failed: %w\nstderr: %s", err, stderrStr)
		case stdoutStr != "":
			return "", fmt.Errorf("claude failed: %w\nstdout: %s", err, stdoutStr)
		default:
			return "", fmt.Errorf("claude failed: %w", err)
		}
	}

	// Parse JSON output if possible
	var result struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return stdout.String(), nil
	}
	return result.Result, nil
}

// filterEnv returns env with the named variable removed.
func filterEnv(env []string, name string) []string {
	prefix := name + "="
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// DraftPlan asks Claude to generate a markdown checklist from a high-level
// intent. If feedback is non-empty, the previous draft and user feedback are
// included so Claude can revise.
func (p *Planner) DraftPlan(ctx context.Context, intent, previousDraft, feedback string) (string, error) {
	granularity := "Each task MUST be small and focused — at most 3-4 files changed per task. Break larger work into multiple sequential tasks."

	var prompt string
	if feedback == "" {
		prompt = fmt.Sprintf(
			"Generate a concise markdown task checklist for the following goal. "+
				"Output ONLY the checklist lines, each starting with \"- [ ] \". "+
				"No preamble, no explanation.\n\n"+
				"%s\n\nGoal: %s", granularity, intent)
	} else {
		prompt = fmt.Sprintf(
			"Revise the following task checklist based on the user's feedback. "+
				"Output ONLY the updated checklist lines, each starting with \"- [ ] \". "+
				"No preamble, no explanation.\n\n"+
				"%s\n\n"+
				"Previous plan:\n%s\n\nFeedback: %s", granularity, previousDraft, feedback)
	}

	return p.runClaudeTextOnly(ctx, prompt)
}

// truncate returns s truncated to maxLen characters.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n... (truncated)"
}

// summarizeTestOutput condenses raw test output for the reviewer.
// Pass: "All tests passed." + last 10 lines.
// Fail: "Tests FAILED:" + last 40 lines.
func summarizeTestOutput(raw string, passed bool) string {
	if passed {
		tail := lastNLines(raw, 10)
		return "All tests passed.\n\n" + tail
	}
	tail := lastNLines(raw, 40)
	return "Tests FAILED:\n\n" + tail
}

// buildRetryFeedback creates structured markdown feedback for the execute agent
// after a needs_revision verdict.
func buildRetryFeedback(reviewText, diff string, changedFiles []string, attempt int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Review Feedback (attempt %d)\n\n", attempt)
	b.WriteString(reviewText)
	b.WriteString("\n\n## Changed Files\n\n")
	for _, f := range changedFiles {
		fmt.Fprintf(&b, "- %s\n", f)
	}
	b.WriteString("\n## Current Diff\n\n")
	b.WriteString(diff)
	return b.String()
}

// buildTestFailureFeedback creates structured markdown feedback for the execute
// agent after a test failure.
func buildTestFailureFeedback(testSummary, diff string, attempt int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Test Failure (attempt %d)\n\n", attempt)
	b.WriteString(testSummary)
	b.WriteString("\n\n## Current Diff\n\n")
	b.WriteString(diff)
	return b.String()
}

// lastNLines returns the last n lines of a string.
func lastNLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
