package rpc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// Agent-facing surface for the durable task queue.
//
// This is what turns the queue from a reliability mechanism for the scheduler
// into infrastructure the agents themselves use: an agent can assign durable
// work, come back later, and read the result. The queue guarantees the work
// runs and runs once; the agent decides what the work IS.
//
// Distinct from POST /task, which is the cross-agent delegation system with its
// own string-keyed store. This one is the SQLite queue in shell.db.

// QueueRequest is the request body for POST /queue.
type QueueRequest struct {
	Action string `json:"action"` // create | list | get | complete
	ID     int64  `json:"id"`
	// Prompt is the work itself, run as an agent turn.
	Prompt string `json:"prompt"`
	Title  string `json:"title"`
	ChatID int64  `json:"chat_id"`
	// NotBefore delays visibility, e.g. "check this again tomorrow at 09:00".
	NotBefore string `json:"not_before"`
	Result    string `json:"result"`
	State     string `json:"state"`
	Limit     int    `json:"limit"`
}

// AgentTaskKind is the queue kind whose handler runs the payload as an agent
// turn. Kept in sync with the scheduler's handler registration.
const AgentTaskKind = "agent.task"

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store not available")
		return
	}
	var req QueueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Action == "" {
		req.Action = "create"
	}

	switch req.Action {
	case "create":
		s.queueCreate(w, req)
	case "list":
		s.queueList(w, req)
	case "get":
		s.queueGet(w, req)
	case "complete":
		s.queueComplete(w, req)
	default:
		writeError(w, http.StatusBadRequest, "action must be create, list, get or complete")
	}
}

func (s *Server) queueCreate(w http.ResponseWriter, req QueueRequest) {
	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required — it is the work to be done")
		return
	}
	payload, err := json.Marshal(map[string]any{
		"prompt":  req.Prompt,
		"title":   req.Title,
		"chat_id": req.ChatID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	t := store.Task{
		Kind:   AgentTaskKind,
		Source: store.TaskSourceAgent,
		// No natural key exists for self-assigned work, so the key is a content
		// hash: a turn that retries its own tool call must not enqueue the work
		// twice.
		IdempotencyKey: store.DeriveIdempotencyKey(AgentTaskKind, req.Prompt, fmt.Sprint(req.ChatID)),
		PartitionKey:   fmt.Sprintf("chat:%d", req.ChatID),
		Payload:        string(payload),
		// Retries are not replays. Re-running an agent turn produces SIMILAR
		// work, not the same work — a differently worded message, a second
		// Notion row. One attempt, then surface the failure to a human.
		MaxAttempts: 1,
	}
	if req.NotBefore != "" {
		when, err := time.Parse(time.RFC3339, req.NotBefore)
		if err != nil {
			writeError(w, http.StatusBadRequest, "not_before must be RFC3339, e.g. 2026-08-06T09:00:00Z")
			return
		}
		utc := when.UTC()
		t.NotBefore = &utc
	}

	id, created, err := s.store.EnqueueTask(t)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := "existing"
	if created {
		status = "created"
	}
	writeJSON(w, map[string]any{
		"id": id, "status": status,
		"note": "runs on a worker, at most once. Poll action=get for state and result.",
	})
}

func (s *Server) queueList(w http.ResponseWriter, req QueueRequest) {
	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	tasks, err := s.store.ListTasks(req.State, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, summarizeTask(t))
	}
	writeJSON(w, map[string]any{"tasks": out, "count": len(out)})
}

func (s *Server) queueGet(w http.ResponseWriter, req QueueRequest) {
	if req.ID == 0 {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	t, err := s.store.GetTask(req.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if t == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("task %d not found", req.ID))
		return
	}
	writeJSON(w, summarizeTask(*t))
}

// queueComplete records the RESULT of a running task. It deliberately does not
// change state: the worker holding the lease owns that.
//
// Completion has to be evidence rather than assertion. An agent saying "done!"
// means nothing — this codebase learned that expensively, which is why write
// verification exists. So a task is complete only when this call was actually
// made, observable in tool_uses; a turn that ends without one fails.
func (s *Server) queueComplete(w http.ResponseWriter, req QueueRequest) {
	if req.ID == 0 {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if req.Result == "" {
		writeError(w, http.StatusBadRequest, "result is required — it is the evidence the work was done")
		return
	}
	if err := s.store.SetTaskResult(req.ID, req.Result); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"id": req.ID, "status": "result recorded"})
}

func summarizeTask(t store.Task) map[string]any {
	row := map[string]any{
		"id": t.ID, "kind": t.Kind, "state": t.State, "source": t.Source,
		"attempts": t.Attempts, "max_attempts": t.MaxAttempts,
		"enqueued_at": t.EnqueuedAt.UTC().Format(time.RFC3339),
	}
	var p struct {
		Title  string `json:"title"`
		Prompt string `json:"prompt"`
	}
	if json.Unmarshal([]byte(t.Payload), &p) == nil {
		if p.Title != "" {
			row["title"] = p.Title
		} else if p.Prompt != "" {
			row["title"] = firstLine(p.Prompt, 80)
		}
	}
	if t.Result != "" {
		row["result"] = t.Result
	}
	if t.LastError != "" {
		row["last_error"] = t.LastError
	}
	if t.NotBefore != nil {
		row["not_before"] = t.NotBefore.UTC().Format(time.RFC3339)
	}
	return row
}

func firstLine(s string, max int) string {
	for i, r := range s {
		if r == '\n' {
			s = s[:i]
			break
		}
	}
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
