package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/rcliao/shell/internal/config"
	"github.com/rcliao/shell/internal/decide"
	"github.com/rcliao/shell/internal/route"
	"github.com/rcliao/shell/internal/store"
)

// shell route — measure the message router in shadow (R0,
// docs/DESIGN-ROUTER-AND-SUGGESTIONS.md). replay re-routes past messages,
// judge labels them, report scores backends against labels and against the
// "always general" baseline, label records the owner's override.
func newRouteCmd() *cobra.Command {
	var configFlag string
	var days int
	cmd := &cobra.Command{Use: "route", Short: "Measure the message router (shadow): replay, judge, report, label"}
	cmd.PersistentFlags().StringVar(&configFlag, "config", "",
		"agent config path (e.g. ~/.shell/agents/<agent>/config.json); default ~/.shell/config.json")
	cmd.PersistentFlags().IntVar(&days, "days", 14, "how far back to look")

	open := func() (config.Config, *store.Store, error) {
		cfg := loadConfigFrom(configFlag)
		st, err := store.Open(cfg.Store.DBPath)
		return cfg, st, err
	}

	var noJev bool
	replay := &cobra.Command{
		Use:   "replay",
		Short: "Re-route the agent's real messages from the last N days through every backend",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, st, err := open()
			if err != nil {
				return err
			}
			defer st.Close()
			backends := []route.Backend{route.Keyword{}}
			if jev := decide.NewJev(func() string { return cfg.Secret(decide.KeyName) }); jev.Enabled() && !noJev {
				backends = append(backends, route.Jev{D: jev})
			}
			return runReplay(cmd.Context(), st, backends, cfg.Route.Sticky(), days)
		},
	}
	replay.Flags().BoolVar(&noJev, "no-jev", false, "keyword baseline only (sends nothing out)")

	var force bool
	judge := &cobra.Command{
		Use:   "judge",
		Short: "Label the agent's real messages with a stronger model (the scoring reference)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, st, err := open()
			if err != nil {
				return err
			}
			defer st.Close()
			return runJudge(cmd.Context(), st, cfg.Route.Judge(), days, force)
		},
	}
	judge.Flags().BoolVar(&force, "force", false, "re-label messages the judge already labelled")

	var source string
	var sample int
	report := &cobra.Command{
		Use:   "report",
		Short: "Score each backend against labels and the always-general baseline",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, st, err := open()
			if err != nil {
				return err
			}
			defer st.Close()
			return runRouteReport(st, source, days, sample)
		},
	}
	report.Flags().StringVar(&source, "source", "replay", "replay | live")
	report.Flags().IntVar(&sample, "sample", 0, "also print N labelled messages to spot-check (keys usable with `route label`)")

	var note string
	label := &cobra.Command{
		Use:   "label <chat/thread/hash> <lane>",
		Short: "Record the owner's label for a message (wins over every other source)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, st, err := open()
			if err != nil {
				return err
			}
			defer st.Close()
			var chat, thread int64
			var hash string
			parts := strings.SplitN(args[0], "/", 3)
			if len(parts) != 3 {
				return fmt.Errorf("key must be chat/thread/hash, as printed by `route report --sample`")
			}
			if _, err := fmt.Sscan(parts[0], &chat); err != nil {
				return err
			}
			if _, err := fmt.Sscan(parts[1], &thread); err != nil {
				return err
			}
			hash = parts[2]
			if err := st.UpsertRouteLabel(store.RouteLabel{ChatID: chat, ThreadID: thread, TextHash: hash,
				Lane: args[1], Source: "human", Sure: true, Note: note}); err != nil {
				return err
			}
			fmt.Printf("labelled %s as %s\n", args[0], args[1])
			return nil
		},
	}
	label.Flags().StringVar(&note, "note", "", "why")

	cmd.AddCommand(replay, judge, report, label)
	return cmd
}

// laneCandidates returns, per chat, the lanes live routing would offer:
// the chat's active projects. A chat with none is not routed (live only
// asks which_project when the chat has projects).
func laneCandidates(st *store.Store) (map[int64][]route.Candidate, error) {
	all, err := st.ListProjects(0)
	if err != nil {
		return nil, err
	}
	out := map[int64][]route.Candidate{}
	for _, p := range all {
		if p.Status != "active" {
			continue
		}
		out[p.ChatID] = append(out[p.ChatID], route.Candidate{Lane: p.Slug, Title: p.Title, Desc: p.Instructions})
	}
	return out, nil
}

func chatKind(chatID int64) string {
	if chatID < 0 {
		return "group"
	}
	return "dm"
}

func runReplay(ctx context.Context, st *store.Store, backends []route.Backend, sticky float64, days int) error {
	cands, err := laneCandidates(st)
	if err != nil {
		return err
	}
	msgs, err := st.UserMessagesSince(time.Now().AddDate(0, 0, -days))
	if err != nil {
		return err
	}
	if err := st.ClearRouteDecisions("replay"); err != nil {
		return err
	}
	routed, failed := 0, 0
	for _, m := range msgs {
		c := cands[m.ChatID]
		if len(c) == 0 {
			continue
		}
		in := route.Input{ChatKind: chatKind(m.ChatID), ThreadID: m.ThreadID, Text: m.Text, Candidates: c}
		for _, b := range backends {
			choice, err := b.Choose(ctx, in)
			if err != nil {
				failed++
				continue
			}
			prev, _ := st.LastRouteLane("replay", b.Name(), m.ChatID, m.ThreadID)
			lane, isSticky := route.Decide(prev, choice, sticky)
			if err := st.LogRouteDecision(store.RouteDecision{Source: "replay", ChatID: m.ChatID, ThreadID: m.ThreadID,
				MsgAt: m.At, TextHash: store.TextHash(m.Text), Backend: b.Name(), LanePrev: prev, Choice: choice.Lane,
				Confidence: choice.Confidence, Lane: lane, Sticky: isSticky, LatencyMS: choice.Latency.Milliseconds()}); err != nil {
				return err
			}
		}
		routed++
	}
	names := make([]string, len(backends))
	for i, b := range backends {
		names[i] = b.Name()
	}
	fmt.Printf("replayed %d of %d messages (chats with projects only) through %s; %d backend errors\n",
		routed, len(msgs), strings.Join(names, ", "), failed)
	return nil
}

// judgeBatch is the most messages one judge call labels.
const judgeBatch = 30

func runJudge(ctx context.Context, st *store.Store, model string, days int, force bool) error {
	cands, err := laneCandidates(st)
	if err != nil {
		return err
	}
	msgs, err := st.UserMessagesSince(time.Now().AddDate(0, 0, -days))
	if err != nil {
		return err
	}
	have := map[string]bool{}
	if !force {
		best, err := st.BestRouteLabels()
		if err != nil {
			return err
		}
		for k := range best {
			have[k] = true
		}
	}
	// Group by thread, keep order: the judge reads a thread as a conversation.
	type key struct{ chat, thread int64 }
	threads := map[key][]store.UserMessage{}
	var order []key
	for _, m := range msgs {
		if len(cands[m.ChatID]) == 0 {
			continue
		}
		k := key{m.ChatID, m.ThreadID}
		if _, ok := threads[k]; !ok {
			order = append(order, k)
		}
		threads[k] = append(threads[k], m)
	}
	labelled, calls := 0, 0
	for _, k := range order {
		ms := threads[k]
		for start := 0; start < len(ms); start += judgeBatch {
			end := min(start+judgeBatch, len(ms))
			batch := ms[start:end]
			todo := false
			items := make([]route.JudgeItem, len(batch))
			for i, m := range batch {
				items[i] = route.JudgeItem{N: i + 1, At: m.At, Text: m.Text}
				if !have[store.LabelKey(m.ChatID, m.ThreadID, store.TextHash(m.Text))] {
					todo = true
				}
			}
			if !todo {
				continue
			}
			out, err := route.ClaudeCLI(ctx, model, route.JudgePrompt(cands[k.chat], items), 5*time.Minute)
			calls++
			if err != nil {
				fmt.Fprintf(os.Stderr, "judge: chat %d thread %d batch %d: %v\n", k.chat, k.thread, start/judgeBatch, err)
				continue
			}
			labels, err := route.ParseJudge(out, cands[k.chat])
			if err != nil {
				fmt.Fprintf(os.Stderr, "judge: chat %d thread %d batch %d: %v\n", k.chat, k.thread, start/judgeBatch, err)
				continue
			}
			for _, l := range labels {
				if l.N < 1 || l.N > len(batch) {
					continue
				}
				m := batch[l.N-1]
				if err := st.UpsertRouteLabel(store.RouteLabel{ChatID: m.ChatID, ThreadID: m.ThreadID,
					TextHash: store.TextHash(m.Text), Lane: l.Lane, Source: "judge", Sure: l.Sure}); err != nil {
					return err
				}
				labelled++
			}
		}
	}
	fmt.Printf("judge (%s): %d labels from %d calls\n", model, labelled, calls)
	return nil
}

// backendScore is one backend's numbers against the labels.
type backendScore struct {
	rows, labelled, correct          int
	nonGeneral, nonGeneralHit        int // labels that are a project, and how many the backend got
	predProject, predProjectHit      int // backend said a project, and how often right
	generalLabels                    int // for the always-general baseline
	changes, transitions, stickyRows int
	latencies                        []int64
}

func runRouteReport(st *store.Store, source string, days, sample int) error {
	rows, err := st.RouteDecisions(source, time.Now().AddDate(0, 0, -days-1))
	if err != nil {
		return err
	}
	labels, err := st.BestRouteLabels()
	if err != nil {
		return err
	}
	scores, srcCount := scoreRoutes(rows, labels)
	if len(scores) == 0 {
		fmt.Printf("no %s decisions in the last %d days\n", source, days)
		return nil
	}
	pct := func(a, b int) string {
		if b == 0 {
			return "—"
		}
		return fmt.Sprintf("%.0f%% (%d/%d)", 100*float64(a)/float64(b), a, b)
	}
	var names []string
	for n := range scores {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("Route report — source=%s, last %d days. Labels by source: %v\n\n", source, days, srcCount)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BACKEND\tROWS\tAGREE\tPROJECT MSGS FOUND\tPROJECT PICKS RIGHT\tLANE CHANGES\tSTICKY\tP50 MS")
	var baseline *backendScore
	for _, n := range names {
		s := scores[n]
		if baseline == nil || s.labelled > baseline.labelled {
			baseline = s
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", n, s.rows, pct(s.correct, s.labelled),
			pct(s.nonGeneralHit, s.nonGeneral), pct(s.predProjectHit, s.predProject),
			pct(s.changes, s.transitions), pct(s.stickyRows, s.rows), p50(s.latencies))
	}
	if baseline != nil {
		fmt.Fprintf(tw, "always-general\t—\t%s\t%s\t—\t0%%\t—\t—\n",
			pct(baseline.generalLabels, baseline.labelled), pct(0, baseline.nonGeneral))
	}
	tw.Flush()
	fmt.Println("\nAGREE = lane matches the best label. PROJECT MSGS FOUND = recall on messages labelled as a project;")
	fmt.Println("the always-general baseline scores 0 there, so that column is the one a router has to win.")
	fmt.Println("Pass bar (design, R0): ≥85% agreement AND beats the baseline on project messages; lane changes < 20%.")

	if sample > 0 {
		printRouteSample(st, rows, labels, days, sample)
	}
	return nil
}

// scoreRoutes computes each backend's numbers against the best labels.
func scoreRoutes(rows []store.RouteDecision, labels map[string]store.RouteLabel) (map[string]*backendScore, map[string]int) {
	scores := map[string]*backendScore{}
	lastLane := map[string]string{} // backend|chat|thread → lane
	srcCount := map[string]int{}
	counted := map[string]bool{}
	for _, r := range rows {
		s := scores[r.Backend]
		if s == nil {
			s = &backendScore{}
			scores[r.Backend] = s
		}
		s.rows++
		if r.Sticky {
			s.stickyRows++
		}
		if r.LatencyMS > 0 {
			s.latencies = append(s.latencies, r.LatencyMS)
		}
		tk := fmt.Sprintf("%s|%d|%d", r.Backend, r.ChatID, r.ThreadID)
		if prev, ok := lastLane[tk]; ok {
			s.transitions++
			if prev != r.Lane {
				s.changes++
			}
		}
		lastLane[tk] = r.Lane
		l, ok := labels[store.LabelKey(r.ChatID, r.ThreadID, r.TextHash)]
		if !ok {
			continue
		}
		s.labelled++
		if !counted[r.TextHash+l.Source] {
			counted[r.TextHash+l.Source] = true
			srcCount[l.Source]++
		}
		if l.Lane == r.Lane {
			s.correct++
		}
		if l.Lane == route.General {
			s.generalLabels++
		} else {
			s.nonGeneral++
			if r.Lane == l.Lane {
				s.nonGeneralHit++
			}
		}
		if r.Lane != route.General {
			s.predProject++
			if r.Lane == l.Lane {
				s.predProjectHit++
			}
		}
	}
	return scores, srcCount
}

// printRouteSample shows labelled messages with every backend's lane, for
// the owner to spot-check the judge. Local terminal only.
func printRouteSample(st *store.Store, rows []store.RouteDecision, labels map[string]store.RouteLabel, days, n int) {
	msgs, err := st.UserMessagesSince(time.Now().AddDate(0, 0, -days-1))
	if err != nil {
		return
	}
	text := map[string]string{}
	for _, m := range msgs {
		text[store.LabelKey(m.ChatID, m.ThreadID, store.TextHash(m.Text))] = m.Text
	}
	byKey := map[string][]string{}
	var keys []string
	for _, r := range rows {
		k := store.LabelKey(r.ChatID, r.ThreadID, r.TextHash)
		if _, ok := labels[k]; !ok {
			continue
		}
		if _, ok := byKey[k]; !ok {
			keys = append(keys, k)
		}
		byKey[k] = append(byKey[k], r.Backend+"="+r.Lane)
	}
	// Disagreements first: those are the ones worth a human look.
	sort.SliceStable(keys, func(i, j int) bool {
		return !allAgree(byKey[keys[i]], labels[keys[i]].Lane) && allAgree(byKey[keys[j]], labels[keys[j]].Lane)
	})
	fmt.Println("\nSample (disagreements first). Override with: shell route label <key> <lane>")
	for i, k := range keys {
		if i == n {
			break
		}
		l := labels[k]
		t := []rune(strings.Join(strings.Fields(text[k]), " "))
		if len(t) > 70 {
			t = append(t[:70], '…')
		}
		sure := ""
		if !l.Sure {
			sure = " (unsure)"
		}
		fmt.Printf("- %s  label=%s[%s%s]  %s\n    %s\n", k, l.Lane, l.Source, sure, strings.Join(byKey[k], " "), string(t))
	}
}

func allAgree(lanes []string, label string) bool {
	for _, l := range lanes {
		if !strings.HasSuffix(l, "="+label) {
			return false
		}
	}
	return true
}

func p50(v []int64) string {
	if len(v) == 0 {
		return "—"
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	return fmt.Sprintf("%d", v[len(v)/2])
}
