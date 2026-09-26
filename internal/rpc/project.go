package rpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

// Agent-facing surface for the project registry (P1, docs/PLAN-PROJECT-
// WORKSPACE.md). One action-multiplexed endpoint, mirroring /queue — no
// path-param routes. P1 shipped the registry (create, get, list, status);
// P2 adds the doc layer: create now scaffolds a per-project git repo, and
// doc-read/doc-write serve the canonical doc with commit-hash receipts.

// ProjectRequest is the request body for POST /project.
type ProjectRequest struct {
	Action string `json:"action"` // create | get | list | status | doc-read | doc-write
	Slug   string `json:"slug"`
	// create fields
	Title           string `json:"title"`
	Emoji           string `json:"emoji"`
	ChatID          int64  `json:"chat_id"`
	MessageThreadID int64  `json:"message_thread_id"` // Telegram forum topic ID (0 = main chat)
	ExportKind      string `json:"export_kind"`
	ExportRef       string `json:"export_ref"`
	DocPath         string `json:"doc_path"`
	Instructions    string `json:"instructions"`
	Lang            string `json:"lang"`
	// Cadence is the autonomous-research cadence registered at create:
	// daily | weekly | monthly (default weekly, owner sign-off #1).
	Cadence string `json:"cadence"`
	// status action
	Status string `json:"status"` // active | paused | archived
	// doc-write fields
	Content     string `json:"content"`
	Attribution string `json:"attribution"` // optional; recorded in the commit message
}

// cadenceCrons maps a research cadence to its cron expression. 09:00 in the
// agent's timezone: late enough to never land in quiet hours, early enough
// that the day's delta is on the pinned list before anyone asks.
var cadenceCrons = map[string]string{
	"daily":   "0 9 * * *",
	"weekly":  "0 9 * * 1",
	"monthly": "0 9 1 * *",
}

// researchCron is the cadence's cron with the minute staggered by project id
// into :10/:20/:30/:40/:50. Every project registering at :00 put the research
// turns on top of each other and of the top-of-the-hour prompt schedules;
// turns that share a chat serialize, and the ones that do not still compete
// for the same tools. Never :00, and stable for a given project.
func researchCron(cadence string, projectID int64) string {
	expr, ok := cadenceCrons[cadence]
	if !ok {
		return ""
	}
	minute := 10 + (projectID%5)*10
	return strconv.FormatInt(minute, 10) + strings.TrimPrefix(expr, "0")
}

// telegramReactionEmoji is the set of emoji Telegram accepts as message
// reactions. A project emoji outside this set still works as a list row and
// doc icon, but setReaction silently no-ops on it — so create warns (and
// stores it anyway; the reaction path falls back to 👀). Keys are stored
// without U+FE0F variation selectors; compare via reactionCapable.
var telegramReactionEmoji = map[string]bool{
	"👍": true, "👎": true, "❤": true, "🔥": true, "🥰": true, "👏": true,
	"😁": true, "🤔": true, "🤯": true, "😱": true, "🤬": true, "😢": true,
	"🎉": true, "🤩": true, "🤮": true, "💩": true, "🙏": true, "👌": true,
	"🕊": true, "🤡": true, "🥱": true, "🥴": true, "😍": true, "🐳": true,
	"❤‍🔥": true, "🌚": true, "🌭": true, "💯": true, "🤣": true, "⚡": true,
	"🍌": true, "🏆": true, "💔": true, "🤨": true, "😐": true, "🍓": true,
	"🍾": true, "💋": true, "🖕": true, "😈": true, "😴": true, "😭": true,
	"🤓": true, "👻": true, "👨‍💻": true, "👀": true, "🎃": true, "🙈": true,
	"😇": true, "😨": true, "🤝": true, "✍": true, "🤗": true, "🫡": true,
	"🎅": true, "🎄": true, "☃": true, "💅": true, "🤪": true, "🗿": true,
	"🆒": true, "💘": true, "🙉": true, "🦄": true, "😘": true, "💊": true,
	"🙊": true, "😎": true, "👾": true, "🤷‍♂": true, "🤷": true, "🤷‍♀": true,
	"😡": true,
}

// reactionCapable reports whether the emoji can be used as a Telegram
// reaction. Variation selectors (U+FE0F) are stripped before lookup so both
// text- and emoji-presentation forms of e.g. ❤ match.
func reactionCapable(emoji string) bool {
	return telegramReactionEmoji[strings.ReplaceAll(emoji, "\uFE0F", "")]
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store not available")
		return
	}
	var req ProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	switch req.Action {
	case "create":
		s.projectCreate(w, req)
	case "get":
		s.projectGet(w, req)
	case "list":
		s.projectList(w, req)
	case "status":
		s.projectStatus(w, req)
	case "doc-read":
		s.projectDocRead(w, req)
	case "doc-write":
		s.projectDocWrite(w, req)
	case "set-instructions":
		s.projectSetInstructions(w, req)
	default:
		writeError(w, http.StatusBadRequest, "action must be create, get, list, status, doc-read, doc-write, or set-instructions")
	}
}

// projectSetInstructions replaces a project's standing instructions — also
// the description the message router reads to decide which messages belong
// to the project. An agent that sees its lanes misroute (weekly review) fixes
// the description here itself.
func (s *Server) projectSetInstructions(w http.ResponseWriter, req ProjectRequest) {
	if req.Slug == "" {
		writeError(w, http.StatusBadRequest, "slug is required")
		return
	}
	p, err := s.store.GetProjectBySlug(req.Slug)
	if err != nil || p == nil {
		writeError(w, http.StatusNotFound, "no project "+req.Slug)
		return
	}
	instr := strings.TrimSpace(req.Instructions)
	if err := s.store.UpdateProjectFields(req.Slug, store.ProjectFieldUpdate{Instructions: &instr}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"slug": req.Slug, "instructions": instr,
		"result": "Instructions updated. The router uses them from the next message."})
}

func (s *Server) projectCreate(w http.ResponseWriter, req ProjectRequest) {
	if req.Title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	if req.ChatID == 0 {
		writeError(w, http.StatusBadRequest, "chat_id is required")
		return
	}
	cadence := req.Cadence
	if cadence == "" {
		cadence = "weekly"
	}
	if _, ok := cadenceCrons[cadence]; !ok {
		writeError(w, http.StatusBadRequest, "cadence must be daily, weekly, or monthly")
		return
	}
	// An export_ref without a kind defaults to notion — the only exporter in
	// v1, and the case the skill's --export-ref flag produces.
	kind := req.ExportKind
	if kind == "" && req.ExportRef != "" {
		kind = "notion"
	}
	p, err := s.store.CreateProject(store.Project{
		Slug:            req.Slug,
		Title:           req.Title,
		Emoji:           req.Emoji,
		ChatID:          req.ChatID,
		MessageThreadID: req.MessageThreadID,
		ExportKind:      kind,
		ExportRef:       req.ExportRef,
		DocPath:         req.DocPath,
		Instructions:    req.Instructions,
		Lang:            req.Lang,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to create project: "+err.Error())
		return
	}
	slog.Info("rpc: project created", "slug", p.Slug, "chat_id", p.ChatID, "export_ref", p.ExportRef)
	resp := projectJSON(*p)
	resp["created"] = true

	// Doc layer (P2): a project without an explicit --doc-path gets its own
	// git repo and a scaffolded doc under the workspace. An explicit doc_path
	// means an EXISTING external file — left alone, the registry just points
	// at it. Scaffold failure degrades to a registry-only project (warn, never
	// fail create): the row is already committed and useful without a doc.
	if req.DocPath == "" && s.workspaceDir != "" {
		if warn := s.scaffoldProjectDoc(p); warn != "" {
			resp["warning"] = warn
		} else if fresh, err := s.store.GetProjectBySlug(p.Slug); err == nil && fresh != nil {
			p = fresh
			resp["doc_path"] = p.DocPath
			resp["doc_rev"] = p.DocRev
		}
	}
	// Warn (do not reject) on a non-reaction-capable emoji: it still works as
	// a list row and doc icon, but the P4 reaction path will fall back to 👀.
	if p.Emoji != "" && !reactionCapable(p.Emoji) {
		resp["warning"] = "emoji not reaction-capable; reactions will fall back to 👀"
	}

	// Self-register the autonomous-research schedule: an event-mode cron whose
	// fire enqueues project.event{research.due} for the daemon's consumer.
	// dedup_key = project:<slug>, which is how archive/pause later finds it.
	if ok, warn := s.registerResearchSchedule(p, cadence); ok {
		resp["cadence"] = cadence
	} else if warn != "" {
		// The warning field is single-valued by contract — an earlier warning
		// (scaffold, emoji) wins and this one lives in the log only.
		if _, has := resp["warning"]; !has {
			resp["warning"] = warn
		}
	}

	s.refreshProjectHome(p.ChatID)
	writeJSON(w, resp)
}

// registerResearchSchedule creates (idempotently) the project's research
// schedule and stamps its dedup key onto the project row. Reports whether a
// schedule exists, plus a warning on failure — create never fails on this:
// the registry row is already committed and useful without a schedule.
func (s *Server) registerResearchSchedule(p *store.Project, cadence string) (bool, string) {
	if s.cronParse == nil {
		// Scheduler disabled is daemon configuration, not a per-create
		// problem — log only, no warning noise on every create.
		slog.Info("rpc: research schedule skipped, scheduler not enabled", "slug", p.Slug)
		return false, ""
	}
	expr := researchCron(cadence, p.ID)
	cronExpr, err := s.cronParse(expr)
	if err != nil {
		slog.Warn("rpc: research schedule cron parse failed", "slug", p.Slug, "expr", expr, "error", err)
		return false, "research schedule not registered: " + err.Error()
	}
	msg, err := project.ResearchScheduleMessage(p.Slug, p.ChatID, p.MessageThreadID)
	if err != nil {
		slog.Warn("rpc: research schedule envelope failed", "slug", p.Slug, "error", err)
		return false, "research schedule not registered: " + err.Error()
	}
	loc := s.location("")
	nextRun := cronExpr.Next(time.Now().In(loc)).UTC()
	if nextRun.IsZero() {
		slog.Warn("rpc: research schedule has no next run", "slug", p.Slug, "expr", expr)
		return false, "research schedule not registered: no next run"
	}
	dedup := project.ScheduleDedupKey(p.Slug)
	sched := &store.Schedule{
		ChatID:    p.ChatID,
		Label:     "project research: " + p.Slug,
		Message:   msg,
		Schedule:  expr,
		Timezone:  s.timezone,
		Type:      "cron",
		Mode:      scheduler.ModeEvent,
		NextRunAt: nextRun,
		Enabled:   true,
		DedupKey:  dedup, // explicit key — honored by UpsertScheduleByKey
	}
	id, created, err := s.store.UpsertScheduleByKey(sched)
	if err != nil {
		slog.Warn("rpc: research schedule upsert failed", "slug", p.Slug, "error", err)
		return false, "research schedule not registered: " + err.Error()
	}
	if err := s.store.UpdateProjectFields(p.Slug, store.ProjectFieldUpdate{ScheduleDedupKey: &dedup}); err != nil {
		slog.Warn("rpc: schedule_dedup_key update failed", "slug", p.Slug, "error", err)
	}
	slog.Info("rpc: project research schedule registered",
		"slug", p.Slug, "schedule_id", id, "created", created, "cadence", cadence)
	return true, ""
}

// syncResearchSchedule brings the project's research schedule in line with a
// lifecycle transition: paused/archived disables it, active re-enables it
// with the next run recomputed from NOW (missed fires are never replayed).
// Best-effort — the status change itself has already landed.
func (s *Server) syncResearchSchedule(p *store.Project, status string) {
	key := p.ScheduleDedupKey
	if key == "" {
		key = project.ScheduleDedupKey(p.Slug)
	}
	sc, err := s.store.FindScheduleByDedupKey(key)
	if err != nil {
		slog.Warn("rpc: research schedule lookup failed", "slug", p.Slug, "error", err)
		return
	}
	if sc == nil {
		return // registry-only project (created before P2, or registration failed)
	}
	switch status {
	case "paused", "archived":
		if !sc.Enabled {
			return
		}
		if err := s.store.PauseSchedule(sc.ID, "project_"+status); err != nil {
			slog.Warn("rpc: research schedule disable failed", "slug", p.Slug, "id", sc.ID, "error", err)
			return
		}
		slog.Info("rpc: research schedule disabled", "slug", p.Slug, "id", sc.ID, "status", status)
	case "active":
		if sc.Enabled {
			return
		}
		if s.cronParse == nil {
			slog.Warn("rpc: cannot re-enable research schedule, scheduler not enabled", "slug", p.Slug)
			return
		}
		cronExpr, err := s.cronParse(sc.Schedule)
		if err != nil {
			slog.Warn("rpc: research schedule cron parse failed", "slug", p.Slug, "expr", sc.Schedule, "error", err)
			return
		}
		nextRun := cronExpr.Next(time.Now().In(s.location(sc.Timezone))).UTC()
		if err := s.store.EnableScheduleFrom(sc.ID, nextRun); err != nil {
			slog.Warn("rpc: research schedule re-enable failed", "slug", p.Slug, "id", sc.ID, "error", err)
			return
		}
		slog.Info("rpc: research schedule re-enabled", "slug", p.Slug, "id", sc.ID, "next_run", nextRun)
	}
}

// refreshProjectHome nudges the pinned 📋 Projects message for a chat, when
// the daemon wired a home renderer in.
func (s *Server) refreshProjectHome(chatID int64) {
	if s.projectHomeRefresh != nil {
		s.projectHomeRefresh(chatID)
	}
}

func (s *Server) projectGet(w http.ResponseWriter, req ProjectRequest) {
	if req.Slug == "" {
		writeError(w, http.StatusBadRequest, "slug is required")
		return
	}
	p, err := s.store.GetProjectBySlug(req.Slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "project not found: "+req.Slug)
		return
	}
	resp := projectJSON(*p)
	// The mirrored page's URL — only when WE rendered it (export_kind=notion
	// plus a block map); a bare export_ref may be a database id or a human
	// page whose URL shape is not ours to guess.
	if url := project.NotionPageURL(*p); url != "" {
		resp["notion_url"] = url
	}
	writeJSON(w, resp)
}

func (s *Server) projectList(w http.ResponseWriter, req ProjectRequest) {
	projects, err := s.store.ListProjects(req.ChatID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(projects))
	for _, p := range projects {
		out = append(out, projectJSON(p))
	}
	writeJSON(w, map[string]any{"projects": out, "count": len(out)})
}

func (s *Server) projectStatus(w http.ResponseWriter, req ProjectRequest) {
	if req.Slug == "" || req.Status == "" {
		writeError(w, http.StatusBadRequest, "slug and status are required")
		return
	}
	if err := s.store.UpdateProjectStatus(req.Slug, req.Status); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	slog.Info("rpc: project status changed", "slug", req.Slug, "status", req.Status)
	p, err := s.store.GetProjectBySlug(req.Slug)
	if err != nil || p == nil {
		writeError(w, http.StatusInternalServerError, "read back failed")
		return
	}
	// Lifecycle side effects: the research schedule follows the status, and
	// the pinned home reflects the new list.
	s.syncResearchSchedule(p, p.Status)
	s.refreshProjectHome(p.ChatID)
	writeJSON(w, projectJSON(*p))
}

// scaffoldProjectDoc creates the per-project doc repo and initial doc for a
// freshly created project, storing doc_path (workspace-relative) and doc_rev
// (the scaffold commit — the first receipt). Returns a warning string on
// failure, empty on success.
func (s *Server) scaffoldProjectDoc(p *store.Project) string {
	dir, err := project.EnsureDocRepo(s.workspaceDir, p.Slug)
	if err != nil {
		slog.Warn("rpc: project doc repo create failed", "slug", p.Slug, "error", err)
		return "doc repo create failed: " + err.Error()
	}
	rev, err := project.ScaffoldDoc(dir, p.Title, p.Instructions)
	if err != nil {
		slog.Warn("rpc: project doc scaffold failed", "slug", p.Slug, "error", err)
		return "doc scaffold failed: " + err.Error()
	}
	docPath := path.Join("projects", p.Slug, project.DocFile)
	if err := s.store.UpdateProjectFields(p.Slug, store.ProjectFieldUpdate{
		DocPath: &docPath, DocRev: &rev,
	}); err != nil {
		slog.Warn("rpc: project doc fields update failed", "slug", p.Slug, "error", err)
		return "doc created but registry update failed: " + err.Error()
	}
	slog.Info("rpc: project doc scaffolded", "slug", p.Slug, "doc_path", docPath, "rev", rev)
	return ""
}

// projectDocDir resolves a project's managed doc repo, or errors when the
// project has none (external doc_path, or the doc layer is disabled). The doc
// verbs only serve repos this daemon manages — an external file has no git
// history to mint receipts from.
func (s *Server) projectDocDir(p *store.Project) (string, error) {
	if s.workspaceDir == "" {
		return "", fmt.Errorf("doc layer disabled: no workspace directory")
	}
	dir := filepath.Join(s.workspaceDir, "projects", p.Slug)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return "", fmt.Errorf("project %q has no managed doc repo (external doc_path?)", p.Slug)
	}
	return dir, nil
}

// loadProject fetches a project by slug, writing the HTTP error itself and
// returning nil when the request cannot proceed.
func (s *Server) loadProject(w http.ResponseWriter, slug string) *store.Project {
	if slug == "" {
		writeError(w, http.StatusBadRequest, "slug is required")
		return nil
	}
	p, err := s.store.GetProjectBySlug(slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "project not found: "+slug)
		return nil
	}
	return p
}

func (s *Server) projectDocRead(w http.ResponseWriter, req ProjectRequest) {
	p := s.loadProject(w, req.Slug)
	if p == nil {
		return
	}
	dir, err := s.projectDocDir(p)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	content, err := project.ReadDoc(dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read doc: "+err.Error())
		return
	}
	rev, err := project.Head(dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "resolve rev: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"slug": p.Slug, "doc_path": p.DocPath, "rev": rev, "content": content,
	})
}

func (s *Server) projectDocWrite(w http.ResponseWriter, req ProjectRequest) {
	p := s.loadProject(w, req.Slug)
	if p == nil {
		return
	}
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}
	dir, err := s.projectDocDir(p)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Doc budget (P3.5): this handler is the AGENT's write path — human edits
	// reconcile through the daemon and are never refused. A missing prev
	// (first write) counts as empty, so only a genuinely oversize first draft
	// can trip it.
	prev, rerr := project.ReadDoc(dir)
	if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		// Treating an unreadable doc as empty would refuse the very write
		// the budget promises to accept: one that shrinks an oversize doc.
		writeError(w, http.StatusInternalServerError, "doc read before write: "+rerr.Error())
		return
	}
	if berr := project.CheckBudget(prev, req.Content, 0); berr != nil {
		slog.Info("rpc: project doc write refused, over budget", "slug", p.Slug,
			"bytes", len(req.Content), "prev_bytes", len(prev))
		writeError(w, http.StatusUnprocessableEntity, berr.Error())
		return
	}
	rev, err := project.WriteDoc(dir, req.Content, req.Attribution)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "doc write: "+err.Error())
		return
	}
	if err := s.store.UpdateProjectFields(p.Slug, store.ProjectFieldUpdate{DocRev: &rev}); err != nil {
		slog.Warn("rpc: doc_rev update failed", "slug", p.Slug, "error", err)
	}
	// One write-hygiene ledger row per doc write, linked to the project. The
	// commit hash IS the successful write evidence, so this is a verified row
	// by construction; best-effort like every other ledger write.
	pid := p.ID
	if err := s.store.LogWriteVerification(store.WriteVerification{
		ChatID:         p.ChatID,
		Classification: "verified",
		Claimed:        true,
		WriteOK:        true,
		ToolNames:      "project.doc-write",
		Source:         "rpc",
		ProjectID:      &pid,
	}); err != nil {
		slog.Warn("rpc: doc-write verification log failed", "slug", p.Slug, "error", err)
	}
	// Render trigger (P3 Wave C): mirror the new rev to Notion via the task
	// queue — the render consumer does all Notion I/O off this request path.
	// Idempotent on (slug, rev); best-effort like the ledger writes above.
	if _, err := project.EnqueueRender(s.store, p.Slug, rev); err != nil {
		slog.Warn("rpc: render enqueue failed", "slug", p.Slug, "error", err)
	}
	slog.Info("rpc: project doc written", "slug", p.Slug, "rev", rev, "bytes", len(req.Content))
	s.refreshProjectHome(p.ChatID)
	writeJSON(w, map[string]any{
		"slug": p.Slug, "doc_path": p.DocPath, "rev": rev, "committed": true,
	})
}

// projectJSON renders a project row for the wire. Nullable timestamps are
// included only when set, matching summarizeTask's style.
func projectJSON(p store.Project) map[string]any {
	row := map[string]any{
		"id": p.ID, "slug": p.Slug, "title": p.Title, "emoji": p.Emoji,
		"status": p.Status, "chat_id": p.ChatID, "message_thread_id": p.MessageThreadID,
		"doc_path": p.DocPath, "doc_rev": p.DocRev,
		"export_kind": p.ExportKind, "export_ref": p.ExportRef,
		"instructions": p.Instructions, "notify_policy": p.NotifyPolicy, "lang": p.Lang,
		"ghost_tag":  p.GhostTag,
		"created_at": p.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": p.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if p.ReviewAfter != nil {
		row["review_after"] = p.ReviewAfter.UTC().Format(time.RFC3339)
	}
	if p.LastResearchAt != nil {
		row["last_research_at"] = p.LastResearchAt.UTC().Format(time.RFC3339)
	}
	if p.LastHumanActivityAt != nil {
		row["last_human_activity_at"] = p.LastHumanActivityAt.UTC().Format(time.RFC3339)
	}
	return row
}
