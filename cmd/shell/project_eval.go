package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/rcliao/shell/internal/config"
	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/route"
	"github.com/rcliao/shell/internal/store"
	"github.com/spf13/cobra"
)

// newProjectEvalCmd measures whether project context helps replies
// (docs/DESIGN-PROJECT-AREAS.md, part 4): it samples the agent's replies in
// project posts and has an independent judge compare each with the doc as
// it stood at that moment. Costs one judge call per sampled reply.
func newProjectEvalCmd(openStore func() (config.Config, *store.Store, error)) *cobra.Command {
	var days, max int
	var model string
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Judge whether replies in project posts used the project's doc (used / ignored / contradicted / reasked / na)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, st, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			if model == "" {
				model = cfg.Route.Judge()
			}
			since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
			ws := workspaceDirFrom(cfg)
			projects, err := st.ListProjects(0)
			if err != nil {
				return err
			}
			type sample struct {
				p    store.Project
				pair store.ReplyPair
			}
			var samples []sample
			for _, p := range projects {
				if p.Status != "active" || p.Kind == store.ProjectKindArea || p.MessageThreadID == 0 {
					continue
				}
				pairs, err := st.ReplyPairsInThread(p.ChatID, p.MessageThreadID, since)
				if err != nil {
					return err
				}
				for _, pr := range pairs {
					samples = append(samples, sample{p, pr})
				}
			}
			// Newest first, spread across projects by taking every k-th.
			sort.Slice(samples, func(i, j int) bool { return samples[i].pair.At.After(samples[j].pair.At) })
			if len(samples) > max {
				step := float64(len(samples)) / float64(max)
				var picked []sample
				for i := 0; i < max; i++ {
					picked = append(picked, samples[int(float64(i)*step)])
				}
				samples = picked
			}
			fmt.Printf("Project context eval, last %d days: %d replies judged by %s\n\n", days, len(samples), model)
			counts := map[string]int{}
			perProject := map[string]map[string]int{}
			for _, s := range samples {
				dir, ok := project.ManagedDocDir(ws, s.p.Slug)
				if !ok {
					continue
				}
				doc, _ := project.DocAt(dir, s.pair.At)
				excerpt := project.ContextExcerpt(doc)
				if excerpt == "" {
					counts["na"]++
					continue
				}
				out, err := route.ClaudeCLI(context.Background(), model, project.ContextJudgePrompt(s.p.Title, excerpt, s.pair.User, s.pair.Reply), 2*time.Minute)
				if err != nil {
					fmt.Printf("  judge failed (%s): %v\n", s.p.Slug, err)
					continue
				}
				v, why, err := project.ParseContextVerdict(out)
				if err != nil {
					fmt.Printf("  unreadable verdict (%s): %v\n", s.p.Slug, err)
					continue
				}
				counts[v]++
				if perProject[s.p.Slug] == nil {
					perProject[s.p.Slug] = map[string]int{}
				}
				perProject[s.p.Slug][v]++
				if v == "ignored" || v == "contradicted" || v == "reasked" {
					fmt.Printf("  %-12s %s %s — %s\n", v, s.p.Slug, s.pair.At.Local().Format("Jan 2 15:04"), why)
				}
			}
			fmt.Println()
			for slug, c := range perProject {
				fmt.Printf("  %-30s", truncateRunes(slug, 30))
				for _, v := range project.ContextVerdicts {
					fmt.Printf(" %s %d", v, c[v])
				}
				fmt.Println()
			}
			fmt.Print("\nTotal:")
			for _, v := range project.ContextVerdicts {
				fmt.Printf(" %s %d", v, counts[v])
			}
			relevant := counts["used"] + counts["ignored"] + counts["contradicted"] + counts["reasked"]
			if relevant > 0 {
				fmt.Printf("\nContext used when relevant: %d of %d (%d%%)\n", counts["used"], relevant, counts["used"]*100/relevant)
			} else {
				fmt.Println()
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 7, "window in days")
	cmd.Flags().IntVar(&max, "max", 20, "most replies to judge (one judge call each)")
	cmd.Flags().StringVar(&model, "model", "", "judge model (default: route.judge_model)")
	return cmd
}
