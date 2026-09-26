package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/event"
	"github.com/rcliao/shell/internal/store"
)

// The heartbeat agenda (docs/DESIGN-HEARTBEAT-AGENDA-EVENTS.md). A heartbeat
// used to ask the agent to "review recent activity" with nothing specific;
// ~80% ended in [noop]. The agenda hands each beat the concrete things that
// need the agent's judgment now. The agent decides what to do with each item;
// when there is nothing, the daemon skips the turn.

// AgendaItem is one thing for the agent to consider.
type AgendaItem struct {
	Kind string // conversation | schedule | tool | project | task | event
	Text string
}

// Agenda is what a heartbeat is for. It is built read-only; CommitAgenda
// advances the conversation watermark and marks events seen, and the daemon
// calls it only once the beat's turn succeeded (or the beat was skipped), so
// a failed turn loses nothing.
type Agenda struct {
	Items     []AgendaItem
	watermark int64   // highest message id counted in this agenda
	eventIDs  []int64 // events shown in this agenda
}

// Empty: nothing needs the agent — the beat can be skipped.
func (a Agenda) Empty() bool { return len(a.Items) == 0 }

// Render is the block appended to the heartbeat message.
func (a Agenda) Render() string {
	if a.Empty() {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[Heartbeat agenda — what needs your judgment now. Handle what matters; leave the rest. If nothing needs action, reply [noop].]\n")
	for _, it := range a.Items {
		fmt.Fprintf(&sb, "- (%s) %s\n", it.Kind, it.Text)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// agendaWatermarkKey persists the conversation watermark (kv), so a daemon
// restart does not shrink "since the last beat".
const agendaWatermarkKey = "heartbeat_agenda:last_msg_id"

const (
	// agendaFirstWindow is the look-back when there is no watermark yet: as
	// long as the idle heartbeat interval, so nothing between beats is missed.
	agendaFirstWindow   = 2 * time.Hour
	agendaScanWindow    = 48 * time.Hour
	agendaEventMaxAge   = 3 * 24 * time.Hour
	agendaRepairWindow  = 3 * 24 * time.Hour
	agendaToolWindow    = 24 * time.Hour
	agendaToolFailures  = 3
	agendaProjectStale  = 3 * 24 * time.Hour
	agendaEventsPerBeat = 10
)

// SetEventsSpool turns on ingestion of the agent's event spool (config
// events.spool). "" = off: events can still be injected and are listed.
func (b *Bridge) SetEventsSpool(dir string) { b.eventsSpool = dir }

// HeartbeatAgenda gathers the signals for one beat.
func (b *Bridge) HeartbeatAgenda(ctx context.Context) Agenda {
	var a Agenda
	if b.store == nil {
		return a
	}
	now := time.Now()
	var lastID int64
	haveMark := false
	if v, ok, err := b.store.GetKV(agendaWatermarkKey); err == nil && ok {
		if n, perr := strconv.ParseInt(v, 10, 64); perr == nil {
			lastID, haveMark = n, true
		}
	}

	// 1. New conversation since the last beat, per chat. The cut-off is the
	// last counted message id (timestamps have one-second resolution).
	// Without a watermark yet, the look-back is the idle interval.
	scanFrom := now.Add(-agendaScanWindow)
	if !haveMark {
		scanFrom = now.Add(-agendaFirstWindow)
	}
	if msgs, err := b.store.UserMessagesSince(scanFrom); err == nil && len(msgs) > 0 {
		per := map[int64]int{}
		a.watermark = lastID
		for _, m := range msgs {
			if m.ID <= lastID {
				continue
			}
			per[m.ChatID]++
			if m.ID > a.watermark {
				a.watermark = m.ID
			}
		}
		var chats []int64
		for c := range per {
			chats = append(chats, c)
		}
		sort.Slice(chats, func(i, j int) bool { return chats[i] < chats[j] })
		for _, c := range chats {
			a.Items = append(a.Items, AgendaItem{"conversation", fmt.Sprintf(
				"chat %d: %d new message(s) since the last beat — anything you promised, or should follow up on?", c, per[c])})
		}
	}

	// 2. Its own schedules the scheduler had to pause.
	if paused, err := b.store.AutoPausedSchedulesSince(now.Add(-agendaRepairWindow)); err == nil {
		for _, p := range paused {
			a.Items = append(a.Items, AgendaItem{"schedule", fmt.Sprintf(
				"schedule #%d was auto-paused (%s): %s — fix or cancel it with shell_schedule", p.ID, p.Reason, clipRunes(p.Label, 100))})
		}
	}

	// 3. Tools failing over and over.
	if failing, err := b.store.FailingTools(now.Add(-agendaToolWindow), agendaToolFailures); err == nil {
		var names []string
		for n := range failing {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			a.Items = append(a.Items, AgendaItem{"tool", fmt.Sprintf("%s failed %d times in 24 h — is something broken you can fix or work around?", n, failing[n])})
		}
	}

	// 4. A project decision waiting on people.
	if projects, err := b.store.ListProjects(0); err == nil {
		for _, p := range projects {
			if p.Status != "active" {
				continue
			}
			if p.LastHumanActivityAt != nil && now.Sub(*p.LastHumanActivityAt) < agendaProjectStale {
				continue
			}
			doc := b.readProjectDoc(p.DocPath)
			pending := strings.TrimSpace(docSection(doc, docToDecideHeading))
			if pending == "" {
				continue
			}
			first := strings.TrimSpace(strings.SplitN(pending, "\n", 2)[0])
			a.Items = append(a.Items, AgendaItem{"project", fmt.Sprintf(
				"%s has an open decision and no human activity for 3+ days: %s — worth a gentle nudge in chat %d?", p.Slug, clipRunes(first, 120), p.ChatID)})
		}
	}

	// 5. Tasks waiting on this agent.
	if b.taskStore != nil {
		if pending, err := b.taskStore.PendingTasksFor(b.agentBotUsername); err == nil && len(pending) > 0 {
			a.Items = append(a.Items, AgendaItem{"task", fmt.Sprintf("%d delegated task(s) pending for you", len(pending))})
		}
	}

	// 6. External events.
	if b.eventsSpool != "" {
		if n, err := event.IngestSpool(b.eventsSpool, b.store); err != nil {
			slog.Warn("events: spool ingest failed", "dir", b.eventsSpool, "error", err)
		} else if n > 0 {
			slog.Info("events: ingested", "count", n)
		}
	}
	// Open events (new, or shown before and not yet marked done/ignored) stay
	// on the agenda for up to 3 days, so a beat that failed or ended in a
	// noop without a decision does not lose them.
	if evs, err := b.store.ListEvents([]string{store.EventNew, store.EventSeen}, agendaEventsPerBeat); err == nil {
		for i := len(evs) - 1; i >= 0; i-- { // oldest first
			e := evs[i]
			if now.Sub(e.CreatedAt) > agendaEventMaxAge {
				continue
			}
			where, again := "", ""
			if e.ChatID != 0 {
				where = fmt.Sprintf(" (for chat %d)", e.ChatID)
			}
			if e.Status == store.EventSeen {
				again = " [shown before, still open]"
			}
			a.Items = append(a.Items, AgendaItem{"event", fmt.Sprintf("#%d %s/%s at %s%s%s: %s — act on it, or mark it with shell_event(done|ignore)",
				e.ID, e.Source, e.Kind, e.OccurredAt.Local().Format("Mon 15:04"), where, again, e.Summary)})
			a.eventIDs = append(a.eventIDs, e.ID)
		}
	}
	return a
}

// CommitAgenda records that a beat handled its agenda: the conversation
// watermark advances and its events are marked seen. Call it only after the
// beat's turn succeeded, or for a skipped beat.
func (b *Bridge) CommitAgenda(a Agenda) {
	if b.store == nil {
		return
	}
	if a.watermark > 0 {
		cur := int64(0)
		if v, ok, err := b.store.GetKV(agendaWatermarkKey); err == nil && ok {
			cur, _ = strconv.ParseInt(v, 10, 64)
		}
		if a.watermark > cur {
			if err := b.store.SetKV(agendaWatermarkKey, strconv.FormatInt(a.watermark, 10)); err != nil {
				slog.Warn("agenda: watermark save failed", "error", err)
			}
		}
	}
	for _, id := range a.eventIDs {
		_ = b.store.MarkEvent(id, store.EventSeen, "")
	}
}

func clipRunes(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// runHeartbeatMaintenance is the housekeeping every heartbeat does whether or
// not the agent took a turn: commit the agent's own skill edits, and memory
// reflect / summarize / hygiene. A beat skipped for an empty agenda still
// runs it (HeartbeatSkipped), just without a model call.
func (b *Bridge) runHeartbeatMaintenance(ctx context.Context, chatID int64) {
	// Self-authored skills: commit + announce any change the agent made to
	// its own skills directory. Off the turn path.
	go b.commitSkillChanges(context.WithoutCancel(ctx))

	// Memory maintenance.
	if b.memory != nil {
		// Run reflect cycle after heartbeat to promote/decay/prune/dedup memories.
		reflectResult := b.memory.RunReflect(ctx)
		// Summarize old exchanges during heartbeat maintenance.
		if n, err := b.memory.SummarizeExchanges(ctx, chatID); err != nil {
			slog.Warn("exchange summarization failed", "error", err)
		} else if n > 0 {
			slog.Info("heartbeat summarized exchanges", "chat_id", chatID, "count", n)
		}
		// Stash consolidation + noise candidates for the NEXT heartbeat enrichment.
		ns := b.memory.AgentNS(chatID)
		if reflectResult != nil {
			candidates := b.memory.ConsolidationCandidates(ctx, reflectResult, 3)
			noise := b.memory.NoisyCandidates(ctx, ns, chatID, 5)
			b.stashConsolidationCandidates(chatID, candidates+noise)
		}
		// Run health check and log hygiene outcome for trend tracking.
		health := b.memory.HealthCheck(ctx, ns, chatID)
		slog.Info("memory health", "noise_ratio", health.NoiseRatio,
			"pinned", health.PinnedPresent, "diagnosis", health.Diagnosis,
			"queries", health.QueriesTested, "avg_results", health.AvgResults)
		if reflectResult != nil {
			b.memory.LogHygieneOutcome(ctx, ns, reflectResult, health)
		}
	}
}

// HeartbeatSkipped runs a skipped beat's housekeeping (no turn was taken).
func (b *Bridge) HeartbeatSkipped(ctx context.Context, chatID int64) {
	b.runHeartbeatMaintenance(ctx, chatID)
}
