package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// The agent-delegated handler.
//
// Every other handler is ordinary code: deterministic, and "it returned nil"
// means it worked. This one runs the payload as an agent turn, which is the
// point of building a queue inside an agent harness rather than using a job
// runner. The division of labor is what makes the non-determinism affordable:
// THE QUEUE OWNS DELIVERY GUARANTEES, THE AGENT OWNS JUDGMENT. Leases, retries,
// idempotency, ordering and completion are the queue's; deciding what to
// actually do is the agent's. An agent that wanders or hangs is still bounded
// by lease expiry and max_attempts.
//
// Two constraints exist that deterministic handlers do not need.
//
// Completion is EVIDENCE, not assertion. An agent narrating "done!" means
// nothing — agents in this system have reported saving things that were never
// saved, which is why internal/bridge/write_verify.go exists. So the task is
// complete only when the agent actually called shell_task complete, which
// writes a result row and is observable in tool_uses. A turn that ends without
// one fails, however confidently it described success.
//
// Retries are not replays. Re-running this handler produces SIMILAR work, not
// the same work — a differently worded message, a second Notion row. So these
// tasks are enqueued with max_attempts = 1 and a failure surfaces to a human
// instead of being gambled on again.

// TaskKindAgent is the kind whose handler runs the payload as an agent turn.
const TaskKindAgent = "agent.task"

type agentTaskPayload struct {
	Prompt string `json:"prompt"`
	Title  string `json:"title"`
	ChatID int64  `json:"chat_id"`
}

// SetAgentTaskHandler registers the agent-delegated kind. Separate from
// SetQueue because it needs a way to read results back, and because a
// deployment that wants scheduled fires without agent-assigned work should be
// able to have exactly that.
func (s *Scheduler) SetAgentTaskHandler(results TaskResults) {
	s.taskResults = results
	s.RegisterHandler(TaskKindAgent, s.handleAgentTask)
}

// TaskResults reads back what a task recorded. The agent writes the result
// through its own tool call; the worker reads it here to decide whether the
// turn actually finished the work.
type TaskResults interface {
	TaskResult(taskID int64) (string, error)
}

func (s *Scheduler) handleAgentTask(ctx context.Context, t LeasedTask) (string, error) {
	var p agentTaskPayload
	if err := json.Unmarshal([]byte(t.Payload), &p); err != nil {
		return "", fmt.Errorf("undecodable agent task payload: %w", err)
	}
	if p.Prompt == "" {
		return "", fmt.Errorf("agent task has no prompt")
	}
	if s.onPrompt == nil {
		return "", fmt.Errorf("no prompt function wired; cannot run agent tasks")
	}

	slog.Info("worker: running agent task", "task_id", t.ID, "chat_id", p.ChatID, "title", p.Title)

	prompt := fmt.Sprintf(`[Task #%d] %s

You are running a queued task. Do the work described above.

When it is done you MUST call shell_task with action="complete", id=%d, and a
result describing what you actually did. The task is not complete until that
call is made — saying you finished is not the same as finishing, and a turn
that ends without the call is recorded as failed.

If the work cannot be done, still call it with a result explaining why.`,
		t.ID, p.Prompt, t.ID)

	if err := s.onPrompt(ctx, p.ChatID, prompt); err != nil {
		return "", fmt.Errorf("agent turn failed: %w", err)
	}

	// Evidence check. The turn returning cleanly says the subprocess exited, not
	// that the work happened.
	result, err := s.taskResults.TaskResult(t.ID)
	if err != nil {
		return "", fmt.Errorf("could not read task result: %w", err)
	}
	if result == "" {
		return "", fmt.Errorf("turn ended without calling shell_task complete — no evidence the work was done")
	}
	return result, nil
}
