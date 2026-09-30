package rpc

import (
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/rcliao/shell/internal/project"
	"strings"
	"unicode"

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
	// EnsureAreaPlaces finds or creates an area's own channel and its
	// projects' forum (part 2). channelThread is the channel as a thread of
	// chatID; placeRef is where the area's projects go.
	EnsureAreaPlaces(chatID int64, name, forumName string, tags []string) (channelThread int64, placeRef string, created bool, err error)
	// FindThread returns an open place under placeRef with this title, so a
	// project joins an existing post instead of opening a second one.
	FindThread(chatID int64, placeRef, title string) (threadID int64, ok bool, err error)
	// ThreadInfo describes a thread: its title, the placeRef it lives under
	// ("" when it is not in a forum), and its tags.
	ThreadInfo(chatID, threadID int64) (title, placeRef string, tags []string, err error)
	// UpdateOpener replaces the first message of a project's own place with
	// its live summary. An error means it could not (not ours, or gone).
	UpdateOpener(chatID, threadID int64, placeRef, text string) error
	// PostSummary posts and pins a summary message of the agent's own in a
	// thread, for when it cannot edit the thread's first message.
	PostSummary(chatID, threadID int64, placeRef, text string) (msgID int64, err error)
	EditSummary(chatID, threadID, msgID int64, placeRef, text string) error
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
func (s *Server) createPlace(p, area *store.Project, content string) (thread int64, joined bool, warn string) {
	if s.places == nil {
		return 0, false, "places are not available (no platform bot); project left unbound"
	}
	var tags []string
	if p.Stage != "" {
		tags = []string{p.Stage}
	}
	// The post is the shared record between agents: an open post with this
	// title is joined, never duplicated.
	var found bool
	var err error
	thread, found, err = s.places.FindThread(p.ChatID, area.PlaceRef, p.Title)
	if err != nil {
		slog.Warn("rpc: project place lookup failed; creating", "slug", p.Slug, "error", err)
	}
	if found {
		if own := s.ownProjectOn(p.ChatID, thread, p.Slug); own != "" {
			return 0, false, fmt.Sprintf("post %d is already your project %s; this one stays unbound (archive it)", thread, own)
		}
		slog.Info("rpc: project joined an existing place", "slug", p.Slug, "thread", thread)
	} else {
		thread, err = s.places.CreateThread(p.ChatID, area.PlaceRef, placeTitle(p), content, tags)
		if err != nil {
			slog.Warn("rpc: project place create failed", "slug", p.Slug, "place_ref", area.PlaceRef, "error", err)
			return 0, false, "place not created: " + err.Error()
		}
	}
	if err := s.store.UpdateProjectFields(p.Slug, store.ProjectFieldUpdate{MessageThreadID: &thread}); err != nil {
		return 0, false, fmt.Sprintf("place %d created but not bound: %v", thread, err)
	}
	slog.Info("rpc: project place bound", "slug", p.Slug, "thread", thread, "joined", found, "place_ref", area.PlaceRef)
	return thread, found, ""
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
	s.refreshOpener(p, "")
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
		if thread, joined, warn := s.createPlace(p, area, req.Content); warn != "" {
			resp["place_warning"] = warn
		} else {
			p.MessageThreadID = thread
			resp["place_created"] = !joined
			resp["place_joined"] = joined
		}
	}
	s.linkShared(p)
	for k, v := range projectJSON(*p) {
		resp[k] = v
	}
	s.refreshProjectHome(p.ChatID)
	writeJSON(w, resp)
}

// Default stage tags for a new area's forum; an agent passes its own with
// tags (travel uses 規劃中/已訂/完成).
var defaultAreaTags = []string{"規劃中", "進行中", "完成"}

// ensureAreaPlaces handles create with kind=area, place=new: it finds or
// creates the area's channel and forum and fills in the request's thread and
// place_ref. It returns the existing area row when this agent already has
// one bound to that channel (the call is then a no-op).
func (s *Server) ensureAreaPlaces(req *ProjectRequest) (existing *store.Project, created bool, errMsg string) {
	if s.places == nil {
		return nil, false, "places are not available (no platform bot)"
	}
	name := req.ChannelName
	if name == "" {
		name = req.Title
	}
	forumName := req.ForumName
	if forumName == "" {
		forumName = name + "-projects"
	}
	tags := req.Tags
	if len(tags) == 0 {
		tags = defaultAreaTags
	}
	thread, ref, created, err := s.places.EnsureAreaPlaces(req.ChatID, name, forumName, tags)
	if err != nil {
		return nil, false, "area places not created: " + err.Error()
	}
	projects, err := s.store.ListProjects(req.ChatID)
	if err == nil {
		for i := range projects {
			p := projects[i]
			if p.Kind == store.ProjectKindArea && p.MessageThreadID == thread && p.Status != "archived" {
				// Self-heal: the row may point at a forum that lost the
				// convergence (two agents, two forum names). The channel
				// decides; follow it.
				if p.PlaceRef != ref {
					if err := s.store.UpdateProjectFields(p.Slug, store.ProjectFieldUpdate{PlaceRef: &ref}); err == nil {
						slog.Info("rpc: area place_ref healed", "slug", p.Slug, "from", p.PlaceRef, "to", ref)
						p.PlaceRef = ref
					}
				}
				return &p, created, ""
			}
		}
	}
	req.MessageThreadID, req.PlaceRef = thread, ref
	return nil, created, ""
}

// placeMentions adds the area's channels to a response as Discord mentions,
// ready to paste into a reply.
func placeMentions(resp map[string]any, p *store.Project) {
	if p.MessageThreadID > 0 {
		resp["channel_mention"] = fmt.Sprintf("<#%d>", p.MessageThreadID)
	}
	if f := strings.TrimPrefix(p.PlaceRef, "discord:"); f != p.PlaceRef {
		resp["forum_mention"] = "<#" + f + ">"
	}
}

// projectJoin registers this agent's own project for an existing post (the
// post is the shared record): title from the post, area from its forum,
// stage from its tag. No research schedule: the post already has an owner
// doing research, and a second schedule would double the cost.
func (s *Server) projectJoin(w http.ResponseWriter, req ProjectRequest) {
	if req.ChatID == 0 || req.MessageThreadID == 0 {
		writeError(w, http.StatusBadRequest, "chat_id and message_thread_id (the post) are required")
		return
	}
	projects, err := s.store.ListProjects(req.ChatID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, p := range projects {
		if p.MessageThreadID == req.MessageThreadID && p.Status != "archived" {
			resp := projectJSON(p)
			resp["joined"] = false
			resp["result"] = "You already track this thread as " + p.Slug + "."
			writeJSON(w, resp)
			return
		}
	}
	if s.places == nil {
		writeError(w, http.StatusServiceUnavailable, "places are not available (no platform bot)")
		return
	}
	title, ref, tags, err := s.places.ThreadInfo(req.ChatID, req.MessageThreadID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read the thread: "+err.Error())
		return
	}
	var area *store.Project
	for i := range projects {
		p := &projects[i]
		if p.Kind == store.ProjectKindArea && p.PlaceRef == ref && ref != "" && p.Status != "archived" {
			area = p
			break
		}
	}
	if area == nil {
		writeError(w, http.StatusBadRequest, "this thread is not a post in one of your areas' forums; register the area first "+
			"(create --kind area --place new with the area channel's name reuses its channels)")
		return
	}
	emoji, bare := splitLeadingEmoji(title)
	stage := req.Stage
	if stage == "" && len(tags) > 0 {
		stage = tags[0]
	}
	p, err := s.store.CreateProject(store.Project{
		Slug: req.Slug, Title: bare, Emoji: emoji, ChatID: req.ChatID, MessageThreadID: req.MessageThreadID,
		Area: area.Slug, Stage: stage, Lang: area.Lang, Instructions: req.Instructions,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to join: "+err.Error())
		return
	}
	resp := projectJSON(*p)
	if req.DocPath == "" && s.workspaceDir != "" {
		if warn := s.scaffoldProjectDoc(p); warn != "" {
			resp["warning"] = warn
		} else if fresh, err := s.store.GetProjectBySlug(p.Slug); err == nil && fresh != nil {
			resp["doc_path"] = fresh.DocPath
		}
	}
	resp["joined"] = true
	if fresh, err := s.store.GetProjectBySlug(p.Slug); err == nil && fresh != nil {
		p = fresh
	}
	s.linkShared(p)
	slog.Info("rpc: project joined a post", "slug", p.Slug, "thread", p.MessageThreadID, "area", area.Slug)
	s.refreshProjectHome(p.ChatID)
	writeJSON(w, resp)
}

// splitLeadingEmoji splits "🎉 日本行程 2027" into ("🎉", "日本行程 2027").
func splitLeadingEmoji(title string) (emoji, rest string) {
	title = strings.TrimSpace(title)
	i := strings.IndexFunc(title, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
	if i <= 0 {
		return "", title
	}
	return strings.TrimSpace(title[:i]), strings.TrimSpace(title[i:])
}

// titleKey compares project titles without leading emoji, case or spacing.
func titleKey(t string) string {
	_, bare := splitLeadingEmoji(t)
	return strings.ToLower(strings.Join(strings.Fields(bare), " "))
}

// sameTitleInArea returns this agent's active project in the area with the
// same title, or nil.
func (s *Server) sameTitleInArea(area *store.Project, title string) *store.Project {
	kids, err := s.store.AreaProjects(area.Slug)
	if err != nil {
		return nil
	}
	want := titleKey(title)
	for i := range kids {
		if kids[i].Status == "active" && titleKey(kids[i].Title) == want && want != "" {
			return &kids[i]
		}
	}
	return nil
}

// ownProjectOn returns the slug of this agent's other active project bound
// to the thread, or "".
func (s *Server) ownProjectOn(chatID, thread int64, except string) string {
	projects, err := s.store.ListProjects(chatID)
	if err != nil {
		return ""
	}
	for _, p := range projects {
		if p.Status == "active" && p.MessageThreadID == thread && p.Slug != except {
			return p.Slug
		}
	}
	return ""
}

// refreshOpener re-renders the first message of p's post from its doc
// (the live summary); see project.RefreshOpener.
func (s *Server) refreshOpener(p *store.Project, doc string) {
	if s.places == nil {
		return
	}
	project.RefreshOpener(s.store, s.workspaceDir, s.agentName, p, doc, s.places)
}

// linkShared puts a project with its own Discord place (a post, or an
// area's channel) on the one doc both agents share (part 3). Best effort:
// a failed link leaves the agent's own doc in place.
func (s *Server) linkShared(p *store.Project) {
	if s.sharedRoot == "" || s.workspaceDir == "" || s.agentName == "" || p.MessageThreadID < 100_000_000_000_000_000 {
		return
	}
	if p.Kind != store.ProjectKindArea && p.Area == "" {
		return
	}
	if fresh, err := s.store.GetProjectBySlug(p.Slug); err == nil && fresh != nil {
		p.ScheduleDedupKey = fresh.ScheduleDedupKey // stamped when research registered
	}
	owner, err := project.LinkShared(s.sharedRoot, s.workspaceDir, p.Slug, s.agentName, p.MessageThreadID, p.ScheduleDedupKey != "")
	if err != nil {
		slog.Warn("rpc: shared doc link failed", "slug", p.Slug, "thread", p.MessageThreadID, "error", err)
		return
	}
	if p.DocPath == "" {
		path := "projects/" + p.Slug + "/doc.md"
		if err := s.store.UpdateProjectFields(p.Slug, store.ProjectFieldUpdate{DocPath: &path}); err == nil {
			p.DocPath = path
		}
	}
	slog.Info("rpc: shared doc linked", "slug", p.Slug, "thread", p.MessageThreadID, "owner", owner)
	if _, err := project.EnqueueNotesFold(s.store, s.workspaceDir, s.agentName, *p, time.Now()); err != nil {
		slog.Warn("rpc: notes fold not queued", "slug", p.Slug, "error", err)
	}
}
