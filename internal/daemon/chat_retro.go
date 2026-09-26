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

// The weekly chat retro (S1, docs/DESIGN-ROUTER-AND-SUGGESTIONS.md). For each
// configured chat the agent reads its week there, grouped by the lane each
// message was routed to, retros it privately, and may file ONE suggestion to
// the people in that chat. The harness posts it into the chat; their answer is
// recorded by the agent in that chat and comes back in the next retro.

const (
	ChatRetroEventKind = "agent.chat_retro"
	ChatRetroDedupKey  = "agent:chat-retro"
	chatRetroPerLane   = 6
	chatRetroRunes     = 200
)

type chatRetroDeps struct {
	store     *store.Store
	agentName string
	chats     []int64
	runTurn   func(ctx context.Context, prompt string) (string, error)
	notify    func(chatID int64, text string) error
}

func chatRetroScheduleMessage() string {
	b, _ := json.Marshal(map[string]any{"kind": ChatRetroEventKind, "payload": map[string]any{}})
	return string(b)
}

func registerChatRetroSchedule(st *store.Store, cronExpr, timezone string, enabled bool) {
	registerWeeklyEvent(st, ChatRetroDedupKey, "weekly chat retro: one suggestion into the chat", chatRetroScheduleMessage(), cronExpr, timezone, enabled)
}

func wireChatRetro(sched *scheduler.Scheduler, deps chatRetroDeps) {
	sched.RegisterHandler(ChatRetroEventKind, deps.handle)
}

func (d chatRetroDeps) handle(ctx context.Context, _ scheduler.LeasedTask) (string, error) {
	var done []string
	for _, chatID := range d.chats {
		res, err := d.retroOne(ctx, chatID)
		if err != nil {
			return "", err // only a turn that never started (session busy) gets here
		}
		done = append(done, fmt.Sprintf("%d: %s", chatID, res))
	}
	return strings.Join(done, "; "), nil
}

func (d chatRetroDeps) retroOne(ctx context.Context, chatID int64) (string, error) {
	since := time.Now().Add(-reviewWindow)
	evidence, n := d.evidence(chatID, since)
	if n == 0 {
		return "skipped: no messages this week", nil
	}
	turnCtx, cancel := context.WithTimeout(ctx, reviewTurnTimeout)
	defer cancel()
	if _, err := d.runTurn(turnCtx, chatRetroPrompt(d.agentName, chatID, evidence)); err != nil {
		if errors.Is(err, process.ErrSessionBusy) {
			return "", fmt.Errorf("chat retro turn: %w", err)
		}
		slog.Warn("chat retro: turn failed", "chat_id", chatID, "error", err)
		return "turn failed: " + err.Error(), nil // never replay a turn that ran
	}
	return d.deliver(chatID)
}

// evidence: the chat's human messages this week, grouped by lane (from the
// acted-on routing), plus what was already suggested there.
func (d chatRetroDeps) evidence(chatID int64, since time.Time) (string, int) {
	msgs, err := d.store.UserMessagesSince(since)
	if err != nil {
		return "", 0
	}
	laneOf := map[string]string{}
	if rows, err := d.store.RouteDecisions("lane", since.Add(-time.Hour)); err == nil {
		for _, r := range rows {
			laneOf[store.LabelKey(r.ChatID, r.ThreadID, r.TextHash)] = r.Lane
		}
	}
	byLane := map[string][]store.UserMessage{}
	n := 0
	for _, m := range msgs {
		if m.ChatID != chatID {
			continue
		}
		lane := laneOf[store.LabelKey(m.ChatID, m.ThreadID, store.TextHash(m.Text))]
		if lane == "" {
			lane = "general"
		}
		byLane[lane] = append(byLane[lane], m)
		n++
	}
	var lanes []string
	for l := range byLane {
		lanes = append(lanes, l)
	}
	sort.Strings(lanes)
	var sb strings.Builder
	for _, l := range lanes {
		ms := byLane[l]
		fmt.Fprintf(&sb, "### Lane: %s — %d messages\n", l, len(ms))
		if len(ms) > chatRetroPerLane {
			ms = ms[len(ms)-chatRetroPerLane:]
			sb.WriteString("(latest shown)\n")
		}
		for _, m := range ms {
			t := []rune(strings.Join(strings.Fields(m.Text), " "))
			if len(t) > chatRetroRunes {
				t = append(t[:chatRetroRunes], '…')
			}
			fmt.Fprintf(&sb, "- [%s] %s\n", m.At.Local().Format("Mon 15:04"), string(t))
		}
		sb.WriteString("\n")
	}
	aud := store.ChatAudience(chatID)
	var past []string
	if all, err := d.store.ListSuggestions(nil, 50); err == nil {
		for _, s := range all {
			if s.Audience != aud {
				continue
			}
			line := fmt.Sprintf("- #%d [%s] %s", s.ID, s.Status, s.Title)
			if s.Note != "" {
				line += " — “" + oneLine(s.Note) + "”"
			}
			past = append(past, line)
		}
	}
	sb.WriteString("### Suggestions already made in this chat\n")
	if len(past) == 0 {
		sb.WriteString("(none yet)\n")
	} else {
		sb.WriteString(strings.Join(past, "\n") + "\n")
	}
	return sb.String(), n
}

func chatRetroPrompt(agent string, chatID int64, evidence string) string {
	kind := "a family DM"
	if chatID < 0 {
		kind = "the family group"
	}
	return fmt.Sprintf(`[Weekly chat retro — %s — %s (chat %d)]
This is a system turn. Nothing you write here is posted, except a suggestion
you file. Here is your week in that chat, grouped by the lane each message
was routed to:

%s
1. RETRO, for yourself: per lane, what the people there asked for, what you
   did, where it went wrong or needed a nudge, and what stayed open. Change
   what is within your own power now (memory, skills, project instructions).

2. SUGGEST, at most ONE thing to the people in that chat, and only if it would
   genuinely make working together easier for THEM (a habit, a shortcut, a
   project worth starting, something you keep having to ask). File it with
   shell_suggestion(action=create, for_chat=%d, title=…, evidence=…,
   change=…). Write the title and change in the chat's language, as a short,
   warm message to them, ending with how to answer (for example: reply 好 or
   不用). Never mention internals (lanes, routing, sessions, tools). Do not
   repeat a suggestion they declined. Filing nothing is the right answer most
   weeks.

3. Reply with one line: what you filed, or "nothing".`, agent, kind, chatID, evidence, chatID)
}

// deliver posts the chat's newest proposed suggestion into the chat (main
// thread) and withdraws any extra: one per chat per retro.
func (d chatRetroDeps) deliver(chatID int64) (string, error) {
	pending, err := d.store.ProposedFor(store.ChatAudience(chatID))
	if err != nil || len(pending) == 0 {
		return "no suggestion", err
	}
	s := pending[len(pending)-1]
	for _, extra := range pending[:len(pending)-1] {
		_ = d.store.DecideSuggestion(extra.ID, store.SuggestionWithdrawn, "harness", "one suggestion per chat per week")
	}
	text := "💡 " + strings.TrimSpace(s.Title) + "\n" + strings.TrimSpace(s.Change)
	if err := d.notify(chatID, text); err != nil {
		slog.Warn("chat retro: delivery failed", "chat_id", chatID, "error", err)
		return "delivery failed (stays proposed): " + err.Error(), nil
	}
	if err := d.store.MarkSuggestionsDelivered([]int64{s.ID}); err != nil {
		return "", err
	}
	return fmt.Sprintf("suggestion #%d posted", s.ID), nil
}
