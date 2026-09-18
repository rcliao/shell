package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

// The Notion comment loop (P3 Wave D, docs/PLAN-PROJECT-WORKSPACE.md
// "Feedback via Notion"): ONE global poll schedule ticks every 30 minutes and
// enqueues project.event{notion.poll}; THIS file consumes it. The poll is a
// PRODUCER — it detects new comment discussions and unexplained page edits
// and emits them as normalized queue events on the project's (chat, thread)
// partition. Separate consumer cases then run the bounded revision turn
// (comment) or fold the page back into canonical (edit). A future webhook
// producer emits the same events with zero consumer changes.

// notionPollCron is the global poll tick.
const notionPollCron = "*/30 * * * *"

// notionEventTTL bounds how long a detected comment/edit event stays worth
// starting. Generous on purpose: revision work is still worth doing hours
// late (unlike a reminder), and the idempotency key keeps a late run safe.
const notionEventTTL = 12 * time.Hour

// commentReplyMaxRunes truncates the in-thread reply as a safety net; the
// prompt contract already asks for ≤2 lines.
const commentReplyMaxRunes = 1500

// renderExplainSkew pads the "did our own render cause this page edit"
// comparison: task DoneAt is our clock, last_edited_time is Notion's.
const renderExplainSkew = 2 * time.Minute

// notionIdentity caches the integration's bot user id (GET /users/me) for
// the life of the process.
type notionIdentity struct {
	mu sync.Mutex
	id string
}

// notionBotID returns the integration's own user id, cached after the first
// fetch. Without it the poller cannot tell its replies from human comments,
// so callers must treat an error as "do not enqueue anything this round".
func (d projectResearchDeps) notionBotID(ctx context.Context) (string, error) {
	if d.notionID == nil {
		return d.notion.Me(ctx)
	}
	d.notionID.mu.Lock()
	defer d.notionID.mu.Unlock()
	if d.notionID.id != "" {
		return d.notionID.id, nil
	}
	id, err := d.notion.Me(ctx)
	if err != nil {
		return "", err
	}
	d.notionID.id = id
	return id, nil
}

// registerNotionPollSchedule idempotently registers the ONE global Notion
// poll schedule at daemon startup (explicit dedup key, same mechanism as a
// project's research schedule). Best-effort: a failure logs and the comment
// loop simply stays dormant until the next start.
func registerNotionPollSchedule(st *store.Store, timezone string) {
	msg, err := project.NotionPollScheduleMessage()
	if err != nil {
		slog.Warn("notion poll: schedule envelope failed", "error", err)
		return
	}
	cron, err := scheduler.ParseCron(notionPollCron)
	if err != nil {
		slog.Warn("notion poll: cron parse failed", "error", err)
		return
	}
	loc := time.UTC
	if timezone != "" {
		if l, lerr := time.LoadLocation(timezone); lerr == nil {
			loc = l
		}
	}
	sched := &store.Schedule{
		Label:     "project notion poll",
		Message:   msg,
		Schedule:  notionPollCron,
		Timezone:  timezone,
		Type:      "cron",
		Mode:      scheduler.ModeEvent,
		NextRunAt: cron.Next(time.Now().In(loc)).UTC(),
		Enabled:   true,
		DedupKey:  project.NotionPollDedupKey,
	}
	id, created, err := st.UpsertScheduleByKey(sched)
	if err != nil {
		slog.Warn("notion poll: schedule registration failed", "error", err)
		return
	}
	if created {
		slog.Info("notion poll: schedule registered", "schedule_id", id, "cron", notionPollCron)
	}
}

// runNotionPoll sweeps every active notion-exported project for new comment
// discussions and unexplained page edits, emitting events for each. Errors on
// one project are logged and do not stop the sweep — the next tick catches
// up, and every enqueue is idempotent.
func (d projectResearchDeps) runNotionPoll(ctx context.Context) (string, error) {
	if d.notion == nil || !d.notion.Enabled() {
		return "skipped: notion not configured", nil
	}
	projects, err := d.store.ListProjects(0)
	if err != nil {
		return "", fmt.Errorf("list projects: %w", err)
	}

	started := time.Now().UTC()
	polled, deferred, comments, edits, failures := 0, 0, 0, 0, 0
	for i := range projects {
		p := &projects[i]
		if p.Status != "active" || p.ExportKind != "notion" || p.ExportRef == "" {
			continue
		}
		bm := project.ParseBlockMap(p.BlockMap)
		if !bm.Rendered() {
			continue
		}
		if !notionPollDue(p, started) {
			deferred++
			continue
		}
		polled++
		c, e, perr := d.pollProject(ctx, p, bm)
		comments += c
		edits += e
		if perr != nil {
			failures++
			slog.Warn("notion poll: project poll failed", "slug", p.Slug, "error", perr)
			continue
		}
		// Only a COMPLETED sweep counts: a failed one must be retried on the
		// next tick, not pushed out by the quiet interval.
		if merr := d.store.MarkProjectPolled(p.Slug, started); merr != nil {
			slog.Warn("notion poll: mark polled failed", "slug", p.Slug, "error", merr)
		}
	}

	result := fmt.Sprintf("polled %d projects: %d comment events, %d edit events, %d failures",
		polled, comments, edits, failures)
	errMsg := ""
	if failures > 0 {
		errMsg = fmt.Sprintf("%d project polls failed (see log)", failures)
	}
	d.recordScheduleRun(project.NotionPollDedupKey, started, store.OutcomeFiredOK, errMsg)
	slog.Info("notion poll: sweep completed", "projects", polled, "deferred", deferred, "comment_events", comments,
		"edit_events", edits, "failures", failures, "duration", time.Since(started).Round(time.Second))
	return result, nil
}

// Poll backoff (P3.5). A sweep costs one ListComments call per mapped block,
// so a quiet project is the expensive kind to keep checking: a month of
// production sweeps found nothing while the per-tick cost grew 5x with the
// docs. A project someone touched recently keeps the full tick rate; a quiet
// one drops to notionQuietPollInterval — a comment there waits hours, not
// minutes, which is the right trade for a page nobody is reading.
const (
	notionActiveWindow      = 7 * 24 * time.Hour
	notionQuietPollInterval = 6 * time.Hour
)

// notionPollDue reports whether p should be swept on this tick. "Recent" is
// human activity, a research pass, or creation — NOT updated_at, which the
// poller's own bookkeeping would keep fresh forever.
func notionPollDue(p *store.Project, now time.Time) bool {
	if p.NotionPolledAt == nil {
		return true
	}
	recent := p.CreatedAt
	for _, t := range []*time.Time{p.LastHumanActivityAt, p.LastResearchAt} {
		if t != nil && t.After(recent) {
			recent = *t
		}
	}
	if now.Sub(recent) <= notionActiveWindow {
		return true
	}
	return now.Sub(*p.NotionPolledAt) >= notionQuietPollInterval
}

// pollProject polls one project's page, returning how many comment and edit
// events it enqueued.
func (d projectResearchDeps) pollProject(ctx context.Context, p *store.Project, bm project.BlockMap) (commentEvents, editEvents int, err error) {
	lastEdited, err := d.notion.GetPageLastEdited(ctx, p.ExportRef)
	if err != nil {
		return 0, 0, fmt.Errorf("page last-edited: %w", err)
	}
	var prev time.Time
	if p.NotionWatermark != "" {
		if t, perr := time.Parse(time.RFC3339, p.NotionWatermark); perr == nil {
			prev = t
		}
	}

	// Comment sweep: the page PLUS every mapped block, because Notion's
	// comment listing is NOT recursive — a page id returns page-level
	// comments only, and the family user usually comments on a specific
	// block. Deliberately NOT gated on the watermark: creating a comment
	// does not reliably move the page's last_edited_time, and a false skip
	// here would silently kill the whole feedback loop. The client's global
	// pacing bounds the cost.
	targets := []string{p.ExportRef}
	seen := map[string]bool{p.ExportRef: true}
	for _, sec := range bm.Sections {
		for _, id := range sec.Blocks {
			if !seen[id] {
				seen[id] = true
				targets = append(targets, id)
			}
		}
	}
	if bm.Footer != "" && !seen[bm.Footer] {
		targets = append(targets, bm.Footer)
	}

	var all []project.NotionComment
	for _, id := range targets {
		cursor := ""
		for {
			page, next, cerr := d.notion.ListComments(ctx, id, cursor)
			if cerr != nil {
				return commentEvents, editEvents, fmt.Errorf("list comments on %s: %w", id, cerr)
			}
			all = append(all, page...)
			if next == "" {
				break
			}
			cursor = next
		}
	}

	// One event per unhandled discussion whose LATEST comment is human — a
	// thread we answered last is waiting on the human, not on us.
	latestByDisc := map[string]project.NotionComment{}
	for _, c := range all {
		if c.DiscussionID == "" {
			continue
		}
		cur, ok := latestByDisc[c.DiscussionID]
		if !ok || c.CreatedTime.After(cur.CreatedTime) {
			latestByDisc[c.DiscussionID] = c
		}
	}
	handled := store.HandledDiscussionSet(p.HandledDiscussions)
	var fresh []project.NotionComment
	for _, latest := range latestByDisc {
		if !handled[latest.DiscussionID] {
			fresh = append(fresh, latest)
		}
	}

	var humanAt time.Time
	if len(fresh) > 0 {
		botID, berr := d.notionBotID(ctx)
		if berr != nil {
			// Cannot tell our replies from human comments — enqueue nothing
			// this round rather than risk answering ourselves in a loop.
			return commentEvents, editEvents, fmt.Errorf("users/me: %w", berr)
		}
		for _, latest := range fresh {
			if latest.CreatedByID == botID {
				continue
			}
			anchor := latest.ParentID
			if anchor == p.ExportRef {
				anchor = "" // page-level comment
			}
			created, qerr := d.enqueueProjectEvent(project.EventPayload{
				Event: project.EventNotionCommentCreated, Slug: p.Slug,
				ChatID: p.ChatID, ThreadID: p.MessageThreadID,
				DiscussionID: latest.DiscussionID, CommentPlain: latest.Plain, AnchorBlock: anchor,
			}, store.DeriveIdempotencyKey(project.EventKind, p.Slug, latest.DiscussionID, latest.ID))
			if qerr != nil {
				return commentEvents, editEvents, fmt.Errorf("enqueue comment event: %w", qerr)
			}
			if created {
				commentEvents++
				if latest.CreatedTime.After(humanAt) {
					humanAt = latest.CreatedTime
				}
			}
		}
	}

	// Page-edit heuristic (kept deliberately simple, plan Wave D): the
	// watermark moved and no render of the CURRENT doc rev completed since
	// the previous watermark → likely a human edit. False positives are
	// cheap — the reconciler no-ops on no-diff.
	if bm.Adopted {
		// Watch-only: an edit is human activity and a reason to re-read the
		// page's block list (people add and remove blocks freely) — never a
		// reconcile. The daemon never writes to an adopted page, so a
		// watermark move is a human's, or the agent's own edit answering a
		// human's comment — either way the page is in use.
		if !prev.IsZero() && !lastEdited.Equal(prev) {
			humanAt = lastEdited
			editEvents++
		}
		if prev.IsZero() || !lastEdited.Equal(prev) {
			d.refreshAdoptedMap(ctx, p)
		}
	} else if !prev.IsZero() && !lastEdited.Equal(prev) && !d.renderExplains(p, prev) {
		created, qerr := d.enqueueProjectEvent(project.EventPayload{
			Event: project.EventNotionPageEdited, Slug: p.Slug,
			ChatID: p.ChatID, ThreadID: p.MessageThreadID,
		}, store.DeriveIdempotencyKey(project.EventKind, p.Slug, "page-edited",
			lastEdited.UTC().Format(time.RFC3339Nano)))
		if qerr != nil {
			return commentEvents, editEvents, fmt.Errorf("enqueue edit event: %w", qerr)
		}
		if created {
			editEvents++
			if now := time.Now().UTC(); now.After(humanAt) {
				humanAt = now
			}
		}
	}

	update := store.ProjectFieldUpdate{}
	if wm := lastEdited.UTC().Format(time.RFC3339Nano); wm != p.NotionWatermark {
		update.NotionWatermark = &wm
	}
	if !humanAt.IsZero() && (p.LastHumanActivityAt == nil || humanAt.After(*p.LastHumanActivityAt)) {
		at := humanAt
		update.LastHumanActivityAt = &at
	}
	if update.NotionWatermark != nil || update.LastHumanActivityAt != nil {
		if uerr := d.store.UpdateProjectFields(p.Slug, update); uerr != nil {
			slog.Warn("notion poll: project update failed", "slug", p.Slug, "error", uerr)
		}
	}
	return commentEvents, editEvents, nil
}

// refreshAdoptedMap re-reads an adopted page's top-level blocks into the
// block map, so comments on blocks added since adoption are found. Best
// effort: a failure keeps the previous map and the next edit retries.
func (d projectResearchDeps) refreshAdoptedMap(ctx context.Context, p *store.Project) {
	encoded, n, err := project.AdoptPage(ctx, d.notion, p.ExportRef)
	if err != nil {
		slog.Warn("notion poll: adopted map refresh failed", "slug", p.Slug, "error", err)
		return
	}
	if encoded == p.BlockMap {
		return
	}
	if uerr := d.store.UpdateProjectFields(p.Slug, store.ProjectFieldUpdate{BlockMap: &encoded}); uerr != nil {
		slog.Warn("notion poll: adopted map update failed", "slug", p.Slug, "error", uerr)
		return
	}
	slog.Info("notion poll: adopted map refreshed", "slug", p.Slug, "blocks", n)
}

// enqueueProjectEvent enqueues one project.event on the project's own
// (chat, thread) partition so revision turns serialize against live chat.
func (d projectResearchDeps) enqueueProjectEvent(p project.EventPayload, idemKey string) (created bool, err error) {
	payload, err := json.Marshal(p)
	if err != nil {
		return false, err
	}
	exp := time.Now().UTC().Add(notionEventTTL)
	_, created, err = d.store.EnqueueTask(store.Task{
		Kind:           project.EventKind,
		Source:         store.TaskSourceAgent,
		IdempotencyKey: idemKey,
		PartitionKey:   scheduler.PartitionKey(p.ChatID, p.ThreadID),
		Payload:        string(payload),
		ExpiresAt:      &exp,
	})
	return created, err
}

// renderExplains reports whether a render of the project's current doc rev
// completed after the previous watermark — i.e. the page movement is our own
// mirror write, not a human edit.
func (d projectResearchDeps) renderExplains(p *store.Project, prev time.Time) bool {
	if p.DocRev == "" {
		return false
	}
	t, err := d.store.GetTaskByIdempotencyKey(store.DeriveIdempotencyKey(project.RenderKind, p.Slug, p.DocRev))
	if err != nil || t == nil || t.State != store.TaskDone || t.DoneAt == nil {
		return false
	}
	return t.DoneAt.After(prev.Add(-renderExplainSkew))
}

// runCommentRevision runs ONE bounded revision pass for one comment thread
// and replies inside that thread. No Telegram delivery here by design — the
// conversation happens in Notion (notify_policy=quiet default).
func (d projectResearchDeps) runCommentRevision(ctx context.Context, p project.EventPayload) (string, error) {
	if p.DiscussionID == "" {
		// The same bytes will not grow a discussion id on retry.
		return "", fmt.Errorf("notion.comment.created payload needs discussion_id")
	}
	proj, err := d.store.GetProjectBySlug(p.Slug)
	if err != nil {
		return "", fmt.Errorf("load project %q: %w", p.Slug, err)
	}
	if proj == nil {
		slog.Warn("project comment: project missing, skipping", "slug", p.Slug)
		return "skipped: project not found", nil
	}
	if proj.Status != "active" {
		return "skipped: status=" + proj.Status, nil
	}
	// Replay guard: a crash after the reply posted but before task completion
	// re-leases the event; an already-handled discussion must not be answered
	// twice. (The poller filters these too — this covers the race.)
	if store.HandledDiscussionSet(proj.HandledDiscussions)[p.DiscussionID] {
		return "skipped: discussion already handled", nil
	}
	if d.notion == nil || !d.notion.Enabled() {
		return "", fmt.Errorf("notion not configured, cannot reply in thread")
	}

	bm := project.ParseBlockMap(proj.BlockMap)
	var prompt string
	if bm.Adopted {
		prompt = project.AdoptedCommentPrompt(proj.Slug, proj.Title, proj.Instructions, proj.Lang,
			proj.ExportRef, p.CommentPlain, p.AnchorBlock)
	} else {
		prompt = project.CommentRevisionPrompt(proj.Slug, proj.Title, proj.Instructions, proj.Lang,
			d.readManagedDoc(proj.Slug), p.CommentPlain, bm.SectionForBlock(p.AnchorBlock))
	}

	started := time.Now().UTC()
	text, err := d.runProjectTurn(ctx, proj, prompt)
	if err != nil {
		return "", fmt.Errorf("revision turn for %q: %w", p.Slug, err)
	}

	reply := truncateRunes(strings.TrimSpace(text), commentReplyMaxRunes)
	replied := false
	if reply != "" {
		if _, cerr := d.notion.CreateComment(ctx, p.DiscussionID, []project.NotionRichText{{Text: reply}}); cerr != nil {
			// The turn already ran; the retry re-runs it (one-fix-pass on an
			// already-fixed doc no-ops) and tries the reply again.
			return "", fmt.Errorf("reply in discussion for %q: %w", p.Slug, cerr)
		}
		replied = true
	} else {
		slog.Warn("project comment: turn produced no visible reply", "slug", proj.Slug)
	}

	if aerr := d.store.AppendHandledDiscussion(proj.Slug, p.DiscussionID); aerr != nil {
		slog.Warn("project comment: handled-discussions append failed", "slug", proj.Slug, "error", aerr)
	}
	if !bm.Adopted {
		// Safety net beside the turn's own doc-write enqueue (idempotent on rev).
		d.enqueueRenderForCurrentRev(proj.Slug)
	}
	if d.refreshHome != nil {
		d.refreshHome(proj.ChatID)
	}
	slog.Info("project comment: revision pass completed", "slug", proj.Slug,
		"replied", replied, "duration", time.Since(started).Round(time.Second))
	return fmt.Sprintf("comment revision done (replied=%t)", replied), nil
}

// runPageEditReconcile folds a direct human edit of the mirrored page back
// into the canonical doc as an attributed human-edit commit, then re-renders
// (normalizing the page). Quiet by design: one log line, no chat message.
func (d projectResearchDeps) runPageEditReconcile(ctx context.Context, p project.EventPayload) (string, error) {
	proj, err := d.store.GetProjectBySlug(p.Slug)
	if err != nil {
		return "", fmt.Errorf("load project %q: %w", p.Slug, err)
	}
	if proj == nil {
		slog.Warn("project page-edit: project missing, skipping", "slug", p.Slug)
		return "skipped: project not found", nil
	}
	bm := project.ParseBlockMap(proj.BlockMap)
	if proj.ExportKind != "notion" || proj.ExportRef == "" || !bm.Rendered() {
		return "skipped: no rendered notion export", nil
	}
	if bm.Adopted {
		return "skipped: adopted page is watch-only", nil
	}
	dir, ok := project.ManagedDocDir(d.workspaceDir, proj.Slug)
	if !ok {
		return "skipped: no managed doc", nil
	}
	if d.notion == nil || !d.notion.Enabled() {
		return "skipped: notion not configured", nil
	}

	blocks, err := d.notion.GetBlockChildren(ctx, proj.ExportRef)
	if err != nil {
		return "", fmt.Errorf("read page for %q: %w", p.Slug, err)
	}
	canonical, err := project.ReadDoc(dir)
	if err != nil {
		return "", fmt.Errorf("read doc for %q: %w", p.Slug, err)
	}

	merged, changed, notes := project.ReconcileDoc(canonical, blocks, bm.Footer)
	for _, n := range notes {
		slog.Info("project page-edit: "+n, "slug", proj.Slug)
	}
	if !changed {
		return "no diff: page matches canonical", nil
	}

	rev, err := project.WriteDoc(dir, merged, "human-edit(notion)")
	if err != nil {
		return "", fmt.Errorf("write reconciled doc for %q: %w", p.Slug, err)
	}
	if uerr := d.store.UpdateProjectFields(proj.Slug, store.ProjectFieldUpdate{DocRev: &rev}); uerr != nil {
		slog.Warn("project page-edit: doc_rev update failed", "slug", proj.Slug, "error", uerr)
	}
	// Re-render normalizes the page back to canonical form.
	if _, rerr := project.EnqueueRender(d.store, proj.Slug, rev); rerr != nil {
		slog.Warn("project page-edit: render enqueue failed", "slug", proj.Slug, "error", rerr)
	}
	if d.refreshHome != nil {
		d.refreshHome(proj.ChatID)
	}
	slog.Info("project page-edit: reconciled into canonical", "slug", proj.Slug, "rev", rev)
	return "reconciled: rev " + rev, nil
}

// truncateRunes bounds s to n runes.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
