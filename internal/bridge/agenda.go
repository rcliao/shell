package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
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

// Agenda is what a heartbeat is for.
type Agenda struct{ Items []AgendaItem }

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

const (
	agendaFirstWindow   = time.Hour
	agendaRepairWindow  = 3 * 24 * time.Hour
	agendaToolWindow    = 24 * time.Hour
	agendaToolFailures  = 3
	agendaProjectStale  = 3 * 24 * time.Hour
	agendaEventsPerBeat = 10
)

// agendaState remembers when the last agenda was built, so "new
// conversation" means since the previous beat.
type agendaState struct {
	mu     sync.Mutex
	last   time.Time
	lastID int64 // highest message id already counted
}

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
	b.agenda.mu.Lock()
	since, lastID := b.agenda.last, b.agenda.lastID
	b.agenda.last = now
	b.agenda.mu.Unlock()
	if since.IsZero() {
		since = now.Add(-agendaFirstWindow)
	}

	// 1. New conversation since the last beat, per chat. The cut-off is
	// the last counted message id (timestamps have one-second resolution);
	// the time bound, a second earlier, only limits the scan.
	if msgs, err := b.store.UserMessagesSince(since.Add(-time.Second)); err == nil && len(msgs) > 0 {
		per := map[int64]int{}
		maxID := lastID
		for _, m := range msgs {
			if m.ID <= lastID {
				continue
			}
			per[m.ChatID]++
			if m.ID > maxID {
				maxID = m.ID
			}
		}
		b.agenda.mu.Lock()
		if maxID > b.agenda.lastID {
			b.agenda.lastID = maxID
		}
		b.agenda.mu.Unlock()
		var chats []int64
		for c := range per {
			chats = append(chats, c)
		}
		sort.Slice(chats, func(i, j int) bool { return chats[i] < chats[j] })
		for _, c := range chats {
			a.Items = append(a.Items, AgendaItem{"conversation", fmt.Sprintf(
				"chat %d: %d new message(s) since %s — anything you promised, or should follow up on?", c, per[c], since.Local().Format("15:04"))})
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
	if evs, err := b.store.ListEvents([]string{store.EventNew}, agendaEventsPerBeat); err == nil {
		for i := len(evs) - 1; i >= 0; i-- { // oldest first
			e := evs[i]
			where := ""
			if e.ChatID != 0 {
				where = fmt.Sprintf(" (for chat %d)", e.ChatID)
			}
			a.Items = append(a.Items, AgendaItem{"event", fmt.Sprintf("#%d %s/%s at %s%s: %s — act on it, or mark it with shell_event(done|ignore)",
				e.ID, e.Source, e.Kind, e.OccurredAt.Local().Format("Mon 15:04"), where, e.Summary)})
			_ = b.store.MarkEvent(e.ID, store.EventSeen, "")
		}
	}
	return a
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
