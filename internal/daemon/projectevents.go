package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/bridge"
	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

// The project.event consumer (P2, docs/PLAN-PROJECT-WORKSPACE.md "Autonomous
// research"): a project's research schedule is an event-mode cron whose fire
// enqueues project.event{research.due}; THIS handler leases it and owns
// everything downstream — the research turn, chat delivery, the bookkeeping.
//
// Delivery deliberately does not reuse the scheduler's prompt path: onPrompt
// runs the turn on thread 0 and sends the reply to thread 0, because
// schedules have no thread column. A group project bound to a forum topic
// needs both the TURN and the DELTA on its own (chat, thread) — so the
// consumer calls the same synthetic-turn machinery directly with the
// project's stored thread and delivers the visible reply itself through the
// Transport. Text only: the consumer owns the no-unprompted-media rule, so
// any photos a turn produced are dropped here, never sent.

// projectResearchTimeout bounds one research turn, matching the scheduler's
// default job timeout (observed turns run to ~8.5 minutes).
const projectResearchTimeout = 20 * time.Minute

// projectResearchRedoWindow is the idempotence guard for replays: the queue
// allows up to MaxAttempts (3) runs of one event, and a crash AFTER the turn
// but before task completion would re-lease it. A research pass that already
// landed within this window is not run again.
const projectResearchRedoWindow = time.Hour

// projectResearchDeps carries what the consumer needs. The funcs exist so
// tests can stub the turn and delivery without a live agent or Telegram.
type projectResearchDeps struct {
	store        *store.Store
	workspaceDir string
	// runTurn executes a project prompt as an agent turn on the project's
	// (chat, thread) session and returns the visible reply text.
	runTurn func(ctx context.Context, chatID, threadID int64, prompt string) (string, error)
	// deliver sends the delta text to the project's chat + thread (Transport),
	// with optional inline URL buttons (the project's doc link).
	deliver func(chatID, threadID int64, text string, buttons []bridge.LinkButton)
	// refreshHome updates the chat's pinned 📋 Projects message.
	refreshHome func(chatID int64)

	// notion is the shared Notion client (Wave D): the poll consumer reads
	// comments and page state through it, the revision consumer replies
	// through it. Sharing ONE client with the renderer keeps every Notion
	// call under the client's global pacing. Nil/disabled = comment loop off.
	notion project.NotionAPI
	// notionID caches the integration's bot user id across polls.
	notionID *notionIdentity
}

// wireProjectEvents registers the project.event handler. Registration is
// unconditional at startup: an unregistered kind fails loudly at lease time,
// so the handler must exist before any project schedule can fire.
func wireProjectEvents(sched *scheduler.Scheduler, deps projectResearchDeps) {
	sched.RegisterHandler(project.EventKind, deps.handleProjectEvent)
}

func (d projectResearchDeps) handleProjectEvent(ctx context.Context, t scheduler.LeasedTask) (string, error) {
	p, err := project.DecodeEventPayload(t.Payload)
	if err != nil {
		// The same bytes will not parse on a retry either; the attempts cap
		// bounds the damage and the last failure records why.
		return "", err
	}
	switch p.Event {
	case project.EventResearchDue:
		return d.runResearch(ctx, p.Slug)
	case project.EventNotionPoll:
		return d.runNotionPoll(ctx)
	case project.EventNotionCommentCreated:
		return d.runCommentRevision(ctx, p)
	case project.EventNotionPageEdited:
		return d.runPageEditReconcile(ctx, p)
	default:
		// A future producer emitting an event this build does not know is not
		// an error worth retrying — record it and move on.
		slog.Warn("project event: unknown event ignored", "event", p.Event, "slug", p.Slug)
		return "ignored: unknown event " + p.Event, nil
	}
}

// runResearch executes one bounded research pass for a project.
func (d projectResearchDeps) runResearch(ctx context.Context, slug string) (string, error) {
	proj, err := d.store.GetProjectBySlug(slug)
	if err != nil {
		return "", fmt.Errorf("load project %q: %w", slug, err)
	}
	if proj == nil {
		slog.Warn("project research: project missing, skipping", "slug", slug)
		return "skipped: project not found", nil
	}
	if proj.Status != "active" {
		slog.Warn("project research: project not active, skipping", "slug", slug, "status", proj.Status)
		return "skipped: status=" + proj.Status, nil
	}
	// Idempotence guard for replays: a crashed run may retry (queue attempts),
	// but a pass that already landed recently must not run twice.
	if proj.LastResearchAt != nil && time.Since(*proj.LastResearchAt) < projectResearchRedoWindow {
		slog.Info("project research: recent pass exists, skipping replay",
			"slug", slug, "last_research_at", proj.LastResearchAt)
		return "skipped: research already ran at " + proj.LastResearchAt.UTC().Format(time.RFC3339), nil
	}

	prompt := project.ResearchPrompt(proj.Slug, proj.Title, proj.Instructions, proj.Lang, d.readManagedDoc(slug))

	started := time.Now().UTC()
	text, err := d.runProjectTurn(ctx, proj, prompt)
	if err != nil {
		d.recordOutcome(proj, started, store.OutcomeTurnFailed, err.Error())
		return "", fmt.Errorf("research turn for %q: %w", slug, err)
	}

	// Stamp BEFORE delivery: the turn is the expensive, already-done part, and
	// a crash between here and task completion must read as done on replay.
	now := time.Now().UTC()
	if err := d.store.UpdateProjectFields(proj.Slug, store.ProjectFieldUpdate{LastResearchAt: &now}); err != nil {
		slog.Warn("project research: last_research_at update failed", "slug", slug, "error", err)
	}

	// Render trigger (P3 Wave C): mirror whatever the pass wrote to Notion.
	d.enqueueRenderForCurrentRev(slug)

	// Delta delivery: text through the Transport, into the project's own
	// (chat, thread). [noop] means the pass found nothing worth saying.
	delta := strings.TrimSpace(text)
	delivered := false
	if delta != "" && !strings.Contains(delta, "[noop]") && d.deliver != nil {
		// One-tap doc link when the project's mirrored page URL is derivable
		// (same export_kind=notion + rendered-block-map rule as the RPC get).
		var buttons []bridge.LinkButton
		if url := project.NotionPageURL(*proj); url != "" {
			buttons = []bridge.LinkButton{{Label: "📄 開啟文件", URL: url}}
		}
		d.deliver(proj.ChatID, proj.MessageThreadID, delta, buttons)
		delivered = true
	}

	if d.refreshHome != nil {
		d.refreshHome(proj.ChatID)
	}
	d.recordOutcome(proj, started, store.OutcomeFiredOK, "")
	slog.Info("project research: pass completed",
		"slug", slug, "delivered", delivered, "duration", time.Since(started).Round(time.Second))
	return fmt.Sprintf("research pass done (delivered=%t)", delivered), nil
}

// readManagedDoc returns the project's doc content when this daemon manages
// it, "" otherwise — doc_path is workspace-relative and an external doc has
// no repo here to read.
func (d projectResearchDeps) readManagedDoc(slug string) string {
	dir, ok := project.ManagedDocDir(d.workspaceDir, slug)
	if !ok {
		return ""
	}
	content, err := project.ReadDoc(dir)
	if err != nil {
		slog.Warn("project event: doc read failed", "slug", slug, "error", err)
		return ""
	}
	return content
}

// runProjectTurn runs one bounded agent turn on the project's (chat, thread),
// under the shared project-turn timeout. Both the research and comment
// consumers go through here.
func (d projectResearchDeps) runProjectTurn(ctx context.Context, proj *store.Project, prompt string) (string, error) {
	turnCtx, cancel := context.WithTimeout(ctx, projectResearchTimeout)
	defer cancel()
	return d.runTurn(turnCtx, proj.ChatID, proj.MessageThreadID, prompt)
}

// enqueueRenderForCurrentRev re-reads the project row and enqueues a render
// for its current doc rev. The turn's own doc-write RPC also enqueues;
// idempotency on (slug, rev) collapses the two, and a turn that wrote nothing
// keeps the old rev and dedupes into the already-done task. Best-effort.
func (d projectResearchDeps) enqueueRenderForCurrentRev(slug string) {
	if fresh, ferr := d.store.GetProjectBySlug(slug); ferr == nil && fresh != nil && fresh.DocRev != "" {
		if _, rerr := project.EnqueueRender(d.store, fresh.Slug, fresh.DocRev); rerr != nil {
			slog.Warn("project event: render enqueue failed", "slug", slug, "error", rerr)
		}
	}
}

// recordOutcome writes the consumer-side job_runs row for the project's
// research schedule, found by its explicit dedup key. The fire ledger already
// recorded "event enqueued"; this row records what running it produced (and,
// on success, stamps last_success_at for the dead-man's-switch). Best-effort.
func (d projectResearchDeps) recordOutcome(proj *store.Project, started time.Time, outcome, errMsg string) {
	key := proj.ScheduleDedupKey
	if key == "" {
		key = project.ScheduleDedupKey(proj.Slug)
	}
	d.recordScheduleRun(key, started, outcome, errMsg)
}

// recordScheduleRun writes a consumer-side job_runs row against the schedule
// carrying dedupKey. Best-effort: no schedule, no row.
func (d projectResearchDeps) recordScheduleRun(dedupKey string, started time.Time, outcome, errMsg string) {
	sc, err := d.store.FindScheduleByDedupKey(dedupKey)
	if err != nil || sc == nil {
		return
	}
	finished := time.Now().UTC()
	if err := d.store.RecordJobRun(store.JobRun{
		ScheduleID:     sc.ID,
		TriggerContext: store.TriggerEvent,
		FiredAt:        started,
		FinishedAt:     &finished,
		DurationMS:     finished.Sub(started).Milliseconds(),
		Outcome:        outcome,
		ErrorMessage:   errMsg,
	}); err != nil {
		slog.Warn("project event: job run record failed", "dedup_key", dedupKey, "error", err)
	}
}
