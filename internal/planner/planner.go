// Package planner runs a plan file through an execute → test → review loop.
// It reports progress via a callback and stops on needs_human for human input.
package planner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
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
	Task              string
	Verdict           Verdict
	Summary           string   // review output or error message
	Attempts          int
	Diff              string   // git diff at time of failure
	TestOutput        string   // test output at time of failure
	ReviewerLearnings []string // [remember] blocks extracted from reviewer output
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
	VerifyInstructions   string        // from config: E2E commands for the reviewer to run
	MaxRetries           int           // retries per task on needs_revision
	Timeout              time.Duration // per-claude-invocation timeout
	AutoApproveThreshold int           // max diff lines to auto-approve without review
	CriticalFlows        string        // injected by bridge from reviewer memory
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

// WithCriticalFlows returns a new Planner with CriticalFlows set.
func (p *Planner) WithCriticalFlows(flows string) *Planner {
	cfg := p.cfg
	cfg.CriticalFlows = flows
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
		slog.Warn("planner: snapshotBase failed", "error", err, "workdir", p.cfg.WorkDir)
		p.taskBase = ""
		return
	}
	p.taskBase = strings.TrimSpace(string(out))
	slog.Info("planner: snapshotBase", "sha", p.taskBase, "workdir", p.cfg.WorkDir)
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
	var allLearnings []string

	for attempt := 0; attempt <= p.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			progress(fmt.Sprintf("Retry %d/%d (addressing review feedback)...", attempt, p.cfg.MaxRetries))
		}

		// Step 1: Execute
		progress(fmt.Sprintf("Executing: %s", task))
		execOutput, err := p.execute(ctx, task, feedback, completedContext)
		if err != nil {
			progress(fmt.Sprintf("Execution error: %v", err))
			return TaskResult{Task: task, Verdict: VerdictNeedsHuman, Summary: err.Error(), Attempts: attempt + 1, Diff: p.getDiff(ctx), ReviewerLearnings: allLearnings}
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
			return TaskResult{Task: task, Verdict: VerdictDone, Summary: "No changes needed", Attempts: attempt + 1, ReviewerLearnings: allLearnings}
		}

		if p.shouldSkipReview(diffOutput, true) {
			progress(fmt.Sprintf("Auto-approved (%d lines, threshold %d).", diffLineCount(diffOutput), p.cfg.AutoApproveThreshold))
			return TaskResult{Task: task, Verdict: VerdictDone, Summary: "Auto-approved: tests pass, diff within threshold", Attempts: attempt + 1, ReviewerLearnings: allLearnings}
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
		verdict, reviewText, learnings := p.review(ctx, pkg)
		allLearnings = append(allLearnings, learnings...)
		progress(fmt.Sprintf("Review verdict: %s", verdict))

		switch verdict {
		case VerdictDone:
			return TaskResult{Task: task, Verdict: VerdictDone, Summary: reviewText, Attempts: attempt + 1, ReviewerLearnings: allLearnings}
		case VerdictNeedsHuman:
			return TaskResult{Task: task, Verdict: VerdictNeedsHuman, Summary: reviewText, Attempts: attempt + 1, Diff: diffOutput, TestOutput: testOutput, ReviewerLearnings: allLearnings}
		case VerdictNeedsRevision:
			feedback = buildRetryFeedback(reviewText, diffOutput, changedFiles, attempt+1)
			progress("Reviewer requested revision.")
		}
	}

	// Exhausted retries
	return TaskResult{Task: task, Verdict: VerdictNeedsHuman, Summary: "Exhausted retries. Last feedback:\n" + feedback, Attempts: p.cfg.MaxRetries + 1, Diff: p.getDiff(ctx), TestOutput: feedback, ReviewerLearnings: allLearnings}
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
	var allLearnings []string

	for attempt := 0; attempt <= p.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			progress(fmt.Sprintf("Retry %d/%d (addressing review feedback)...", attempt, p.cfg.MaxRetries))
		}

		progress(fmt.Sprintf("Executing: %s", task))
		execOutput, err := p.execute(ctx, task, feedback, completedContext)
		if err != nil {
			progress(fmt.Sprintf("Execution error: %v", err))
			return TaskResult{Task: task, Verdict: VerdictNeedsHuman, Summary: err.Error(), Attempts: attempt + 1, Diff: p.getDiff(ctx), ReviewerLearnings: allLearnings}
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
			return TaskResult{Task: task, Verdict: VerdictDone, Summary: "No changes needed", Attempts: attempt + 1, ReviewerLearnings: allLearnings}
		}

		if p.shouldSkipReview(diffOutput, true) {
			progress(fmt.Sprintf("Auto-approved (%d lines, threshold %d).", diffLineCount(diffOutput), p.cfg.AutoApproveThreshold))
			return TaskResult{Task: task, Verdict: VerdictDone, Summary: "Auto-approved: tests pass, diff within threshold", Attempts: attempt + 1, ReviewerLearnings: allLearnings}
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
		verdict, reviewText, learnings := p.review(ctx, pkg)
		allLearnings = append(allLearnings, learnings...)
		progress(fmt.Sprintf("Review verdict: %s", verdict))

		switch verdict {
		case VerdictDone:
			return TaskResult{Task: task, Verdict: VerdictDone, Summary: reviewText, Attempts: attempt + 1, ReviewerLearnings: allLearnings}
		case VerdictNeedsHuman:
			return TaskResult{Task: task, Verdict: VerdictNeedsHuman, Summary: reviewText, Attempts: attempt + 1, Diff: diffOutput, TestOutput: testOutput, ReviewerLearnings: allLearnings}
		case VerdictNeedsRevision:
			feedback = buildRetryFeedback(reviewText, diffOutput, changedFiles, attempt+1)
			progress("Reviewer requested revision.")
		}
	}

	return TaskResult{Task: task, Verdict: VerdictNeedsHuman, Summary: "Exhausted retries. Last feedback:\n" + feedback, Attempts: p.cfg.MaxRetries + 1, Diff: p.getDiff(ctx), TestOutput: feedback, ReviewerLearnings: allLearnings}
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

	slog.Info("planner: execute start",
		"task", truncate(task, 100),
		"attempt", feedback != "",
		"has_feedback", feedback != "",
	)
	start := time.Now()
	output, err := p.runClaude(ctx, prompt)
	elapsed := time.Since(start)
	if err != nil {
		slog.Error("planner: execute failed", "elapsed", elapsed, "error", err)
	} else {
		slog.Info("planner: execute done",
			"elapsed", elapsed,
			"output_len", len(output),
		)
	}
	return output, err
}

// review runs verification commands ourselves (with a hard timeout), then
// passes all evidence to a text-only Claude reviewer. No Bash tool — the
// reviewer only analyzes the diff, test results, and verification output.
// Returns verdict, review text, and any extracted learnings.
func (p *Planner) review(ctx context.Context, pkg ReviewPackage) (Verdict, string, []string) {
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

	criticalFlowsBlock := "None recorded yet."
	if p.cfg.CriticalFlows != "" {
		criticalFlowsBlock = p.cfg.CriticalFlows
	}

	slog.Info("planner: review start",
		"task", truncate(pkg.Task, 100),
		"attempt", pkg.Attempt,
		"changed_files", len(pkg.ChangedFiles),
		"diff_lines", diffLineCount(pkg.Diff),
	)

	// Run E2E verification commands ourselves with a hard timeout.
	verifyResultBlock := "No verification commands configured."
	if p.cfg.VerifyInstructions != "" {
		slog.Info("planner: verify commands start", "cmd", p.cfg.VerifyInstructions)
		vStart := time.Now()
		verifyOutput, verifyOk := p.runVerifyCommands(ctx)
		status := "PASSED"
		if !verifyOk {
			status = "FAILED"
		}
		slog.Info("planner: verify commands done",
			"elapsed", time.Since(vStart),
			"status", status,
			"output_len", len(verifyOutput),
		)
		verifyResultBlock = fmt.Sprintf("Command: %s\nStatus: %s\nOutput:\n%s",
			p.cfg.VerifyInstructions, status, truncate(verifyOutput, 3000))
	}

	// Build a robust diff header — never show "0 files" when there's diff content.
	diffHeader := "## Git Diff"
	if n := len(pkg.ChangedFiles); n > 0 {
		diffHeader = fmt.Sprintf("## Git Diff (%d files changed: %s)", n, strings.Join(pkg.ChangedFiles, ", "))
	}

	prompt := fmt.Sprintf(`You are a verification gate. An agent completed a task and ALL TESTS PASSED.
Your DEFAULT verdict is DONE.

## Task
%s
%s%s
## Known Critical Flows
%s

## E2E Verification Result
%s

%s
%s

## Verification Checklist

### 1. Scope Check
Read the diff. Do the changes relate to the task?

### 2. Completeness Check
Does the diff cover all requirements explicitly stated in the task?

### 3. E2E Verification
Review the verification result above. Did it pass?

### 4. Critical Flow Check
Based on the diff and known critical flows, are there obvious bugs?

### 5. Summary
Based on checks 1-4, give your verdict.

## Decision Rules
VERDICT: done — DEFAULT. Use when checks pass.
VERDICT: needs_revision — ONLY when a check FAILed with evidence.
VERDICT: needs_human — ONLY when approach is fundamentally wrong.

## Learning
If you discover something important about this codebase, emit:
[remember]what you learned[/remember]

Respond with EXACTLY this format:
VERDICT: done|needs_revision|needs_human
<brief explanation>`, pkg.Task, conventionsBlock, execSummaryBlock, criticalFlowsBlock, verifyResultBlock, diffHeader, pkg.Diff)

	slog.Info("planner: reviewer claude start", "prompt_len", len(prompt))
	rStart := time.Now()
	output, err := p.runClaudeTextOnly(ctx, prompt)
	rElapsed := time.Since(rStart)
	if err != nil {
		slog.Error("planner: reviewer claude failed", "elapsed", rElapsed, "error", err)
		return VerdictNeedsHuman, fmt.Sprintf("Review failed: %v", err), nil
	}
	slog.Info("planner: reviewer claude done", "elapsed", rElapsed, "output_len", len(output))

	// Extract learnings before parsing verdict.
	cleanedOutput, learnings := extractLearnings(output)

	// Extract verdict
	re := regexp.MustCompile(`VERDICT:\s*(done|needs_revision|needs_human)`)
	match := re.FindStringSubmatch(cleanedOutput)
	if match == nil {
		slog.Warn("planner: reviewer verdict unparseable", "output", truncate(cleanedOutput, 200))
		return VerdictNeedsHuman, "Could not parse reviewer verdict.\n\n" + cleanedOutput, learnings
	}

	verdict := Verdict(match[1])
	slog.Info("planner: review done",
		"verdict", verdict,
		"learnings", len(learnings),
		"elapsed_total", time.Since(rStart),
	)
	return verdict, cleanedOutput, learnings
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

	// Match deleted files: --- a/path/to/file (where +++ is /dev/null)
	reDel := regexp.MustCompile(`(?m)^--- a/(.+)$`)
	for _, m := range reDel.FindAllStringSubmatch(diff, -1) {
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

	// Fallback: parse "diff --git a/X b/Y" headers if nothing matched above.
	if len(files) == 0 {
		reGit := regexp.MustCompile(`(?m)^diff --git a/(.+) b/(.+)$`)
		for _, m := range reGit.FindAllStringSubmatch(diff, -1) {
			f := m[2] // prefer the "b/" (destination) path
			if !seen[f] {
				seen[f] = true
				files = append(files, f)
			}
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
	slog.Info("planner: tests start", "cmd", p.cfg.TestCmd)
	start := time.Now()

	testCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(testCtx, "sh", "-c", p.cfg.TestCmd)
	if p.cfg.WorkDir != "" {
		cmd.Dir = p.cfg.WorkDir
	}

	output, err := cmd.CombinedOutput()
	passed := err == nil
	slog.Info("planner: tests done",
		"elapsed", time.Since(start),
		"passed", passed,
		"output_len", len(output),
	)
	return string(output), passed
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

	// Also capture committed changes since the base (agent may have committed).
	if diff == "" && base != "HEAD" {
		diff = run("diff", "-U10", base, "HEAD")
	}

	// Log diagnostics when diff is empty — helps debug "no changes detected".
	if diff == "" {
		currentHead := strings.TrimSpace(run("rev-parse", "HEAD"))
		status := run("status", "--short")
		slog.Warn("planner: getDiff empty",
			"base", base,
			"head", currentHead,
			"same_commit", base == currentHead,
			"status_len", len(strings.TrimSpace(status)),
			"workdir", p.cfg.WorkDir,
		)
	}

	// Include new untracked files (skip binary files).
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
			// Skip binary files (contain null bytes in first 8KB).
			probe := content
			if len(probe) > 8192 {
				probe = probe[:8192]
			}
			if bytes.ContainsRune(probe, 0) {
				diff += fmt.Sprintf("\n--- new file: %s (binary, skipped) ---\n", f)
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

// verifyTimeout caps how long E2E verification commands can run.
const verifyTimeout = 60 * time.Second

// runVerifyCommands executes VerifyInstructions as a shell command with a hard
// timeout. Returns the combined stdout/stderr output and whether it succeeded.
// Returns ("", true) if no verify instructions are configured.
func (p *Planner) runVerifyCommands(ctx context.Context) (string, bool) {
	if p.cfg.VerifyInstructions == "" {
		return "", true
	}

	vCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()

	cmd := exec.CommandContext(vCtx, "sh", "-c", p.cfg.VerifyInstructions)
	cmd.Env = append(
		filterEnv(os.Environ(), "CLAUDECODE"),
		"GIT_PAGER=cat",
		"PAGER=cat",
	)
	if p.cfg.WorkDir != "" {
		cmd.Dir = p.cfg.WorkDir
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	output, err := cmd.CombinedOutput()
	if vCtx.Err() == context.DeadlineExceeded {
		return string(output) + "\n(verification timed out)", false
	}
	return string(output), err == nil
}

// runClaude spawns a Claude CLI subprocess with full tool access and
// permissions bypassed. Used for task execution where the planner's
// own test+review gates provide safety.
//
// Uses --output-format json (NOT stream-json) because stream-json
// changes Claude's agentic behavior, causing it to read/analyze without
// making file changes. A background monitor logs git diff progress
// every 30s for visibility during long-running executions.
func (p *Planner) runClaude(ctx context.Context, prompt string) (string, error) {
	return p.runClaudeWithMonitor(ctx, prompt, p.cfg.Timeout, []string{
		"--allowedTools", "Bash,Write,Edit,Read",
		"--permission-mode", "bypassPermissions",
	})
}

// runClaudeWithMonitor runs Claude with --output-format json (for correct
// tool execution) while a background goroutine periodically logs git diff
// stats to provide visibility into what's changing during long executions.
func (p *Planner) runClaudeWithMonitor(ctx context.Context, prompt string, timeout time.Duration, extraArgs []string) (string, error) {
	// Start a background progress monitor that logs git activity.
	monCtx, monCancel := context.WithCancel(ctx)
	defer monCancel()
	if p.cfg.WorkDir != "" {
		go p.monitorProgress(monCtx)
	}

	return p.runClaudeWithTimeout(ctx, prompt, timeout, extraArgs)
}

// monitorProgress periodically logs git diff stats in the working directory.
// Runs until the context is cancelled (when the execute step completes).
func (p *Planner) monitorProgress(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Check for file changes via git status.
			cmd := exec.CommandContext(ctx, "git", "diff", "--stat", "HEAD")
			cmd.Dir = p.cfg.WorkDir
			out, _ := cmd.Output()
			stat := strings.TrimSpace(string(out))

			// Also check for untracked files.
			ucmd := exec.CommandContext(ctx, "git", "ls-files", "--others", "--exclude-standard")
			ucmd.Dir = p.cfg.WorkDir
			uout, _ := ucmd.Output()
			untracked := strings.TrimSpace(string(uout))

			if stat != "" || untracked != "" {
				slog.Info("planner: progress",
					"diff_stat", truncate(stat, 500),
					"untracked", truncate(untracked, 200),
				)
			} else {
				slog.Info("planner: progress", "status", "no changes yet")
			}
		}
	}
}

// streamEvent represents a single NDJSON event from --output-format stream-json.
type streamEvent struct {
	Type    string          `json:"type"`
	Name    string          `json:"name,omitempty"`    // tool_use: tool name
	Input   json.RawMessage `json:"input,omitempty"`   // tool_use: tool input
	Result  string          `json:"result,omitempty"`  // result: final text
	IsError bool            `json:"is_error,omitempty"`
}

// runClaudeStreaming spawns Claude with --output-format stream-json and reads
// NDJSON events in real-time. Tool usage is logged as it happens, giving
// visibility into what Claude is doing during long-running executions.
func (p *Planner) runClaudeStreaming(ctx context.Context, prompt string, timeout time.Duration, extraArgs []string) (string, error) {
	procCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",
	}
	args = append(args, extraArgs...)
	if p.cfg.Model != "" {
		args = append(args, "--model", p.cfg.Model)
	}

	binary := p.cfg.ClaudeBinary
	if binary == "" {
		binary = "claude"
	}

	slog.Info("planner: spawning claude (streaming)",
		"timeout", timeout,
		"prompt_len", len(prompt),
	)
	spawnStart := time.Now()

	cmd := exec.CommandContext(procCtx, binary, args...)
	cmd.Env = append(
		filterEnv(os.Environ(), "CLAUDECODE"),
		"GIT_PAGER=cat",
		"PAGER=cat",
		"GIT_TERMINAL_PROMPT=0",
	)
	if p.cfg.WorkDir != "" {
		cmd.Dir = p.cfg.WorkDir
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("creating stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("starting claude: %w", err)
	}

	// Read NDJSON events from stdout line-by-line.
	// Using bufio.Scanner instead of json.Decoder so non-JSON lines
	// (e.g. verbose progress output) don't kill the parser.
	var finalResult string
	var toolCount int
	var lastTool string
	eventTypes := make(map[string]int) // count every event type we see
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 2*1024*1024), 2*1024*1024) // 2MB per line
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Parse into a generic map first to see all fields.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			slog.Debug("planner: stream non-json line", "line", truncate(string(line), 200))
			continue
		}

		// Extract the type field.
		var evType string
		if t, ok := raw["type"]; ok {
			json.Unmarshal(t, &evType)
		}
		eventTypes[evType]++

		// Detect tool usage from any event that contains tool info.
		// The stream-json format may use various type names.
		if isToolUseEvent(evType, raw) {
			toolCount++
			toolName := extractToolName(raw)
			lastTool = toolName
			slog.Info("planner: tool_use",
				"tool", toolName,
				"event_type", evType,
				"input", truncate(extractToolInput(raw), 200),
				"seq", toolCount,
				"elapsed", time.Since(spawnStart),
			)
		}

		if evType == "result" {
			var ev streamEvent
			json.Unmarshal(line, &ev)
			finalResult = ev.Result
			slog.Info("planner: stream result",
				"is_error", ev.IsError,
				"result_len", len(ev.Result),
				"elapsed", time.Since(spawnStart),
				"event_types", eventTypes,
			)
			break // Result is the final event — stop reading.
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("planner: stream scanner error", "error", err)
	}

	// Kill process if still running — we have what we need.
	cancel()

	err = cmd.Wait()
	spawnElapsed := time.Since(spawnStart)

	// If we got the result, the process was killed by us — that's success.
	if finalResult != "" {
		slog.Info("planner: claude done (streaming)",
			"elapsed", spawnElapsed,
			"result_len", len(finalResult),
			"tools_used", toolCount,
			"event_types", eventTypes,
		)
		return finalResult, nil
	}

	// No result event received — check for errors.
	if err != nil {
		if procCtx.Err() == context.DeadlineExceeded {
			slog.Error("planner: claude timed out",
				"type", "execute-streaming",
				"elapsed", spawnElapsed,
				"timeout", timeout,
				"tools_used", toolCount,
				"last_tool", lastTool,
				"event_types", eventTypes,
			)
			return "", fmt.Errorf("claude timed out after %s (used %d tools, last: %s)", timeout, toolCount, lastTool)
		}
		stderrStr := strings.TrimSpace(stderr.String())
		slog.Error("planner: claude failed",
			"type", "execute-streaming",
			"elapsed", spawnElapsed,
			"error", err,
			"stderr_len", len(stderrStr),
			"tools_used", toolCount,
			"event_types", eventTypes,
		)
		if stderrStr != "" {
			return "", fmt.Errorf("claude failed: %w\nstderr: %s", err, stderrStr)
		}
		return "", fmt.Errorf("claude failed: %w", err)
	}

	// Process exited without result event.
	slog.Warn("planner: claude exited without result event",
		"elapsed", spawnElapsed,
		"tools_used", toolCount,
		"event_types", eventTypes,
	)
	return "", fmt.Errorf("claude exited without producing a result")
}

// isToolUseEvent checks if a stream event represents tool usage.
// Handles multiple possible formats from Claude CLI stream-json.
func isToolUseEvent(evType string, raw map[string]json.RawMessage) bool {
	// Direct tool_use type
	if evType == "tool_use" {
		return true
	}
	// content_block_start with nested tool_use (API streaming format)
	if evType == "content_block_start" {
		if cb, ok := raw["content_block"]; ok {
			var block struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(cb, &block) == nil && block.Type == "tool_use" {
				return true
			}
		}
	}
	// Any event with a "tool" or "name" field alongside tool-like content
	if _, hasName := raw["name"]; hasName {
		if _, hasInput := raw["input"]; hasInput {
			return true
		}
	}
	return false
}

// extractToolName extracts the tool name from a stream event.
func extractToolName(raw map[string]json.RawMessage) string {
	// Try top-level "name" field
	if n, ok := raw["name"]; ok {
		var name string
		if json.Unmarshal(n, &name) == nil && name != "" {
			return name
		}
	}
	// Try nested content_block.name
	if cb, ok := raw["content_block"]; ok {
		var block struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(cb, &block) == nil && block.Name != "" {
			return block.Name
		}
	}
	// Try nested tool.name
	if t, ok := raw["tool"]; ok {
		var tool struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(t, &tool) == nil && tool.Name != "" {
			return tool.Name
		}
	}
	return "unknown"
}

// extractToolInput extracts a string representation of tool input from a stream event.
func extractToolInput(raw map[string]json.RawMessage) string {
	if inp, ok := raw["input"]; ok {
		return string(inp)
	}
	if cb, ok := raw["content_block"]; ok {
		var block struct {
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(cb, &block) == nil && len(block.Input) > 0 {
			return string(block.Input)
		}
	}
	return ""
}

func (p *Planner) runClaudeWithArgs(ctx context.Context, prompt string, extraArgs []string) (string, error) {
	return p.runClaudeWithTimeout(ctx, prompt, p.cfg.Timeout, extraArgs)
}

func (p *Planner) runClaudeWithTimeout(ctx context.Context, prompt string, timeout time.Duration, extraArgs []string) (string, error) {
	procCtx, cancel := context.WithTimeout(ctx, timeout)
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

	// Identify the invocation type from the args for logging.
	invocationType := "execute"
	for _, a := range extraArgs {
		if a == "Bash,Write,Edit" { // --disallowedTools value for text-only
			invocationType = "text-only"
			break
		}
	}
	slog.Info("planner: spawning claude",
		"type", invocationType,
		"timeout", timeout,
		"prompt_len", len(prompt),
	)
	spawnStart := time.Now()

	cmd := exec.CommandContext(procCtx, binary, args...)

	// Disable pagers — git, less, and other tools will hang forever
	// waiting for TTY input when spawned as Bash tool children.
	cmd.Env = append(
		filterEnv(os.Environ(), "CLAUDECODE"),
		"GIT_PAGER=cat",
		"PAGER=cat",
		"GIT_TERMINAL_PROMPT=0",
	)

	if p.cfg.WorkDir != "" {
		cmd.Dir = p.cfg.WorkDir
	}

	// Create a new process group so we can kill the entire tree
	// (claude + any child bash processes) on timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Kill the entire process group, not just the parent.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		spawnElapsed := time.Since(spawnStart)
		if procCtx.Err() == context.DeadlineExceeded {
			slog.Error("planner: claude timed out",
				"type", invocationType,
				"elapsed", spawnElapsed,
				"timeout", timeout,
				"stdout_len", stdout.Len(),
				"stderr_len", stderr.Len(),
			)
			return "", fmt.Errorf("claude timed out after %s", timeout)
		}
		stderrStr := strings.TrimSpace(stderr.String())
		stdoutStr := strings.TrimSpace(stdout.String())
		slog.Error("planner: claude failed",
			"type", invocationType,
			"elapsed", spawnElapsed,
			"error", err,
			"stderr_len", len(stderrStr),
			"stdout_len", len(stdoutStr),
		)
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

	slog.Info("planner: claude done",
		"type", invocationType,
		"elapsed", time.Since(spawnStart),
		"stdout_len", stdout.Len(),
	)

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

// learningRe matches [remember]...[/remember] blocks in reviewer output.
var learningRe = regexp.MustCompile(`(?s)\[remember\]\s*(.*?)\s*\[/remember\]`)

// extractLearnings parses [remember]...[/remember] blocks from text.
// Returns the cleaned text (blocks removed) and the list of learning strings.
func extractLearnings(text string) (string, []string) {
	matches := learningRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text, nil
	}

	var learnings []string
	clean := text
	for i := len(matches) - 1; i >= 0; i-- {
		loc := matches[i]
		content := strings.TrimSpace(text[loc[2]:loc[3]])
		if content != "" {
			learnings = append(learnings, content)
		}
		clean = clean[:loc[0]] + clean[loc[1]:]
	}

	// Reverse learnings to preserve original order (we iterated backwards).
	for i, j := 0, len(learnings)-1; i < j; i, j = i+1, j-1 {
		learnings[i], learnings[j] = learnings[j], learnings[i]
	}

	return strings.TrimSpace(clean), learnings
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
