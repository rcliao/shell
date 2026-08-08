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
	// chat action
	ExternalID string `json:"external_id"`
	ThreadID   int64  `json:"thread_id"`
	SenderName string `json:"sender_name"`
}

// MessageTurnKind mirrors scheduler.TaskKindMessageTurn.
const MessageTurnKind = "message.turn"

// chatTTL bounds how long an unstarted chat turn stays worth running.
//
// Staleness is a TIME question, not an attempts question. Long enough to
// survive a deploy or a crash-restart; short enough that nobody is answered
// about something they have moved past. Past it the turn is dropped with a
// recorded outcome rather than delivered late.
const chatTTL = 10 * time.Minute

// chatMaxAttempts is one normal run plus one crash replay.
const chatMaxAttempts = 2

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
	switch req.Action {
	case "list":
		s.queueList(w, req)
	case "get":
		s.queueGet(w, req)
	case "chat":
		s.queueChat(w, req)
	default:
		writeError(w, http.StatusBadRequest, "action must be chat, list or get")
	}
}

func (s *Server) queueChat(w http.ResponseWriter, req QueueRequest) {
	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	m := map[string]any{
		"transport":   "cli",
		"external_id": req.ExternalID,
		"chat_id":     req.ChatID,
		"thread_id":   req.ThreadID,
		"sender_name": req.SenderName,
		"text":        req.Prompt,
	}
	payload, err := json.Marshal(m)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	expires := time.Now().UTC().Add(chatTTL)
	id, created, err := s.store.EnqueueTask(store.Task{
		Kind:   MessageTurnKind,
		Source: store.TaskSourceAgent,
		// Natural key: the caller supplies an id, so re-running the same
		// command is a redelivery rather than a second question.
		IdempotencyKey: "cli:" + req.ExternalID,
		PartitionKey:   fmt.Sprintf("chat:%d:%d", req.ChatID, req.ThreadID),
		Payload:        string(payload),
		// One normal run plus ONE crash replay.
		//
		// This was max_attempts=1 with a comment claiming a re-run would answer
		// a question the person had moved on from. That reasoning is what
		// expires_at is for, and using the attempt budget for it made chat
		// at-most-once — which docs/TASKS.md lists as an explicit non-goal: "a
		// duplicated reminder is a nuisance and a dropped one is a failure."
		// The person is still waiting; a crash 20 seconds in should still get
		// answered.
		MaxAttempts: chatMaxAttempts,
		ExpiresAt:   &expires,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := "queued"
	if !created {
		status = "existing"
	}
	writeJSON(w, map[string]any{"id": id, "status": status})
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
