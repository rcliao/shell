package bridge

import (
	"log/slog"
	"regexp"
	"strings"

	"github.com/rcliao/shell/internal/transcript"
)

// Task directive regexes for A2A-inspired agent-to-agent task delegation.
//
// Create: [task to=agent_name]description[/task]
// Result: [task-result id=task_id]result text[/task-result]
// Status: [task-status id=task_id status=working]
var (
	taskCreateRe = regexp.MustCompile(`(?s)\[task\s+to=(\w+)\](.*?)\[/task\]`)
	taskResultRe = regexp.MustCompile(`(?s)\[task-result\s+id=([a-f0-9]+)\](.*?)\[/task-result\]`)
	taskStatusRe = regexp.MustCompile(`\[task-status\s+id=([a-f0-9]+)\s+status=(\w+)\]`)
)

// parseTaskDirectives extracts and processes task directives from an agent's
// response. Creates/completes/updates tasks in the shared transcript store.
// Returns the response with directives stripped.
func (b *Bridge) parseTaskDirectives(chatID int64, response string) string {
	if b.transcript == nil {
		return response
	}

	// Parse [task to=...]...[/task] — create new tasks.
	response = taskCreateRe.ReplaceAllStringFunc(response, func(match string) string {
		sub := taskCreateRe.FindStringSubmatch(match)
		if len(sub) < 3 {
			return match
		}
		toAgent := strings.TrimSpace(sub[1])
		description := strings.TrimSpace(sub[2])
		if toAgent == "" || description == "" {
			return match
		}

		taskID, err := b.transcript.CreateTask(chatID, b.agentBotUsername, toAgent, description)
		if err != nil {
			slog.Warn("failed to create delegated task", "to", toAgent, "error", err)
			return match
		}
		slog.Info("task created", "id", taskID, "from", b.agentBotUsername, "to", toAgent, "description", description)
		return "(delegated task " + taskID + " to " + toAgent + ")"
	})

	// Parse [task-result id=...]...[/task-result] — complete tasks.
	response = taskResultRe.ReplaceAllStringFunc(response, func(match string) string {
		sub := taskResultRe.FindStringSubmatch(match)
		if len(sub) < 3 {
			return match
		}
		taskID := strings.TrimSpace(sub[1])
		result := strings.TrimSpace(sub[2])

		task, err := b.transcript.GetTask(taskID)
		if err != nil || task == nil {
			slog.Warn("task-result for unknown task", "id", taskID, "error", err)
			return match
		}

		if err := b.transcript.CompleteTask(taskID, result); err != nil {
			slog.Warn("failed to complete task", "id", taskID, "error", err)
			return match
		}
		slog.Info("task completed", "id", taskID, "by", b.agentBotUsername)

		// Truncate result preview for inline replacement.
		preview := result
		if len(preview) > 100 {
			preview = preview[:100] + "..."
		}
		return "(task " + taskID + " completed: " + preview + ")"
	})

	// Parse [task-status id=... status=...] — update task status.
	response = taskStatusRe.ReplaceAllStringFunc(response, func(match string) string {
		sub := taskStatusRe.FindStringSubmatch(match)
		if len(sub) < 3 {
			return match
		}
		taskID := strings.TrimSpace(sub[1])
		status := strings.TrimSpace(sub[2])

		switch status {
		case transcript.TaskWorking, transcript.TaskFailed, transcript.TaskCanceled:
			if status == transcript.TaskFailed {
				if err := b.transcript.FailTask(taskID, "agent reported failure"); err != nil {
					slog.Warn("failed to update task status", "id", taskID, "error", err)
				}
			} else {
				if err := b.transcript.UpdateTaskStatus(taskID, status); err != nil {
					slog.Warn("failed to update task status", "id", taskID, "error", err)
				}
			}
			slog.Info("task status updated", "id", taskID, "status", status)
		default:
			slog.Warn("unknown task status", "id", taskID, "status", status)
		}
		return ""
	})

	return response
}
