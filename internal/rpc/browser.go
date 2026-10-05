package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/browserhandoff"
	"github.com/rcliao/shell/internal/store"
)

// BrowserRequest is the JSON body for POST /browser (the shell_browser tool).
type BrowserRequest struct {
	Action   string `json:"action"` // handoff | watch | status | cancel
	Session  string `json:"session"`
	Reason   string `json:"reason"`
	Message  string `json:"message"`
	ChatID   int64  `json:"chat_id"`
	ThreadID int64  `json:"thread_id"` // may be a lane's session thread; mapped to the real one here
	TTLMin   int    `json:"ttl_min"`
	ID       int64  `json:"id"`
}

func (s *Server) handleBrowser(w http.ResponseWriter, r *http.Request) {
	if s.browserMgr == nil {
		writeError(w, http.StatusServiceUnavailable, `browser handoffs are not enabled for this agent ("browser": {"enabled": true} in config)`)
		return
	}
	var req BrowserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	switch req.Action {
	case "handoff", "watch":
		thread := req.ThreadID
		if s.store != nil && req.ChatID != 0 {
			thread = s.store.RealThread(req.ChatID, req.ThreadID)
		}
		mode := store.HandoffModeHandoff
		if req.Action == "watch" {
			mode = store.HandoffModeWatch
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		h, err := s.browserMgr.Open(ctx, browserhandoff.OpenRequest{
			Session: req.Session, Mode: mode, Reason: req.Reason, Message: req.Message,
			ChatID: req.ChatID, ThreadID: thread, TTL: time.Duration(req.TTLMin) * time.Minute,
		})
		if errors.Is(err, browserhandoff.ErrBusy) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, map[string]any{"result": openedText(h), "id": h.ID, "link": h.Link})
	case "status":
		if req.ID != 0 {
			h, err := s.browserMgr.Get(req.ID)
			if err != nil || h == nil {
				writeError(w, http.StatusNotFound, fmt.Sprintf("no browser handoff #%d", req.ID))
				return
			}
			writeJSON(w, map[string]any{"result": statusLine(*h)})
			return
		}
		active := s.browserMgr.Active()
		if len(active) == 0 {
			writeJSON(w, map[string]any{"result": "no open live views"})
			return
		}
		var lines []string
		for _, h := range active {
			lines = append(lines, statusLine(h))
		}
		writeJSON(w, map[string]any{"result": strings.Join(lines, "\n")})
	case "cancel":
		if req.ID == 0 {
			writeError(w, http.StatusBadRequest, "cancel needs id")
			return
		}
		if err := s.browserMgr.Cancel(req.ID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]any{"result": fmt.Sprintf("closed live view #%d", req.ID)})
	default:
		writeError(w, http.StatusBadRequest, `action must be "handoff", "watch", "status" or "cancel"`)
	}
}

func openedText(h *store.BrowserHandoff) string {
	until := h.ExpiresAt.Local().Format("15:04")
	if h.Mode == store.HandoffModeWatch {
		return fmt.Sprintf("Live view #%d is open until %s and the link is posted in the chat: %s\n"+
			"Keep working; the person only watches. Close it with shell_browser(action=\"cancel\", id=%d) when you are done.",
			h.ID, until, h.Link, h.ID)
	}
	return fmt.Sprintf("Handoff #%d is open until %s and the link is posted in the chat: %s\n"+
		"End your turn now: tell the chat briefly what you need them to do. Do NOT drive session %q until you receive "+
		"the \"[Browser handoff #%d ...]\" message — the browser skill refuses while a person holds the tab.",
		h.ID, until, h.Link, h.Session, h.ID)
}

func statusLine(h store.BrowserHandoff) string {
	return fmt.Sprintf("#%d %s session=%s status=%s expires=%s link=%s",
		h.ID, h.Mode, h.Session, h.Status, h.ExpiresAt.Local().Format("15:04"), h.Link)
}
