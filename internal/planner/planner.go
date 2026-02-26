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
	cfg Config
}

// New creates a planner with the given config.
func New(cfg Config) *Planner {
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 2
	}
	if cfg.TestCmd == "" {
		cfg.TestCmd = "go test ./..."
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Minute
	}
	return &Planner{cfg: cfg}
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
		_ = execOutput // logged by progress, used for context

		// Step 2: Test
		progress("Running tests...")
		testOutput, testOk := p.runTests(ctx)
		if !testOk {
			diff := p.getDiff(ctx)
			tail := lastNLines(testOutput, 40)
			feedback = fmt.Sprintf("Tests failed:\n%s\n\nYour current changes on disk:\n%s", tail, diff)
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
		progress("Reviewing changes...")
		verdict, reviewText := p.review(ctx, task, diffOutput, testOutput)
		progress(fmt.Sprintf("Review verdict: %s", verdict))

		switch verdict {
		case VerdictDone:
			return TaskResult{Task: task, Verdict: VerdictDone, Summary: reviewText, Attempts: attempt + 1}
		case VerdictNeedsHuman:
			return TaskResult{Task: task, Verdict: VerdictNeedsHuman, Summary: reviewText, Attempts: attempt + 1, Diff: diffOutput, TestOutput: testOutput}
		case VerdictNeedsRevision:
			feedback = reviewText + "\n\nYour current changes on disk:\n" + diffOutput
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
		_ = execOutput

		progress("Running tests...")
		testOutput, testOk := p.runTests(ctx)
		if !testOk {
			d := p.getDiff(ctx)
			tail := lastNLines(testOutput, 40)
			feedback = fmt.Sprintf("Tests failed:\n%s\n\nYour current changes on disk:\n%s", tail, d)
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

		progress("Reviewing changes...")
		verdict, reviewText := p.review(ctx, task, diffOutput, testOutput)
		progress(fmt.Sprintf("Review verdict: %s", verdict))

		switch verdict {
		case VerdictDone:
			return TaskResult{Task: task, Verdict: VerdictDone, Summary: reviewText, Attempts: attempt + 1}
		case VerdictNeedsHuman:
			return TaskResult{Task: task, Verdict: VerdictNeedsHuman, Summary: reviewText, Attempts: attempt + 1, Diff: diffOutput, TestOutput: testOutput}
		case VerdictNeedsRevision:
			feedback = reviewText + "\n\nYour current changes on disk:\n" + diffOutput
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

	prompt := fmt.Sprintf(`You are an autonomous coding agent. Complete this task fully in this session.

## Your Task
%s
%s%s%s
## Rules
- Complete the ENTIRE task in this session
- Run '%s' before finishing to make sure nothing is broken
- Keep changes minimal and focused on the task
- If the task is ambiguous or would require violating a convention, explain what you need clarified instead of guessing`, task, feedbackBlock, conventionsBlock, contextBlock, p.cfg.TestCmd)

	return p.runClaude(ctx, prompt)
}

// review runs a second Claude invocation to evaluate the diff.
func (p *Planner) review(ctx context.Context, task, diffOutput, testOutput string) (Verdict, string) {
	conventionsBlock := ""
	if p.cfg.Conventions != "" {
		conventionsBlock = fmt.Sprintf(`
## Project Conventions
%s`, p.cfg.Conventions)
	}

	prompt := fmt.Sprintf(`You are a strict code reviewer evaluating whether an automated agent completed a task correctly.
You are the last line of defense before code is accepted. Be critical.

## Task That Was Assigned
%s
%s
## Git Diff (what the agent changed)
%s

## Test Results
%s

## Your Job
Evaluate the changes against BOTH the task description AND the project conventions.

Check for correctness:
- Does the diff actually implement what the task asked for?
- Is the implementation correct and idiomatic?
- Are there bugs, edge cases, or missing error handling?

Check for convention violations (MUST flag as needs_human):
- Does it change the database schema without the task explicitly calling for it?
- Does it add new CLI subcommands, data models, or Store interface methods that aren't specified?
- Does it break backwards compatibility?
- Does it touch more than 8 non-test files?

Check for scope creep (flag as needs_revision or needs_human):
- Did the agent make design decisions that the task didn't specify?
- Did the agent add features beyond what was asked?
- Is the agent guessing at requirements instead of keeping to what's specified?

You MUST respond with EXACTLY one of these three verdicts on the FIRST line:
VERDICT: done
VERDICT: needs_revision
VERDICT: needs_human

Rules for choosing:
- done: Implementation matches the task, follows conventions, no scope creep. If tests pass and the diff matches the task intent, use done.
- needs_revision: Has bugs or incomplete work the agent can fix (give specific feedback)
- needs_human: Use ONLY for clear convention violations or fundamentally wrong approaches. Do NOT use needs_human just because the agent made minor judgement calls.`, task, conventionsBlock, diffOutput, testOutput)

	output, err := p.runClaude(ctx, prompt)
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

// runTests executes the test command and returns output + success.
func (p *Planner) runTests(ctx context.Context) (string, bool) {
	testCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(testCtx, "sh", "-c", p.cfg.TestCmd)
	if p.cfg.WorkDir != "" {
		cmd.Dir = p.cfg.WorkDir
	}

	output, err := cmd.CombinedOutput()
	return string(output), err == nil
}

// getDiff returns the current git diff including untracked files.
func (p *Planner) getDiff(ctx context.Context) string {
	run := func(args ...string) string {
		cmd := exec.CommandContext(ctx, "git", args...)
		if p.cfg.WorkDir != "" {
			cmd.Dir = p.cfg.WorkDir
		}
		out, _ := cmd.Output()
		return string(out)
	}

	diff := run("diff")

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
// permissions bypassed. Used for task execution and review where the planner's
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

// lastNLines returns the last n lines of a string.
func lastNLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
