package bridge

import (
	"log/slog"
	"regexp"
	"strings"

	"github.com/rcliao/shell/internal/transcript"
)

// Task directive regexes — fallback for when agents emit text directives
// instead of using the shell-task skill. The skill is the preferred path.
var (
	taskCreateRe = regexp.MustCompile(`(?s)\[task\s+to=(\w+)\](.*?)\[/task\]`)
	taskResultRe = regexp.MustCompile(`(?s)\[task-result\s+id=([a-f0-9]+)\](.*?)\[/task-result\]`)
	taskStatusRe = regexp.MustCompile(`\[task-status\s+id=([a-f0-9]+)\s+status=(\w+)\]`)
)

// parseTaskDirectives extracts and processes task directives from an agent's
// response. Routes through TaskStore (preferred) or falls back to transcript Store.
// Returns the response with directives stripped.
func (b *Bridge) parseTaskDirectives(chatID int64, response string) string {
	if b.taskStore == nil {
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

		taskID, err := b.taskStore.CreateTask(transcript.Task{
			ChatID:      chatID,
			FromAgent:   b.agentBotUsername,
			ToAgent:     toAgent,
			Description: description,
		})
		if err != nil {
			slog.Warn("failed to create delegated task", "to", toAgent, "error", err)
			return match
		}
		slog.Info("task created via directive", "id", taskID, "from", b.agentBotUsername, "to", toAgent)
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

		if err := b.taskStore.CompleteTask(taskID, result); err != nil {
			slog.Warn("failed to complete task via directive", "id", taskID, "error", err)
			return match
		}
		slog.Info("task completed via directive", "id", taskID, "by", b.agentBotUsername)

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
				b.taskStore.FailTask(taskID, "agent reported failure")
			} else {
				b.taskStore.UpdateTaskStatus(taskID, status)
			}
			slog.Info("task status updated via directive", "id", taskID, "status", status)
		default:
			slog.Warn("unknown task status in directive", "id", taskID, "status", status)
		}
		return ""
	})

	return response
}
