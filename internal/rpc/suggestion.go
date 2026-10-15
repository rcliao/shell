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
	// ForChat files a suggestion addressed to a chat (S1) instead of the owner.
	ForChat int64 `json:"for_chat"`
	// Source is who filed it: "" (the weekly review) or "steward" (the
	// project steward on a deep beat, sent to the owner after that beat).
	Source string `json:"source"`
	// Project is the slug a steward proposal is for (required with source
	// "steward"); it places the proposal in that project's thread.
	Project string `json:"project"`
}

// stewardMaxOpen caps undelivered steward proposals; it matches
// bridge.StewardMaxProposals (rpc cannot import bridge).
const stewardMaxOpen = 2

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
		sg := store.Suggestion{Title: req.Title, Evidence: req.Evidence, Change: req.Change}
		if req.ForChat != 0 {
			sg.Audience = store.ChatAudience(req.ForChat)
		}
		reaches := "the owner when this review ends"
		switch req.Source {
		case "":
		case store.SuggestionSourceSteward:
			if req.ForChat != 0 {
				writeError(w, http.StatusBadRequest, "steward proposals are placed by project=<slug>, not for_chat")
				return
			}
			if req.Project == "" {
				writeError(w, http.StatusBadRequest, "project=<slug> is required for a steward proposal")
				return
			}
			p, err := s.store.GetProjectBySlug(req.Project)
			if err != nil || p == nil || p.Status != "active" {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("no active project %q", req.Project))
				return
			}
			// The caps are enforced here, not only in the prompt: two per
			// beat, and one open question per project — the family answers
			// the first before the steward asks again.
			if open, err := s.store.ProposedFrom(store.SuggestionSourceSteward); err == nil && len(open) >= stewardMaxOpen {
				writeError(w, http.StatusConflict, fmt.Sprintf("already %d steward proposals waiting to be sent; file nothing more this beat", len(open)))
				return
			}
			if open, err := s.store.OpenStewardFor(p.Slug, store.StewardOpenAge); err == nil && len(open) > 0 {
				writeError(w, http.StatusConflict, fmt.Sprintf("project %s already has open steward proposal #%d; wait for its answer", p.Slug, open[0].ID))
				return
			}
			sg.Source = req.Source
			sg.Project = p.Slug
			reaches = "the owner after this heartbeat"
			if s.stewardInThreads && p.ChatID != 0 {
				sg.Audience = store.ChatAudience(p.ChatID)
				sg.ThreadID = p.MessageThreadID
				reaches = "the project's thread after this heartbeat"
			}
		default:
			writeError(w, http.StatusBadRequest, "source must be empty or steward")
			return
		}
		id, err := s.store.CreateSuggestion(sg)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"id": id, "status": store.SuggestionProposed,
			"result": fmt.Sprintf("Suggestion #%d filed; it reaches %s.", id, reaches)})

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
		if req.ID == 0 || req.Status == "" {
			writeError(w, http.StatusBadRequest, "id and status are required")
			return
		}
		// Owner suggestions are decided in the owner's chat; a chat's own
		// suggestion (S1) in that chat, from the people it was posted to.
		by := ""
		if cur, err := s.store.GetSuggestion(req.ID); err == nil && cur != nil {
			switch {
			case cur.Audience == store.ChatAudience(req.ChatID) && req.ChatID != 0:
				by = "chat"
			case cur.Audience == "owner" && s.ownerChatID != 0 && req.ChatID == s.ownerChatID:
				by = "owner"
			}
		}
		if by == "" {
			writeError(w, http.StatusForbidden, "a decision is recorded only where the suggestion was sent (the owner's chat, or the chat it was posted to), from the people there")
			return
		}
		if req.Status == store.SuggestionWithdrawn {
			writeError(w, http.StatusBadRequest, "use action=withdraw to withdraw your own suggestion")
			return
		}
		if err := s.store.DecideSuggestion(req.ID, req.Status, by, req.Note); err != nil {
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
