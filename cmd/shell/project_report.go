package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/config"
	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/store"
	"github.com/spf13/cobra"
)

// newProjectReportCmd measures focus per area (docs/DESIGN-PROJECT-AREAS.md):
// how much of each project's talk happens in its own post, how often its doc
// moves, and which projects have gone quiet. Read-only; the weekly check
// reads it.
func newProjectReportCmd(openStore func() (config.Config, *store.Store, error)) *cobra.Command {
	var days int
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Focus per area: talk in each project's post vs elsewhere, doc updates, stage, staleness",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, st, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
			projects, err := st.ListProjects(0)
			if err != nil {
				return err
			}
			ws := workspaceDirFrom(cfg)
			fmt.Printf("Focus report, last %d days (since %s)\n", days, since.Local().Format("Mon Jan 2"))

			var areas []store.Project
			byArea := map[string][]store.Project{}
			for _, p := range projects {
				if p.Status != "active" && !(p.Status == "archived" && p.UpdatedAt.After(since)) {
					continue
				}
				if p.Kind == store.ProjectKindArea {
					areas = append(areas, p)
				} else {
					byArea[p.Area] = append(byArea[p.Area], p)
				}
			}
			var inPostTotal, elsewhereTotal int
			row := func(p store.Project) {
				inPost, elsewhere := 0, 0
				if p.MessageThreadID != 0 {
					inPost, _ = st.HumanMessagesInThread(p.ChatID, p.MessageThreadID, since)
				}
				elsewhere, _ = st.LaneRoutedElsewhere(p.ChatID, p.Slug, p.MessageThreadID, since)
				inPostTotal += inPost
				elsewhereTotal += elsewhere
				last := "never"
				stale := ""
				if t := st.LastHumanTouch(p); t != nil {
					last = t.Local().Format("Jan 2")
					if time.Since(*t) > 21*24*time.Hour && p.Status == "active" {
						stale = "  STALE"
					}
				}
				stage := p.Stage
				if stage == "" {
					stage = "-"
				}
				fmt.Printf("  %-28s %-8s stage %-6s in-post %3d  elsewhere %3d  doc commits %2d  last human %s%s\n",
					truncateRunes(p.Slug, 28), p.Status, stage, inPost, elsewhere, docCommits(ws, p.Slug, since), last, stale)
			}
			for _, a := range areas {
				fmt.Printf("\n%s %s (area, chat %d)  doc commits %d\n", a.Emoji, a.Slug, a.ChatID, docCommits(ws, a.Slug, since))
				if len(byArea[a.Slug]) == 0 {
					fmt.Println("  (no projects)")
				}
				for _, p := range byArea[a.Slug] {
					row(p)
				}
				delete(byArea, a.Slug)
			}
			var loose []store.Project
			for _, ps := range byArea {
				loose = append(loose, ps...)
			}
			if len(loose) > 0 {
				fmt.Println("\nNo area")
				for _, p := range loose {
					row(p)
				}
			}
			if total := inPostTotal + elsewhereTotal; total > 0 {
				fmt.Printf("\nIn their own post: %d of %d project messages (%d%%)\n", inPostTotal, total, inPostTotal*100/total)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 7, "window in days")
	return cmd
}

// docCommits counts commits in the project's doc repo inside the window
// (0 when it has no managed doc).
func docCommits(ws, slug string, since time.Time) int {
	dir, ok := project.ManagedDocDir(ws, slug)
	if !ok {
		return 0
	}
	out, err := exec.Command("git", "-C", dir, "rev-list", "--count", "--since="+since.Format(time.RFC3339), "HEAD").Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
