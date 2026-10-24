package rpc

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/rcliao/shell/internal/project"
)

// projectTake makes this agent the owner of a doc it shares with the other
// agent (docs/DESIGN-PROJECT-WIKI.md): it then polls the doc's Notion page,
// runs its research and keeps its post summary. The receiving agent takes;
// nobody pushes, because only the taker knows whether it runs research.
func (s *Server) projectTake(w http.ResponseWriter, req ProjectRequest) {
	if req.Slug == "" {
		writeError(w, http.StatusBadRequest, "slug is required")
		return
	}
	p, err := s.store.GetProjectBySlug(req.Slug)
	if err != nil || p == nil {
		writeError(w, http.StatusNotFound, "no project "+req.Slug)
		return
	}
	dir, ok := project.ManagedDocDir(s.workspaceDir, p.Slug)
	if !ok {
		writeError(w, http.StatusConflict, project.ErrNotShared.Error())
		return
	}
	prev, err := project.TakeOwner(dir, s.agentName, p.ScheduleDedupKey != "")
	switch {
	case errors.Is(err, project.ErrNotShared), errors.Is(err, project.ErrTakeNeedsResearch):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if prev != s.agentName {
		slog.Info("rpc: project ownership taken", "slug", p.Slug, "from", prev, "to", s.agentName)
	}
	writeJSON(w, map[string]any{"slug": p.Slug, "owner": s.agentName, "previous": prev})
}
