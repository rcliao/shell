package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/planner"
	"github.com/rcliao/shell/internal/worktree"
)

// planState represents where a plan is in its lifecycle.
type planState string

const (
	planStateIdle      planState = "idle"
	planStateDrafting  planState = "drafting"
	planStateExecuting planState = "executing"
	planStateBlocked   planState = "blocked"
	planStateDone      planState = "done"
)

// repoWorktree holds worktree state for a single repository.
type repoWorktree struct {
	repoDir string           // resolved git repo path
	path    string           // worktree checkout path
	branch  string           // git branch name
	planner *planner.Planner // planner with this worktree as WorkDir
}

// planRun tracks the state of an active or completed plan execution.
type planRun struct {
	cancel    context.CancelFunc
	results   []planner.TaskResult
	progress  []string
	done      bool
	startedAt time.Time

	// Drafting state
	state     planState
	draftPlan string
	intent    string

	// Blocked state: index of the task that needs human guidance
	failedTaskIdx int
	failedRepo    string // repo name of the failed task (for multi-repo routing)

	// Multi-repo worktree isolation
	repoWorktrees map[string]*repoWorktree // repo name → worktree info

	// Available repo names discovered at plan time
	repoNames []string

	// Legacy single-repo fields kept for backwards compat with non-repo-grouped plans
	worktreeRepoDir string           // resolved git repo directory (source of the worktree)
	worktreePath    string           // filesystem path to the worktree checkout
	worktreeBranch  string           // git branch name for the worktree
	execPlanner     *planner.Planner // planner configured with worktree WorkDir (nil = use bridge default)
}

// Plan starts plan drafting. If the input contains checklist tasks, it skips
// drafting and executes directly (backwards compatible). Otherwise it asks Claude
// to generate a plan from the intent, entering drafting state.
func (b *Bridge) Plan(ctx context.Context, chatID int64, input string) (string, error) {
	if b.plan == nil {
		return "Planner is not configured. Set planner.enabled=true in config.", nil
	}

	input = strings.TrimSpace(input)
	if input == "" {
		return "Usage: /plan <what you want to do>\n\nDescribe your goal and I'll draft a plan.", nil
	}

	b.planMu.Lock()
	if existing, ok := b.planRuns[chatID]; ok && !existing.done && existing.state != planStateDone {
		b.planMu.Unlock()
		return "A plan is already active. Use /planstop to cancel it first.", nil
	}
	b.planMu.Unlock()

	// If the input already contains checklist tasks, execute directly (backwards compat).
	if tasks := planner.ParsePlan(input); len(tasks) > 0 {
		return b.startExecution(ctx, chatID, input, input)
	}

	// Discover available repos for repo-aware plan generation.
	var repoNames []string
	if b.repoDir != "" {
		repos := worktree.ListRepos(b.repoDir)
		for name := range repos {
			repoNames = append(repoNames, name)
		}
		sort.Strings(repoNames)
	}

	// Otherwise, draft a plan from the intent.
	draft, err := b.plan.DraftPlan(ctx, input, "", "", repoNames...)
	if err != nil {
		return "", fmt.Errorf("failed to generate plan: %w", err)
	}

	b.planMu.Lock()
	b.planRuns[chatID] = &planRun{
		state:     planStateDrafting,
		draftPlan: draft,
		intent:    input,
		startedAt: time.Now(),
		repoNames: repoNames,
	}
	b.planMu.Unlock()

	return formatDraftResponse(draft), nil
}

// handlePlanDraft processes user messages while in drafting state.
func (b *Bridge) handlePlanDraft(ctx context.Context, chatID int64, userMsg string) (string, error) {
	b.planMu.Lock()
	run := b.planRuns[chatID]
	b.planMu.Unlock()

	normalized := strings.TrimSpace(strings.ToLower(userMsg))

	switch normalized {
	case "go":
		return b.startExecution(ctx, chatID, run.draftPlan, run.intent)
	case "stop":
		b.planMu.Lock()
		delete(b.planRuns, chatID)
		b.planMu.Unlock()
		return "Plan cancelled.", nil
	default:
		// Treat as revision feedback.
		revised, err := b.plan.DraftPlan(ctx, run.intent, run.draftPlan, userMsg, run.repoNames...)
		if err != nil {
			return "", fmt.Errorf("failed to revise plan: %w", err)
		}
		b.planMu.Lock()
		run.draftPlan = revised
		b.planMu.Unlock()
		return formatDraftResponse(revised), nil
	}
}

// handlePlanBlocked processes user messages while in blocked state.
// "stop" cancels the plan; anything else is treated as guidance to retry the failed task.
func (b *Bridge) handlePlanBlocked(ctx context.Context, chatID int64, userMsg string) (string, error) {
	b.planMu.Lock()
	run := b.planRuns[chatID]
	failedIdx := run.failedTaskIdx
	planText := run.draftPlan
	failedRepo := run.failedRepo
	b.planMu.Unlock()

	normalized := strings.TrimSpace(strings.ToLower(userMsg))
	if normalized == "stop" {
		b.planMu.Lock()
		delete(b.planRuns, chatID)
		b.planMu.Unlock()
		return "Plan cancelled.", nil
	}

	// Determine the failed task and the correct planner.
	var failedTask string
	var tasks []string // flat task list for legacy context building

	repoTasks := planner.ParsePlanByRepo(planText)
	isMultiRepo := len(repoTasks) > 0

	if isMultiRepo && failedIdx < len(repoTasks) {
		failedTask = repoTasks[failedIdx].Task
		for _, rt := range repoTasks {
			tasks = append(tasks, rt.Task)
		}
	} else {
		tasks = planner.ParsePlan(planText)
		if failedIdx < len(tasks) {
			failedTask = tasks[failedIdx]
		}
	}

	planCtx, cancel := context.WithCancel(context.Background())

	b.planMu.Lock()
	run.cancel = cancel
	run.state = planStateExecuting
	completedCtx := buildCompletedContext(tasks, run.results, failedIdx)

	// Route to the correct repo's planner for multi-repo plans.
	var execPlan *planner.Planner
	if isMultiRepo && failedRepo != "" {
		if rw, ok := run.repoWorktrees[failedRepo]; ok {
			execPlan = rw.planner
		}
	}
	if execPlan == nil {
		execPlan = run.execPlanner
	}
	if execPlan == nil {
		execPlan = b.plan
	}
	b.planMu.Unlock()

	progress := func(msg string) {
		b.planMu.Lock()
		run.progress = append(run.progress, msg)
		b.planMu.Unlock()
		if b.transport != nil {
			b.transport.Notify(chatID, msg)
		}
	}

	b.runBackground(planCtx, fmt.Sprintf("plan-%d-retry", chatID), "plan retry with guidance",
		map[string]string{"type": "plan", "chat": fmt.Sprint(chatID)},
		func(ctx context.Context) error {
			defer cancel()
			progress(fmt.Sprintf("Retrying task %d/%d with guidance: %s", failedIdx+1, len(tasks), failedTask))

			result := execPlan.RunTaskWithGuidance(ctx, failedTask, userMsg, completedCtx, progress)

			b.planMu.Lock()
			// Replace the failed result with the new one.
			run.results[failedIdx] = result
			b.planMu.Unlock()

			if result.Verdict != planner.VerdictDone {
				b.planMu.Lock()
				run.state = planStateBlocked
				b.planMu.Unlock()
				if b.transport != nil {
					b.transport.Notify(chatID, b.formatPlanSummary(run))
				}
				return nil
			}

			execPlan.GitCheckpoint(ctx, failedTask)
			updatedCtx := completedCtx + fmt.Sprintf("- %s: %s\n", failedTask, result.Summary)

			// Continue with remaining tasks.
			if isMultiRepo && failedIdx+1 < len(repoTasks) {
				// Multi-repo: continue with remaining repo tasks.
				remaining := repoTasks[failedIdx+1:]
				b.executeMultiRepoFrom(ctx, chatID, run, remaining, updatedCtx, progress)
			} else if failedIdx+1 < len(tasks) {
				remaining := execPlan.RunPlanFrom(ctx, planText, failedIdx+1, updatedCtx, progress)

				b.planMu.Lock()
				run.results = append(run.results, remaining...)

				lastIdx := len(remaining) - 1
				if lastIdx >= 0 && remaining[lastIdx].Verdict == planner.VerdictNeedsHuman {
					run.state = planStateBlocked
					run.failedTaskIdx = failedIdx + 1 + lastIdx
					run.done = false
				} else {
					run.state = planStateDone
					run.done = true
				}
				b.planMu.Unlock()
			} else {
				b.planMu.Lock()
				run.state = planStateDone
				run.done = true
				b.planMu.Unlock()
			}

			b.storeReviewerLearnings(ctx, run)
			b.cleanupWorktree(run, chatID)

			if b.transport != nil {
				b.transport.Notify(chatID, b.formatPlanSummary(run))
			}
			return nil
		})

	return fmt.Sprintf("Retrying task %d with your guidance. Use /planstatus to check progress.", failedIdx+1), nil
}

// startExecution transitions to executing and runs the plan in a background goroutine.
// intent is used to resolve which git repo to create a worktree from when the
// workspace contains multiple repositories.
func (b *Bridge) startExecution(ctx context.Context, chatID int64, planText, intent string) (string, error) {
	// Try repo-grouped parsing first; fall back to flat.
	repoTasks := planner.ParsePlanByRepo(planText)
	isMultiRepo := len(repoTasks) > 0

	if !isMultiRepo {
		flatTasks := planner.ParsePlan(planText)
		if len(flatTasks) == 0 {
			return "No tasks found in plan.", nil
		}
	}

	planCtx, cancel := context.WithCancel(context.Background())
	run := &planRun{
		cancel:        cancel,
		state:         planStateExecuting,
		draftPlan:     planText,
		intent:        intent,
		startedAt:     time.Now(),
		repoWorktrees: make(map[string]*repoWorktree),
	}

	if isMultiRepo && b.useWorktree && b.repoDir != "" {
		// Multi-repo: create one worktree per unique repo.
		availableRepos := worktree.ListRepos(b.repoDir)
		seen := map[string]bool{}
		for _, rt := range repoTasks {
			if seen[rt.Repo] {
				continue
			}
			seen[rt.Repo] = true

			repoPath, ok := availableRepos[rt.Repo]
			if !ok {
				// Try ResolveRepoDir as fallback
				resolved, err := worktree.ResolveRepoDir(b.repoDir, rt.Repo)
				if err != nil {
					slog.Warn("worktree: unknown repo in plan, skipping worktree", "repo", rt.Repo, "error", err)
					continue
				}
				repoPath = resolved
			}

			wtPath, branch, err := worktree.Create(repoPath, b.worktreeDir, chatID)
			if err != nil {
				slog.Warn("worktree: failed to create for repo", "repo", rt.Repo, "error", err)
				continue
			}

			pl := b.plan.CloneWithWorkDir(wtPath)
			pl = b.injectReviewerMemory(ctx, pl)

			run.repoWorktrees[rt.Repo] = &repoWorktree{
				repoDir: repoPath,
				path:    wtPath,
				branch:  branch,
				planner: pl,
			}
			slog.Info("plan execution using worktree", "chat_id", chatID, "repo", rt.Repo, "branch", branch, "path", wtPath)
		}
	} else if !isMultiRepo && b.useWorktree && b.repoDir != "" {
		// Legacy single-repo path
		repoDir, err := worktree.ResolveRepoDir(b.repoDir, intent)
		if err != nil {
			slog.Warn("worktree: could not resolve repo, running without isolation", "error", err)
		} else {
			wtPath, branch, err := worktree.Create(repoDir, b.worktreeDir, chatID)
			if err != nil {
				cancel()
				return "", fmt.Errorf("failed to create worktree: %w", err)
			}
			run.worktreeRepoDir = repoDir
			run.worktreePath = wtPath
			run.worktreeBranch = branch
			execPlan := b.plan.CloneWithWorkDir(wtPath)
			execPlan = b.injectReviewerMemory(ctx, execPlan)
			run.execPlanner = execPlan
			slog.Info("plan execution using worktree", "chat_id", chatID, "repo", repoDir, "branch", branch, "path", wtPath)
		}
	}

	// Ensure legacy execPlanner is set for non-multi-repo plans.
	if run.execPlanner == nil && !isMultiRepo {
		execPlan := b.plan
		execPlan = b.injectReviewerMemory(ctx, execPlan)
		run.execPlanner = execPlan
	}

	b.planMu.Lock()
	b.planRuns[chatID] = run
	b.planMu.Unlock()

	progress := func(msg string) {
		b.planMu.Lock()
		run.progress = append(run.progress, msg)
		b.planMu.Unlock()

		if b.transport != nil {
			b.transport.Notify(chatID, msg)
		}
	}

	if isMultiRepo {
		b.runBackground(planCtx, fmt.Sprintf("plan-%d", chatID), "plan execution (multi-repo)",
			map[string]string{"type": "plan", "chat": fmt.Sprint(chatID)},
			func(ctx context.Context) error {
				defer cancel()
				b.executeMultiRepo(ctx, chatID, run, repoTasks, progress)
				return nil
			})

		var branches []string
		for repo, rw := range run.repoWorktrees {
			branches = append(branches, fmt.Sprintf("%s: %s", repo, rw.branch))
		}
		sort.Strings(branches)
		extra := ""
		if len(branches) > 0 {
			extra = "\nWorktree branches:\n" + strings.Join(branches, "\n")
		}
		return fmt.Sprintf("Plan started with %d tasks across %d repos. Progress will be reported as tasks complete.\nUse /planstatus to check, /planstop to cancel.%s",
			len(repoTasks), len(run.repoWorktrees), extra), nil
	}

	// Legacy flat plan execution
	b.runBackground(planCtx, fmt.Sprintf("plan-%d", chatID), "plan execution",
		map[string]string{"type": "plan", "chat": fmt.Sprint(chatID)},
		func(ctx context.Context) error {
			defer cancel()
			results := run.execPlanner.RunPlan(ctx, planText, progress)

			b.planMu.Lock()
			run.results = results
			lastIdx := len(results) - 1
			if lastIdx >= 0 && results[lastIdx].Verdict == planner.VerdictNeedsHuman {
				run.state = planStateBlocked
				run.failedTaskIdx = lastIdx
				run.done = false
			} else {
				run.state = planStateDone
				run.done = true
			}
			b.planMu.Unlock()

			b.storeReviewerLearnings(ctx, run)
			b.cleanupWorktree(run, chatID)

			if b.transport != nil {
				b.transport.Notify(chatID, b.formatPlanSummary(run))
			}
			return nil
		})

	extra := ""
	if run.worktreeBranch != "" {
		extra = fmt.Sprintf("\nWorktree branch: %s", run.worktreeBranch)
	}
	return fmt.Sprintf("Plan started with %d tasks. Progress will be reported as tasks complete.\nUse /planstatus to check, /planstop to cancel.%s", len(planner.ParsePlan(planText)), extra), nil
}

// executeMultiRepo runs repo-grouped tasks sequentially, routing each to the
// correct repo's planner. Stops on first needs_human.
func (b *Bridge) executeMultiRepo(ctx context.Context, chatID int64, run *planRun, repoTasks []planner.RepoTask, progress planner.ProgressFunc) {
	total := len(repoTasks)
	progress(fmt.Sprintf("Plan has %d tasks across repos.", total))
	var completedContext string

	for i, rt := range repoTasks {
		rw, ok := run.repoWorktrees[rt.Repo]
		if !ok {
			progress(fmt.Sprintf("Skipping task %d/%d (no worktree for repo %s): %s", i+1, total, rt.Repo, rt.Task))
			continue
		}

		progress(fmt.Sprintf("\n=== Task %d/%d [%s]: %s ===", i+1, total, rt.Repo, rt.Task))
		result := rw.planner.RunTask(ctx, rt.Task, completedContext, progress)

		b.planMu.Lock()
		run.results = append(run.results, result)
		b.planMu.Unlock()

		if result.Verdict != planner.VerdictDone {
			progress(fmt.Sprintf("Task %d stopped: %s", i+1, result.Verdict))
			b.planMu.Lock()
			run.state = planStateBlocked
			run.failedTaskIdx = i
			run.failedRepo = rt.Repo
			run.done = false
			b.planMu.Unlock()

			b.storeReviewerLearningsMultiRepo(ctx, run)

			if b.transport != nil {
				b.transport.Notify(chatID, b.formatPlanSummary(run))
			}
			return
		}

		rw.planner.GitCheckpoint(ctx, rt.Task)
		completedContext += fmt.Sprintf("- [%s] %s: %s\n", rt.Repo, rt.Task, result.Summary)
		progress(fmt.Sprintf("Task %d/%d: DONE", i+1, total))

		if i < total-1 {
			time.Sleep(3 * time.Second)
		}
	}

	b.planMu.Lock()
	run.state = planStateDone
	run.done = true
	b.planMu.Unlock()

	b.storeReviewerLearningsMultiRepo(ctx, run)
	b.cleanupWorktree(run, chatID)

	if b.transport != nil {
		b.transport.Notify(chatID, b.formatPlanSummary(run))
	}
}

// executeMultiRepoFrom continues multi-repo execution from a slice of remaining
// repo tasks, using the given completedContext. Called when resuming after a
// blocked task is resolved.
func (b *Bridge) executeMultiRepoFrom(ctx context.Context, chatID int64, run *planRun, remaining []planner.RepoTask, completedContext string, progress planner.ProgressFunc) {
	total := len(remaining)
	for i, rt := range remaining {
		rw, ok := run.repoWorktrees[rt.Repo]
		if !ok {
			progress(fmt.Sprintf("Skipping task (no worktree for repo %s): %s", rt.Repo, rt.Task))
			continue
		}

		progress(fmt.Sprintf("\n=== Continuing [%s]: %s ===", rt.Repo, rt.Task))
		result := rw.planner.RunTask(ctx, rt.Task, completedContext, progress)

		b.planMu.Lock()
		run.results = append(run.results, result)
		b.planMu.Unlock()

		if result.Verdict != planner.VerdictDone {
			b.planMu.Lock()
			run.state = planStateBlocked
			// Calculate absolute index from original repoTasks
			allRepoTasks := planner.ParsePlanByRepo(run.draftPlan)
			run.failedTaskIdx = len(allRepoTasks) - total + i
			run.failedRepo = rt.Repo
			run.done = false
			b.planMu.Unlock()
			return
		}

		rw.planner.GitCheckpoint(ctx, rt.Task)
		completedContext += fmt.Sprintf("- [%s] %s: %s\n", rt.Repo, rt.Task, result.Summary)

		if i < total-1 {
			time.Sleep(3 * time.Second)
		}
	}

	b.planMu.Lock()
	run.state = planStateDone
	run.done = true
	b.planMu.Unlock()
}

// storeReviewerLearningsMultiRepo persists reviewer learnings from all repo planners.
func (b *Bridge) storeReviewerLearningsMultiRepo(ctx context.Context, run *planRun) {
	if b.memory == nil {
		return
	}
	for _, rw := range run.repoWorktrees {
		if rw.planner == nil {
			continue
		}
		ns := reviewerNamespace(rw.planner.WorkDir())
		for _, result := range run.results {
			for _, learning := range result.ReviewerLearnings {
				if err := b.memory.StoreReviewerLearning(ctx, ns, learning); err != nil {
					slog.Warn("failed to store reviewer learning", "ns", ns, "error", err)
				}
			}
		}
	}
}

// cleanupWorktree handles worktree lifecycle at the end of a plan.
// On success (all done): merge branch into main repo and remove worktree.
// On blocked/failure: remove worktree but keep the branch for inspection.
func (b *Bridge) cleanupWorktree(run *planRun, chatID int64) {
	selfModified := false

	// Multi-repo cleanup
	if len(run.repoWorktrees) > 0 {
		if run.state == planStateDone && run.done {
			for repo, rw := range run.repoWorktrees {
				if err := worktree.MergeAndCleanup(rw.repoDir, rw.path, rw.branch); err != nil {
					slog.Warn("worktree merge failed", "repo", repo, "branch", rw.branch, "error", err)
					run.progress = append(run.progress, fmt.Sprintf("Worktree merge failed for %s: %v\nBranch %s is still available for manual merge.", repo, err, rw.branch))
				} else {
					slog.Info("worktree merged and cleaned up", "repo", repo, "branch", rw.branch)
					if b.isSelfRepo(rw.repoDir) {
						selfModified = true
					}
				}
			}
		}
		// For blocked/stopped state, worktrees are cleaned up by PlanStop
		if selfModified {
			b.triggerSelfRestart(run, chatID)
		}
		return
	}

	// Legacy single-repo cleanup
	if run.worktreePath == "" {
		return
	}

	if run.state == planStateDone && run.done {
		if err := worktree.MergeAndCleanup(run.worktreeRepoDir, run.worktreePath, run.worktreeBranch); err != nil {
			slog.Warn("worktree merge failed", "branch", run.worktreeBranch, "error", err)
			run.progress = append(run.progress, fmt.Sprintf("Worktree merge failed: %v\nBranch %s is still available for manual merge.", err, run.worktreeBranch))
			return
		}
		slog.Info("worktree merged and cleaned up", "branch", run.worktreeBranch)
		if b.isSelfRepo(run.worktreeRepoDir) {
			selfModified = true
		}
	}

	if selfModified {
		b.triggerSelfRestart(run, chatID)
	}
}

// isSelfRepo checks if repoDir matches shell's own source directory.
func (b *Bridge) isSelfRepo(repoDir string) bool {
	if b.selfSourceDir == "" || repoDir == "" {
		return false
	}
	// Resolve symlinks for reliable comparison.
	selfReal, err1 := filepath.EvalSymlinks(b.selfSourceDir)
	repoReal, err2 := filepath.EvalSymlinks(repoDir)
	if err1 != nil || err2 != nil {
		return b.selfSourceDir == repoDir
	}
	return selfReal == repoReal
}

// triggerSelfRestart notifies the user and triggers a rebuild + restart.
func (b *Bridge) triggerSelfRestart(run *planRun, chatID int64) {
	if b.onSelfRestart == nil {
		return
	}
	slog.Info("self-modification detected after plan merge, scheduling rebuild+restart")
	run.progress = append(run.progress, "Changes affect shell itself — rebuilding and restarting...")
	// Give a short delay so the notification can be sent before restart.
	transport := b.transport
	go func() {
		time.Sleep(2 * time.Second)
		b.onSelfRestart()
		// If we get here, restart failed (exec replaces the process on success).
		msg := "Self-restart failed. Relay continues running with old code."
		slog.Error("self-restart: onSelfRestart returned (rebuild likely failed)")
		if transport != nil && chatID != 0 {
			transport.Notify(chatID, msg)
		}
	}()
}

func formatDraftResponse(draft string) string {
	return fmt.Sprintf("Here's the proposed plan:\n\n%s\n\nReply 'go' to execute, send edits to revise, or 'stop' to cancel.", draft)
}

// PlanStatus returns the current state of a running or completed plan.
func (b *Bridge) PlanStatus(chatID int64) (string, error) {
	b.planMu.Lock()
	run, ok := b.planRuns[chatID]
	if !ok {
		b.planMu.Unlock()
		return "No plan has been run. Use /plan to start one.", nil
	}

	state := run.state
	results := run.results
	elapsed := time.Since(run.startedAt).Truncate(time.Second)
	progressCount := len(run.progress)
	draft := run.draftPlan
	wtBranch := run.worktreeBranch
	repoWTs := run.repoWorktrees
	b.planMu.Unlock()

	switch state {
	case planStateDrafting:
		return fmt.Sprintf("Plan: DRAFTING\n\n%s\n\nReply 'go' to execute, send edits to revise, or 'stop' to cancel.", draft), nil

	case planStateExecuting:
		var sb strings.Builder
		sb.WriteString("Plan: RUNNING\n")
		sb.WriteString(fmt.Sprintf("Elapsed: %s\n", elapsed))
		if len(repoWTs) > 0 {
			sb.WriteString("Worktree branches:\n")
			for repo, rw := range repoWTs {
				sb.WriteString(fmt.Sprintf("  %s: %s\n", repo, rw.branch))
			}
		} else if wtBranch != "" {
			sb.WriteString(fmt.Sprintf("Worktree branch: %s\n", wtBranch))
		}
		sb.WriteString(fmt.Sprintf("Progress messages: %d\n\n", progressCount))

		if len(results) > 0 {
			sb.WriteString("Results:\n")
			for i, r := range results {
				icon := verdictIcon(r.Verdict)
				sb.WriteString(fmt.Sprintf("%d. [%s] %s (%d attempts)\n", i+1, icon, r.Task, r.Attempts))
			}
		}
		return sb.String(), nil

	case planStateBlocked:
		return b.formatBlockedSummary(run), nil

	case planStateDone:
		var sb strings.Builder
		sb.WriteString("Plan: COMPLETED\n")
		sb.WriteString(fmt.Sprintf("Elapsed: %s\n\n", elapsed))

		if len(results) > 0 {
			sb.WriteString("Results:\n")
			for i, r := range results {
				icon := verdictIcon(r.Verdict)
				sb.WriteString(fmt.Sprintf("%d. [%s] %s (%d attempts)\n", i+1, icon, r.Task, r.Attempts))
			}
		}
		return sb.String(), nil

	default:
		return "No plan has been run. Use /plan to start one.", nil
	}
}

// PlanStop cancels a plan from either drafting or executing state.
func (b *Bridge) PlanStop(chatID int64) (string, error) {
	b.planMu.Lock()
	run, ok := b.planRuns[chatID]
	if !ok {
		b.planMu.Unlock()
		return "No plan is currently active.", nil
	}

	state := run.state
	if state == planStateDone {
		b.planMu.Unlock()
		return "Plan already completed. Nothing to stop.", nil
	}

	if run.cancel != nil {
		run.cancel()
	}

	// Clean up worktrees
	var branches []string
	for repo, rw := range run.repoWorktrees {
		worktree.Cleanup(rw.repoDir, rw.path, rw.branch)
		branches = append(branches, fmt.Sprintf("%s: %s", repo, rw.branch))
	}
	if run.worktreePath != "" {
		worktree.Cleanup(run.worktreeRepoDir, run.worktreePath, run.worktreeBranch)
		branches = append(branches, run.worktreeBranch)
	}

	delete(b.planRuns, chatID)
	b.planMu.Unlock()

	suffix := ""
	if len(branches) > 0 {
		sort.Strings(branches)
		suffix = fmt.Sprintf("\nWorktrees removed. Branches kept for inspection:\n%s", strings.Join(branches, "\n"))
	}

	switch state {
	case planStateDrafting:
		return "Draft cancelled." + suffix, nil
	case planStateBlocked:
		return "Blocked plan cancelled." + suffix, nil
	case planStateExecuting:
		return "Plan execution cancelled." + suffix, nil
	default:
		return "Plan cleared." + suffix, nil
	}
}

func verdictIcon(v planner.Verdict) string {
	switch v {
	case planner.VerdictDone:
		return "ok"
	case planner.VerdictNeedsHuman:
		return "BLOCKED"
	case planner.VerdictNeedsRevision:
		return "retry"
	default:
		return "?"
	}
}

// formatPlanSummary creates a human-readable summary of plan results.
func (b *Bridge) formatPlanSummary(run *planRun) string {
	results := run.results
	if len(results) == 0 {
		return "Plan finished with no results."
	}

	// Blocked state: show actionable diagnostic info.
	if run.state == planStateBlocked {
		return b.formatBlockedSummary(run)
	}

	var sb strings.Builder
	sb.WriteString("--- Plan Complete ---\n\n")

	done := 0
	for _, r := range results {
		if r.Verdict == planner.VerdictDone {
			done++
		}
	}
	sb.WriteString(fmt.Sprintf("Tasks: %d/%d completed\n\n", done, len(results)))

	for i, r := range results {
		icon := verdictIcon(r.Verdict)
		sb.WriteString(fmt.Sprintf("%d. [%s] %s\n", i+1, icon, r.Task))
		if r.Verdict == planner.VerdictNeedsHuman {
			summary := r.Summary
			if len(summary) > 500 {
				summary = summary[:500] + "..."
			}
			sb.WriteString(fmt.Sprintf("   Reason: %s\n", summary))
		}
	}

	return sb.String()
}

// formatBlockedSummary creates an actionable summary when a plan is blocked.
func (b *Bridge) formatBlockedSummary(run *planRun) string {
	results := run.results
	totalTasks := len(planner.ParsePlan(run.draftPlan))
	blocked := results[run.failedTaskIdx]

	var sb strings.Builder
	sb.WriteString("--- Plan Blocked ---\n\n")

	// Show completed tasks first
	for i, r := range results {
		icon := verdictIcon(r.Verdict)
		sb.WriteString(fmt.Sprintf("Task %d/%d: [%s] %s\n", i+1, totalTasks, icon, r.Task))
	}

	sb.WriteString(fmt.Sprintf("\nAttempts: %d\n", blocked.Attempts))

	if blocked.Diff != "" {
		diff := blocked.Diff
		if len(diff) > 1000 {
			diff = diff[:1000] + "\n... (truncated)"
		}
		sb.WriteString(fmt.Sprintf("\nChanges on disk:\n%s\n", diff))
	}

	if blocked.TestOutput != "" {
		testOut := blocked.TestOutput
		if len(testOut) > 500 {
			testOut = testOut[:500] + "\n... (truncated)"
		}
		sb.WriteString(fmt.Sprintf("\nTest output:\n%s\n", testOut))
	}

	if blocked.Summary != "" {
		summary := blocked.Summary
		if len(summary) > 500 {
			summary = summary[:500] + "\n... (truncated)"
		}
		sb.WriteString(fmt.Sprintf("\nReviewer feedback:\n%s\n", summary))
	}

	sb.WriteString("\nReply with guidance to retry, or use:\n/planskip — skip this task\n/planretry — retry automatically\n/planstop — cancel the plan")
	return sb.String()
}

// reviewerNamespace returns the memory namespace for reviewer learnings.
func reviewerNamespace(workDir string) string {
	return "reviewer:" + workDir
}

// injectReviewerMemory queries reviewer memory and returns a planner with
// critical flows set. Returns the original planner if memory is unavailable.
func (b *Bridge) injectReviewerMemory(ctx context.Context, pl *planner.Planner) *planner.Planner {
	if b.memory == nil {
		return pl
	}
	ns := reviewerNamespace(pl.WorkDir())
	flows := b.memory.ReviewerContext(ctx, ns, "critical flows verification review", 500)
	if flows == "" {
		return pl
	}
	return pl.WithCriticalFlows(flows)
}

// storeReviewerLearnings persists reviewer learnings from a plan run.
func (b *Bridge) storeReviewerLearnings(ctx context.Context, run *planRun) {
	if b.memory == nil {
		return
	}
	execPlan := run.execPlanner
	if execPlan == nil {
		return
	}
	ns := reviewerNamespace(execPlan.WorkDir())
	for _, result := range run.results {
		for _, learning := range result.ReviewerLearnings {
			if err := b.memory.StoreReviewerLearning(ctx, ns, learning); err != nil {
				slog.Warn("failed to store reviewer learning", "ns", ns, "error", err)
			}
		}
	}
}

// buildCompletedContext creates a summary string from completed task results.
func buildCompletedContext(tasks []string, results []planner.TaskResult, upToIdx int) string {
	var sb strings.Builder
	for i := 0; i < upToIdx && i < len(results); i++ {
		if results[i].Verdict == planner.VerdictDone {
			task := ""
			if i < len(tasks) {
				task = tasks[i]
			} else {
				task = results[i].Task
			}
			sb.WriteString(fmt.Sprintf("- %s: %s\n", task, results[i].Summary))
		}
	}
	return sb.String()
}

// PlanSkip skips the currently blocked task and continues with the next one.
func (b *Bridge) PlanSkip(chatID int64) (string, error) {
	if b.plan == nil {
		return "Planner is not configured.", nil
	}

	b.planMu.Lock()
	run, ok := b.planRuns[chatID]
	if !ok || run.state != planStateBlocked {
		b.planMu.Unlock()
		return "No plan is currently blocked. Nothing to skip.", nil
	}

	failedIdx := run.failedTaskIdx
	planText := run.draftPlan

	// Determine task list — multi-repo or flat.
	repoTasks := planner.ParsePlanByRepo(planText)
	isMultiRepo := len(repoTasks) > 0

	var tasks []string
	if isMultiRepo {
		for _, rt := range repoTasks {
			tasks = append(tasks, rt.Task)
		}
	} else {
		tasks = planner.ParsePlan(planText)
	}

	if failedIdx+1 >= len(tasks) {
		run.state = planStateDone
		run.done = true
		b.planMu.Unlock()
		b.cleanupWorktree(run, chatID)
		return "Skipped last task. Plan complete.", nil
	}

	planCtx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	run.state = planStateExecuting
	completedCtx := buildCompletedContext(tasks, run.results, failedIdx)

	// Resolve exec planner for non-multi-repo.
	var execPlan *planner.Planner
	if !isMultiRepo {
		execPlan = run.execPlanner
		if execPlan == nil {
			execPlan = b.plan
		}
	}
	b.planMu.Unlock()

	progress := func(msg string) {
		b.planMu.Lock()
		run.progress = append(run.progress, msg)
		b.planMu.Unlock()
		if b.transport != nil {
			b.transport.Notify(chatID, msg)
		}
	}

	b.runBackground(planCtx, fmt.Sprintf("plan-%d-skip", chatID), "plan skip and continue",
		map[string]string{"type": "plan", "chat": fmt.Sprint(chatID)},
		func(ctx context.Context) error {
			defer cancel()
			progress(fmt.Sprintf("Skipped task %d, continuing from task %d.", failedIdx+1, failedIdx+2))

			if isMultiRepo {
				remaining := repoTasks[failedIdx+1:]
				b.executeMultiRepoFrom(ctx, chatID, run, remaining, completedCtx, progress)

				// Check if executeMultiRepoFrom left us in done state.
				b.planMu.Lock()
				if run.state != planStateBlocked {
					run.state = planStateDone
					run.done = true
				}
				b.planMu.Unlock()
			} else {
				remaining := execPlan.RunPlanFrom(ctx, planText, failedIdx+1, completedCtx, progress)

				b.planMu.Lock()
				run.results = append(run.results, remaining...)
				lastIdx := len(remaining) - 1
				if lastIdx >= 0 && remaining[lastIdx].Verdict == planner.VerdictNeedsHuman {
					run.state = planStateBlocked
					run.failedTaskIdx = failedIdx + 1 + lastIdx
					run.done = false
				} else {
					run.state = planStateDone
					run.done = true
				}
				b.planMu.Unlock()
			}

			b.cleanupWorktree(run, chatID)

			if b.transport != nil {
				b.transport.Notify(chatID, b.formatPlanSummary(run))
			}
			return nil
		})

	return fmt.Sprintf("Skipping task %d, continuing from task %d. Use /planstatus to check progress.", failedIdx+1, failedIdx+2), nil
}

// PlanRetry retries the blocked task with generic guidance.
func (b *Bridge) PlanRetry(ctx context.Context, chatID int64) (string, error) {
	if b.plan == nil {
		return "Planner is not configured.", nil
	}

	b.planMu.Lock()
	run, ok := b.planRuns[chatID]
	if !ok || run.state != planStateBlocked {
		b.planMu.Unlock()
		return "No plan is currently blocked. Nothing to retry.", nil
	}
	b.planMu.Unlock()

	return b.handlePlanBlocked(ctx, chatID, "Try again, addressing any issues from the previous attempt.")
}
