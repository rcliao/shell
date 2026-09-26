package rpc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/rcliao/shell/internal/store"
)

// POST /lane — the agent labels the message it is answering with the lane
// it really belongs to (R0, docs/DESIGN-ROUTER-AND-SUGGESTIONS.md). Labels
// only score the router in shadow; nothing about the turn changes.

// LaneRequest is the body of POST /lane.
type LaneRequest struct {
	ChatID   int64  `json:"chat_id"`
	ThreadID int64  `json:"thread_id"`
	Lane     string `json:"lane"`
	Note     string `json:"note"`
}

func (s *Server) handleLane(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store not available")
		return
	}
	var req LaneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.ChatID == 0 {
		writeError(w, http.StatusBadRequest, "no current chat: lanes label messages in a conversation")
		return
	}
	valid := map[string]bool{"general": true}
	var names []string
	if ps, err := s.store.ListProjects(req.ChatID); err == nil {
		for _, p := range ps {
			if p.Status == "active" {
				valid[p.Slug] = true
				names = append(names, p.Slug)
			}
		}
	}
	if !valid[req.Lane] {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("lane must be general or one of this chat's active projects: %v", names))
		return
	}
	text, err := s.store.LastUserText(req.ChatID, req.ThreadID)
	if err != nil || text == "" {
		writeError(w, http.StatusNotFound, "no user message to label in this conversation")
		return
	}
	if strings.HasPrefix(strings.TrimSpace(text), "[") {
		writeError(w, http.StatusBadRequest, "the message you are answering is not from a person (a relayed or scheduled turn); only human messages are labelled")
		return
	}
	if err := s.store.UpsertRouteLabel(store.RouteLabel{ChatID: req.ChatID, ThreadID: req.ThreadID,
		TextHash: store.TextHash(text), Lane: req.Lane, Source: "agent", Sure: true, Note: req.Note}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"result": "Labelled the current message as " + req.Lane + "."})
}
