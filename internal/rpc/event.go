package rpc

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/rcliao/shell/internal/store"
)

// POST /event — the agent's side of external events
// (docs/DESIGN-HEARTBEAT-AGENDA-EVENTS.md): list them, and mark one done
// (acted on) or ignored (not worth acting on), with a note.

// EventRequest is the body of POST /event.
type EventRequest struct {
	Action string `json:"action"` // list | done | ignore
	ID     int64  `json:"id"`
	Note   string `json:"note"`
	All    bool   `json:"all"`
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store not available")
		return
	}
	var req EventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	switch req.Action {
	case "list", "":
		statuses := []string{store.EventNew, store.EventSeen}
		if req.All {
			statuses = nil
		}
		evs, err := s.store.ListEvents(statuses, 30)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"events": evs})
	case "done", "ignore":
		status := store.EventDone
		if req.Action == "ignore" {
			status = store.EventIgnored
		}
		if err := s.store.MarkEvent(req.ID, status, req.Note); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]any{"result": fmt.Sprintf("Event #%d marked %s.", req.ID, status)})
	default:
		writeError(w, http.StatusBadRequest, "action must be list, done or ignore")
	}
}
