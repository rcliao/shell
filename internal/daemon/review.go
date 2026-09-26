package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

// The weekly review (docs/DESIGN-ROUTER-AND-SUGGESTIONS.md, S0). Once a week
// each agent gets an evidence pack about its own week, makes the changes that
// are within its own power, and files at most three suggestions for its
// owner with shell_suggestion. The harness then delivers the suggestions to
// the owner's chat. The owner's answers come back in the next pack; that is
// how the agent learns what its human values.

const (
	// ReviewEventKind is the task-queue kind of the weekly review.
	ReviewEventKind = "agent.review"
	// ReviewDedupKey is the schedule dedup key of the weekly review.
	ReviewDedupKey = "agent:review"
	// reviewWindow is the period one review looks back over.
	reviewWindow = 7 * 24 * time.Hour
	// reviewMaxAsks caps suggestions delivered from one review.
	reviewMaxAsks = 3
	// reviewSender marks the turn as a system turn: "scheduler" is one of
	// the bridge's system senders, so no router shadow and no transcript.
	reviewSender = "scheduler"
	// reviewTurnTimeout bounds the turn well inside the queue lease (job
	// timeout + 10m), so a slow turn can never be reclaimed and run twice.
	reviewTurnTimeout = 20 * time.Minute
)

// reviewDeps carries what the review needs; the funcs are stubbed in tests.
type reviewDeps struct {
	store       *store.Store
	agentName   string
	ownerChatID int64
	// ownerEval renders the week's OwnerEval dimensions ("" when unavailable).
	ownerEval func(since, until time.Time) string
	// proposals returns titles of the agent's open ghost proposals.
	proposals func(ctx context.Context) []string
	// runTurn runs the review prompt as a system turn and returns the reply.
	runTurn func(ctx context.Context, prompt string) (string, error)
	// notify delivers text to a chat and reports whether it was sent.
	notify func(chatID int64, text string) error
}

// reviewScheduleMessage is the event-mode schedule envelope.
func reviewScheduleMessage() string {
	b, _ := json.Marshal(map[string]any{"kind": ReviewEventKind, "payload": map[string]any{}})
	return string(b)
}

// registerReviewSchedule keeps exactly one live review schedule matching the
// config. Enabled rows are unique by dedup key, disabled ones are not, so an
// upsert alone would pile up a disabled row per restart and would never
// switch a live row off; the existing row is reconciled first.
func registerReviewSchedule(st *store.Store, cronExpr, timezone string, enabled bool) {
	existing, err := st.FindScheduleByDedupKey(ReviewDedupKey)
	if err != nil {
		slog.Warn("review: schedule lookup failed", "error", err)
		return
	}
	if existing != nil && existing.Enabled {
		if enabled && existing.Schedule == cronExpr && existing.Timezone == timezone {
			return // already right
		}
		if err := st.DisableSchedule(existing.ID); err != nil {
			slog.Warn("review: disabling old schedule failed", "schedule_id", existing.ID, "error", err)
			return
		}
		slog.Info("review: schedule disabled", "schedule_id", existing.ID, "reason", "config changed or review off")
	}
	if !enabled {
		return
	}
	cron, err := scheduler.ParseCron(cronExpr)
	if err != nil {
		slog.Warn("review: cron parse failed", "cron", cronExpr, "error", err)
		return
	}
	loc := time.UTC
	if timezone != "" {
		if l, lerr := time.LoadLocation(timezone); lerr == nil {
			loc = l
		}
	}
	id, created, err := st.UpsertScheduleByKey(&store.Schedule{
		Label:     "weekly review: suggestions to the owner",
		Message:   reviewScheduleMessage(),
		Schedule:  cronExpr,
		Timezone:  timezone,
		Type:      "cron",
		Mode:      scheduler.ModeEvent,
		NextRunAt: cron.Next(time.Now().In(loc)).UTC(),
		Enabled:   true,
		DedupKey:  ReviewDedupKey,
	})
	if err != nil {
		slog.Warn("review: schedule registration failed", "error", err)
		return
	}
	if created {
		slog.Info("review: schedule registered", "schedule_id", id, "cron", cronExpr)
	}
}

func wireReview(sched *scheduler.Scheduler, deps reviewDeps) {
	sched.RegisterHandler(ReviewEventKind, deps.handle)
}

func (d reviewDeps) handle(ctx context.Context, _ scheduler.LeasedTask) (string, error) {
	if d.ownerChatID == 0 {
		return "skipped: no owner chat", nil
	}
	started := time.Now().UTC()
	prompt := reviewPrompt(d.agentName, d.evidence(ctx, started.Add(-reviewWindow), started))
	turnCtx, cancel := context.WithTimeout(ctx, reviewTurnTimeout)
	defer cancel()
	reply, err := d.runTurn(turnCtx, prompt)
	if err != nil {
		// Retry only when the turn never started. A turn that ran may have
		// changed skills and filed suggestions; running it again would
		// repeat both (the queue's rule: never fail a fire that ran).
		if errors.Is(err, process.ErrSessionBusy) {
			return "", fmt.Errorf("review turn: %w", err)
		}
		slog.Warn("review: turn failed", "agent", d.agentName, "error", err)
		// Whatever it filed stays proposed and goes out with the next review.
		return "review failed after the turn started: " + err.Error(), nil
	}
	n, err := d.deliver(reply)
	if err != nil {
		slog.Warn("review: delivery failed", "agent", d.agentName, "error", err)
		return "review ran; delivery failed (suggestions stay proposed): " + err.Error(), nil
	}
	return fmt.Sprintf("review done: %d suggestion(s) delivered", n), nil
}

// evidence assembles the week's pack. Every section says plainly when it
// has nothing, so the agent never mistakes a missing signal for a quiet week.
func (d reviewDeps) evidence(ctx context.Context, since, until time.Time) string {
	var sb strings.Builder
	section := func(title, body string) {
		sb.WriteString("### " + title + "\n")
		if strings.TrimSpace(body) == "" {
			body = "(nothing recorded)"
		}
		sb.WriteString(strings.TrimRight(body, "\n") + "\n\n")
	}

	if d.ownerEval != nil {
		section("How the week went with your humans (OwnerEval, regex counts)", d.ownerEval(since, until))
	}

	if counts, err := d.store.FeedbackCounts("reaction", since); err == nil {
		var parts []string
		for emoji, n := range counts {
			parts = append(parts, fmt.Sprintf("%s ×%d", emoji, n))
		}
		sort.Strings(parts)
		section("Reactions to your replies", strings.Join(parts, ", "))
	}

	if usage, err := d.store.SkillUsage(since); err == nil {
		type row struct {
			name string
			u    store.SkillUse
		}
		var rows []row
		for name, u := range usage {
			rows = append(rows, row{name, u})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].u.Runs > rows[j].u.Runs })
		var lines []string
		for i, r := range rows {
			if i == 10 {
				break
			}
			lines = append(lines, fmt.Sprintf("- %s: %d runs, %d failed, SKILL.md read %d×", r.name, r.u.Runs, r.u.Failures, r.u.Reads))
		}
		section("Your skills in use", strings.Join(lines, "\n"))
	}

	var sugg []string
	if open, err := d.store.ListSuggestions([]string{store.SuggestionProposed, store.SuggestionDelivered, store.SuggestionAccepted}, 20); err == nil {
		for _, s := range open {
			sugg = append(sugg, fmt.Sprintf("- #%d [%s] %s", s.ID, s.Status, s.Title))
		}
	}
	if decided, err := d.store.SuggestionsDecidedSince(since); err == nil {
		for _, s := range decided {
			line := fmt.Sprintf("- #%d [%s by %s] %s", s.ID, s.Status, s.DecidedBy, s.Title)
			if s.Note != "" {
				line += " — “" + s.Note + "”"
			}
			sugg = append(sugg, line)
		}
	}
	section("Your suggestions: open, and decided this week (with the owner's words)", strings.Join(sugg, "\n"))

	if d.proposals != nil {
		p := d.proposals(ctx)
		if len(p) > 12 {
			p = append(p[:12], fmt.Sprintf("(and %d more)", len(p)-12))
		}
		// Named bluntly: the first live review (2026-09-25) skipped filing
		// because "my open proposals already cover it", but these were never
		// in front of anyone.
		section("Your older proposals in loop:proposals — NOT in front of the owner; nobody has read them", "- "+strings.Join(p, "\n- "))
	}

	if refl, err := d.store.ListReflections(3); err == nil {
		var lines []string
		for _, r := range refl {
			t := strings.Join(strings.Fields(r.Text), " ")
			if len([]rune(t)) > 300 {
				t = string([]rune(t)[:300]) + "…"
			}
			lines = append(lines, fmt.Sprintf("- %s: %s", r.CreatedAt.Format("Jan 2"), t))
		}
		section("Your last deep reflections", strings.Join(lines, "\n"))
	}
	return sb.String()
}

func reviewPrompt(agent, evidence string) string {
	return fmt.Sprintf(`[Weekly review — %s]
This is your weekly review, a system turn. Your final reply is sent to your
owner verbatim, as the summary above your suggestions. Evidence about your
past week:

%s
Do three things, in this order.

1. DO — make the changes that are within your own power now: your own skills
   (they are committed and the owner is notified automatically), your memory,
   your schedules. Only changes the evidence above supports.

2. ASK — file at most %d suggestions for changes only a human can make: how
   the harness, your tools or your setup should change, or how you and your
   owner could work better together. Use shell_suggestion(action=create) with
   a short title, the evidence (quote the numbers above), and ONE concrete
   change. Filing is the only way a suggestion exists; writing it in your reply
   does not file it. Do not re-file one the owner declined unless you have new
   evidence, and say what is new.
   Your older proposals above are NOT a backlog anyone is working: the owner
   has never seen them. If one still matters, file it now as a suggestion
   (it counts toward the limit). "Already covered by my proposals" is not a
   reason to file nothing. Filing nothing is fine only when nothing matters.

3. REPLY — your whole reply goes to the owner as written, so it is ONLY
   3 to 6 plain lines: what you changed yourself, and in one line why each
   suggestion matters. No preamble, no headings, no "Do:"/"Ask:" labels, no
   narration of this review.`, agent, evidence, reviewMaxAsks)
}

// deliver sends the owner the summary and every suggestion not yet
// delivered (capped), then marks them delivered.
func (d reviewDeps) deliver(summary string) (int, error) {
	pending, err := d.store.ListSuggestions([]string{store.SuggestionProposed}, 50)
	if err != nil {
		return 0, err
	}
	// Oldest first, capped: a review that over-files still sends only the cap;
	// the rest stay proposed and are listed for the agent next week.
	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
	if len(pending) > reviewMaxAsks {
		pending = pending[:reviewMaxAsks]
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "🗒 Weekly review from %s\n", d.agentName)
	if s := strings.TrimSpace(summary); s != "" {
		sb.WriteString("\n" + s + "\n")
	}
	var ids []int64
	for _, s := range pending {
		fmt.Fprintf(&sb, "\n#%d %s\n  why: %s\n  change: %s\n", s.ID, s.Title, oneLine(s.Evidence), oneLine(s.Change))
		ids = append(ids, s.ID)
	}
	if len(ids) > 0 {
		sb.WriteString("\nReply here, e.g. “accept 12” or “decline 12 because …”.")
	}
	if len(ids) == 0 && strings.TrimSpace(summary) == "" {
		return 0, nil // nothing to say; no bare header
	}
	// Delivered means the owner can see it: mark only after a send that
	// reported success, so a failed send leaves them for the next review.
	if err := d.notify(d.ownerChatID, sb.String()); err != nil {
		return 0, fmt.Errorf("send to owner: %w", err)
	}
	if err := d.store.MarkSuggestionsDelivered(ids); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
