package route

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// Running the judge (R0 scoring; weekly in the review since the feedback
// loops). Shared by `shell route judge` and the daemon's weekly review.

// noSubject marks a judge label whose subject the model left out.
const noSubject = "-"

// judgeBatch is the most messages one judge call labels.
const judgeBatch = 30

// Candidates returns, per chat, the lanes routing offers there: the chat's
// active projects.
func Candidates(st *store.Store) (map[int64][]Candidate, error) {
	all, err := st.ListProjects(0)
	if err != nil {
		return nil, err
	}
	out := map[int64][]Candidate{}
	for _, p := range all {
		if p.Status != "active" {
			continue
		}
		out[p.ChatID] = append(out[p.ChatID], Candidate{Lane: p.Slug, Title: p.Title, Desc: p.Instructions})
	}
	return out, nil
}

// JudgeCall runs one judge prompt (ClaudeCLI in production).
type JudgeCall func(ctx context.Context, model, prompt string) (string, error)

// RunJudge labels the agent's real messages since a cutoff, one thread per
// batch, skipping batches already fully labelled (unless force). Chats in
// exclude are left out. A chat without projects is judged too — its lanes
// are just "general" — so recurring subjects there can still be found.
func RunJudge(ctx context.Context, st *store.Store, model string, since time.Time, force bool, exclude map[int64]bool, call JudgeCall) (labels, calls int, err error) {
	cands, err := Candidates(st)
	if err != nil {
		return 0, 0, err
	}
	msgs, err := st.UserMessagesSince(since)
	if err != nil {
		return 0, 0, err
	}
	have := map[string]bool{}
	if !force {
		best, err := st.BestRouteLabels()
		if err != nil {
			return 0, 0, err
		}
		for k, l := range best {
			if l.Source != "judge" || l.Subject != "" { // an old judge label without a subject is redone
				have[k] = true
			}
		}
	}
	type key struct{ chat, thread int64 }
	threads := map[key][]store.UserMessage{}
	var order []key
	for _, m := range msgs {
		if exclude[m.ChatID] {
			continue
		}
		k := key{m.ChatID, m.ThreadID}
		if _, ok := threads[k]; !ok {
			order = append(order, k)
		}
		threads[k] = append(threads[k], m)
	}
	for _, k := range order {
		ms := threads[k]
		for start := 0; start < len(ms); start += judgeBatch {
			batch := ms[start:min(start+judgeBatch, len(ms))]
			todo := false
			items := make([]JudgeItem, len(batch))
			for i, m := range batch {
				items[i] = JudgeItem{N: i + 1, At: m.At, Text: m.Text}
				if !have[store.LabelKey(m.ChatID, m.ThreadID, store.TextHash(m.Text))] {
					todo = true
				}
			}
			if !todo {
				continue
			}
			out, cerr := call(ctx, model, JudgePrompt(cands[k.chat], items))
			calls++
			if cerr != nil {
				slog.Warn("judge: batch failed", "chat_id", k.chat, "thread_id", k.thread, "error", cerr)
				continue
			}
			parsed, perr := ParseJudge(out, cands[k.chat])
			if perr != nil {
				slog.Warn("judge: unparseable batch", "chat_id", k.chat, "thread_id", k.thread, "error", perr)
				continue
			}
			for _, l := range parsed {
				if l.N < 1 || l.N > len(batch) {
					continue
				}
				m := batch[l.N-1]
				subject := strings.ToLower(strings.TrimSpace(l.Subject))
				if subject == "" {
					subject = noSubject // labelled; never re-judged just for a missing subject
				}
				if err := st.UpsertRouteLabel(store.RouteLabel{ChatID: m.ChatID, ThreadID: m.ThreadID,
					TextHash: store.TextHash(m.Text), Lane: l.Lane, Source: "judge", Sure: l.Sure,
					Subject: subject}); err != nil {
					return labels, calls, err
				}
				labels++
			}
		}
	}
	return labels, calls, nil
}

// Miss is one acted-on routing the best label disagrees with.
type Miss struct {
	At           time.Time
	ChatID       int64
	Routed, Want string
	Source       string // label source
	Text         string
}

// Misses returns acted-on lane decisions (source "lane") since a cutoff that
// disagree with the best label, oldest first, with the message text.
func Misses(st *store.Store, since time.Time, exclude map[int64]bool) ([]Miss, error) {
	rows, err := st.RouteDecisions("lane", since)
	if err != nil {
		return nil, err
	}
	labels, err := st.BestRouteLabels()
	if err != nil {
		return nil, err
	}
	msgs, _ := st.UserMessagesSince(since.Add(-time.Hour))
	text := map[string]string{}
	for _, m := range msgs {
		text[store.LabelKey(m.ChatID, m.ThreadID, store.TextHash(m.Text))] = m.Text
	}
	var out []Miss
	for _, r := range rows {
		if exclude[r.ChatID] {
			continue
		}
		k := store.LabelKey(r.ChatID, r.ThreadID, r.TextHash)
		l, ok := labels[k]
		if !ok || l.Lane == r.Lane {
			continue
		}
		out = append(out, Miss{At: r.MsgAt, ChatID: r.ChatID, Routed: r.Lane, Want: l.Lane, Source: l.Source, Text: text[k]})
	}
	return out, nil
}

// Subject is a recurring subject without a project behind it.
type Subject struct {
	ChatID   int64
	Name     string
	Count    int
	Days     int
	Last     time.Time
	Examples []string
}

// RecurringSubjects finds subjects the judge put in the general lane that
// keep coming back: at least minCount messages on at least minDays distinct
// days since the cutoff. These are candidate projects.
func RecurringSubjects(st *store.Store, since time.Time, minCount, minDays int, exclude map[int64]bool) ([]Subject, error) {
	msgs, err := st.UserMessagesSince(since)
	if err != nil {
		return nil, err
	}
	labels, err := st.BestRouteLabels()
	if err != nil {
		return nil, err
	}
	type agg struct {
		s    Subject
		days map[string]bool
	}
	by := map[string]*agg{}
	for _, m := range msgs {
		if exclude[m.ChatID] {
			continue
		}
		l, ok := labels[store.LabelKey(m.ChatID, m.ThreadID, store.TextHash(m.Text))]
		if !ok || l.Lane != General || l.Subject == "" || l.Subject == noSubject {
			continue
		}
		k := fmt.Sprintf("%d|%s", m.ChatID, l.Subject)
		a := by[k]
		if a == nil {
			a = &agg{s: Subject{ChatID: m.ChatID, Name: l.Subject}, days: map[string]bool{}}
			by[k] = a
		}
		a.s.Count++
		a.days[m.At.Local().Format("2006-01-02")] = true
		if m.At.After(a.s.Last) {
			a.s.Last = m.At
		}
		if len(a.s.Examples) < 3 {
			t := []rune(strings.Join(strings.Fields(m.Text), " "))
			if len(t) > 80 {
				t = append(t[:80], '…')
			}
			a.s.Examples = append(a.s.Examples, string(t))
		}
	}
	var out []Subject
	for _, a := range by {
		a.s.Days = len(a.days)
		if a.s.Count >= minCount && a.s.Days >= minDays {
			out = append(out, a.s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out, nil
}
