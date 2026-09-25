package rpc

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/rcliao/shell/internal/store"
)

// POST /suggestion — the agent side of the suggestion loop (docs/DESIGN-
// ROUTER-AND-SUGGESTIONS.md, S0). create files one during a review; list
// shows open and recent ones; decide records the owner's answer and is only
// accepted from the owner's chat, so an agent cannot accept its own
// suggestion from a system turn. The operator CLI writes the store directly.

// SuggestionRequest is the body of POST /suggestion.
type SuggestionRequest struct {
	Action   string `json:"action"` // create | list | decide | withdraw
	ChatID   int64  `json:"chat_id"`
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Evidence string `json:"evidence"`
	Change   string `json:"change"`
	Status   string `json:"status"` // decide: accepted | declined | done
	Note     string `json:"note"`
	All      bool   `json:"all"`
}

func (s *Server) handleSuggestion(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store not available")
		return
	}
	var req SuggestionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	switch req.Action {
	case "create":
		if req.Title == "" || req.Change == "" {
			writeError(w, http.StatusBadRequest, "title and change are required")
			return
		}
		id, err := s.store.CreateSuggestion(store.Suggestion{Title: req.Title, Evidence: req.Evidence, Change: req.Change})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"id": id, "status": store.SuggestionProposed,
			"result": fmt.Sprintf("Suggestion #%d filed; it reaches the owner when this review ends.", id)})

	case "list", "":
		statuses := []string{store.SuggestionProposed, store.SuggestionDelivered, store.SuggestionAccepted}
		if req.All {
			statuses = nil
		}
		list, err := s.store.ListSuggestions(statuses, 30)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"suggestions": list})

	case "decide":
		if s.ownerChatID == 0 || req.ChatID != s.ownerChatID {
			writeError(w, http.StatusForbidden, "decisions are recorded only in the owner's chat, from the owner's own words")
			return
		}
		if req.ID == 0 || req.Status == "" {
			writeError(w, http.StatusBadRequest, "id and status are required")
			return
		}
		if req.Status == store.SuggestionWithdrawn {
			writeError(w, http.StatusBadRequest, "use action=withdraw to withdraw your own suggestion")
			return
		}
		if err := s.store.DecideSuggestion(req.ID, req.Status, "owner", req.Note); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]any{"result": fmt.Sprintf("Suggestion #%d marked %s.", req.ID, req.Status)})

	case "withdraw":
		if err := s.store.DecideSuggestion(req.ID, store.SuggestionWithdrawn, "agent", req.Note); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]any{"result": fmt.Sprintf("Suggestion #%d withdrawn.", req.ID)})

	default:
		writeError(w, http.StatusBadRequest, "action must be create, list, decide or withdraw")
	}
}
