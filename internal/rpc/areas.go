package rpc

import (
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/rcliao/shell/internal/store"
)

// Project areas (docs/DESIGN-PROJECT-AREAS.md). An area is an umbrella
// project; the projects filed under it get their own places, a forum post
// on Discord or a forum topic on Telegram, created here through Places so
// the agent never handles raw platform ids.

// Places creates and manages the threads that are projects' own places. The
// daemon picks the platform from placeRef ("discord:<forum id>" or
// "telegram").
type Places interface {
	CreateThread(chatID int64, placeRef, title, content string, tags []string) (threadID int64, err error)
	SetThreadTags(chatID, threadID int64, placeRef string, tags []string) error
	ArchiveThread(chatID, threadID int64, placeRef string) error
}

var discordPlaceRef = regexp.MustCompile(`^discord:[0-9]{17,20}$`)

func validPlaceRef(ref string) bool {
	return ref == "" || ref == "telegram" || discordPlaceRef.MatchString(ref)
}

// areaFor resolves req.Area to an area row. It returns (nil, "") when the
// request names no area, and an error message when the area is unusable.
func (s *Server) areaFor(req ProjectRequest) (*store.Project, string) {
	if req.Area == "" {
		if req.Place == "auto" {
			return nil, "place=auto needs an area (its place_ref says where places go)"
		}
		return nil, ""
	}
	a, err := s.store.GetProjectBySlug(req.Area)
	if err != nil || a == nil {
		return nil, "no area " + req.Area
	}
	if a.Kind != store.ProjectKindArea {
		return nil, req.Area + " is a project, not an area (create areas with kind=area)"
	}
	if a.Status == "archived" {
		return nil, "area " + req.Area + " is archived"
	}
	if req.Place == "auto" && a.PlaceRef == "" {
		return nil, "area " + req.Area + " has no place_ref; set one before using place=auto"
	}
	return a, ""
}

// placeTitle is the thread name: the project's emoji and title.
func placeTitle(p *store.Project) string {
	return strings.TrimSpace(p.Emoji + " " + p.Title)
}

// createPlace opens p's own place in its area and binds p to it. It returns
// the thread id, or a warning when the place could not be made or bound.
func (s *Server) createPlace(p, area *store.Project, content string) (int64, string) {
	if s.places == nil {
		return 0, "places are not available (no platform bot); project left unbound"
	}
	var tags []string
	if p.Stage != "" {
		tags = []string{p.Stage}
	}
	thread, err := s.places.CreateThread(p.ChatID, area.PlaceRef, placeTitle(p), content, tags)
	if err != nil {
		slog.Warn("rpc: project place create failed", "slug", p.Slug, "place_ref", area.PlaceRef, "error", err)
		return 0, "place not created: " + err.Error()
	}
	if err := s.store.UpdateProjectFields(p.Slug, store.ProjectFieldUpdate{MessageThreadID: &thread}); err != nil {
		return 0, fmt.Sprintf("place %d created but not bound: %v", thread, err)
	}
	slog.Info("rpc: project place created", "slug", p.Slug, "thread", thread, "place_ref", area.PlaceRef)
	return thread, ""
}

// placeOf returns the area whose place_ref p's thread lives under, or nil
// when p has no managed place (no area, no thread, or an area without one).
func (s *Server) placeOf(p *store.Project) *store.Project {
	if p.Area == "" || p.MessageThreadID == 0 {
		return nil
	}
	a, err := s.store.GetProjectBySlug(p.Area)
	if err != nil || a == nil || a.PlaceRef == "" || a.ChatID != p.ChatID {
		return nil
	}
	return a
}

func (s *Server) closePlace(p *store.Project) string {
	a := s.placeOf(p)
	if a == nil || s.places == nil {
		return ""
	}
	if err := s.places.ArchiveThread(p.ChatID, p.MessageThreadID, a.PlaceRef); err != nil {
		slog.Warn("rpc: project place close failed", "slug", p.Slug, "error", err)
		return "archived, but its place was not closed: " + err.Error()
	}
	return ""
}

// projectStage sets a project's stage and mirrors it onto its place's tags.
func (s *Server) projectStage(w http.ResponseWriter, req ProjectRequest) {
	if req.Slug == "" {
		writeError(w, http.StatusBadRequest, "slug is required")
		return
	}
	p, err := s.store.GetProjectBySlug(req.Slug)
	if err != nil || p == nil {
		writeError(w, http.StatusNotFound, "no project "+req.Slug)
		return
	}
	stage := strings.TrimSpace(req.Stage)
	if err := s.store.UpdateProjectFields(p.Slug, store.ProjectFieldUpdate{Stage: &stage}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.Stage = stage
	resp := projectJSON(*p)
	if a := s.placeOf(p); a != nil && s.places != nil {
		var tags []string
		if stage != "" {
			tags = []string{stage}
		}
		if err := s.places.SetThreadTags(p.ChatID, p.MessageThreadID, a.PlaceRef, tags); err != nil {
			resp["warning"] = "stage saved, but the place's tag was not updated: " + err.Error()
		}
	}
	s.refreshProjectHome(p.ChatID)
	writeJSON(w, resp)
}

// projectMove files an existing project under an area (or out of one, with
// area ""), optionally sets its stage, and with place=auto gives it its own
// place there. This is the migration path for projects made before areas.
func (s *Server) projectMove(w http.ResponseWriter, req ProjectRequest) {
	if req.Slug == "" {
		writeError(w, http.StatusBadRequest, "slug is required")
		return
	}
	p, err := s.store.GetProjectBySlug(req.Slug)
	if err != nil || p == nil {
		writeError(w, http.StatusNotFound, "no project "+req.Slug)
		return
	}
	if p.Kind == store.ProjectKindArea {
		writeError(w, http.StatusBadRequest, "an area cannot belong to another area")
		return
	}
	area, errMsg := s.areaFor(req)
	if errMsg != "" {
		writeError(w, http.StatusBadRequest, errMsg)
		return
	}
	if area != nil && area.ChatID != p.ChatID {
		writeError(w, http.StatusBadRequest, "area and project are in different chats; bind the project to the area's chat first")
		return
	}
	if req.Place == "auto" && p.MessageThreadID != 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("project already has thread %d; it keeps its place", p.MessageThreadID))
		return
	}
	u := store.ProjectFieldUpdate{Area: &req.Area}
	if req.Stage != "" {
		u.Stage = &req.Stage
	}
	if err := s.store.UpdateProjectFields(p.Slug, u); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.Area = req.Area
	if req.Stage != "" {
		p.Stage = req.Stage
	}
	resp := map[string]any{}
	if req.Place == "auto" {
		if thread, warn := s.createPlace(p, area, req.Content); warn != "" {
			resp["warning"] = warn
		} else {
			p.MessageThreadID = thread
			resp["place_created"] = true
		}
	}
	for k, v := range projectJSON(*p) {
		resp[k] = v
	}
	s.refreshProjectHome(p.ChatID)
	writeJSON(w, resp)
}
