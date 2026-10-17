package rpc

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

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

// Auto steward guardrails (docs/DESIGN-STEWARD-AUTO.md), enforced here and
// not only in the prompt.
const (
	// stewardAutoWindow allows one auto step per project in this window,
	// shadow ones included, so shadow shows what "on" would do.
	stewardAutoWindow = 7 * 24 * time.Hour
	// stewardDeclineWindow: after the family said no to a steward proposal
	// in a project, nothing there runs without asking for this long.
	stewardDeclineWindow = 30 * 24 * time.Hour
)

// autoRefusal says why an auto proposal for slug may not be filed now, or ""
// when it may. Each refusal tells the agent to file it as a do instead.
func (s *Server) autoRefusal(slug string, now time.Time) string {
	if s.stewardAuto != "shadow" && s.stewardAuto != "on" {
		return "auto is off for this agent; file it as DO (a yes/no proposal) instead"
	}
	declined, err := s.store.StewardDeclinedSince(slug, now.Add(-stewardDeclineWindow))
	if err != nil {
		return "could not check this project's earlier proposals; file it as DO instead"
	}
	if declined != nil {
		return fmt.Sprintf("the family said no to proposal #%d in this project within 30 days; file it as DO (a yes/no proposal) instead", declined.ID)
	}
	past, err := s.store.StewardForProject(slug, now.Add(-stewardAutoWindow), now.Add(time.Second))
	if err != nil {
		return "could not check this project's earlier proposals; file it as DO instead"
	}
	for _, p := range past {
		if store.StewardKind(p.Evidence) == "auto" {
			return fmt.Sprintf("auto proposal #%d already ran (or was shadowed) in this project this week; file this one as DO instead", p.ID)
		}
	}
	return ""
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
			if store.StewardKind(req.Evidence) == "auto" {
				if msg := s.autoRefusal(p.Slug, time.Now()); msg != "" {
					writeError(w, http.StatusConflict, msg)
					return
				}
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
		// A thread proposal replaces the project's owner-only (shadow) ones:
		// the family answers it, so the owner's copies are withdrawn.
		var replaced []store.Suggestion
		if sg.Source == store.SuggestionSourceSteward && sg.Audience != "owner" && sg.Audience != "" && store.StewardKind(sg.Evidence) != "auto" {
			replaced, _ = s.store.OwnerOnlyStewardFor(sg.Project)
		}
		id, err := s.store.CreateSuggestion(sg)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, old := range replaced {
			if err := s.store.DecideSuggestion(old.ID, store.SuggestionWithdrawn, "steward", fmt.Sprintf("re-filed in the project's thread as #%d", id)); err != nil {
				slog.Warn("steward: withdraw owner-only proposal", "id", old.ID, "error", err)
			}
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
