package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/rcliao/shell/internal/config"
	"github.com/rcliao/shell/internal/skill"
	"github.com/rcliao/shell/internal/store"
)

// shell skills report — how distinct each agent is becoming (design:
// docs/DESIGN-CONTEXT-AND-SKILLS.md). Per agent: every skill it sees, where
// it lives, whether it actually reaches the prompt, how much it is used,
// the playground drafts, and how many skill changes the agent committed.
func newSkillsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "skills", Short: "Inspect agent skills"}
	var days int
	var only string
	report := &cobra.Command{
		Use:   "report",
		Short: "Per-agent skills: tier, prompt fit, real usage, drafts, self-authored commits",
		RunE: func(cmd *cobra.Command, args []string) error {
			agents, err := filepath.Glob(filepath.Join(config.DefaultConfigDir(), "agents", "*", "config.json"))
			if err != nil {
				return err
			}
			if len(agents) == 0 {
				return fmt.Errorf("no agent configs under %s", filepath.Join(config.DefaultConfigDir(), "agents"))
			}
			for _, path := range agents {
				name := filepath.Base(filepath.Dir(path))
				if only != "" && name != only {
					continue
				}
				skillsReport(os.Stdout, name, loadConfigFrom(path), days)
			}
			return nil
		},
	}
	report.Flags().IntVar(&days, "days", 30, "usage window in days")
	report.Flags().StringVar(&only, "agent", "", "only this agent (directory name under ~/.shell/agents)")
	cmd.AddCommand(report)
	return cmd
}

func skillsReport(w *os.File, agent string, cfg config.Config, days int) {
	ownDir := ""
	if cfg.Daemon.PIDFile != "" {
		ownDir = filepath.Join(filepath.Dir(cfg.Daemon.PIDFile), "skills")
	}
	fmt.Fprintf(w, "\n== %s ==\n", agent)

	reg := loadSkillRegistryFromConfig(cfg)
	if reg == nil {
		fmt.Fprintln(w, "  no skills")
		return
	}
	demoted := map[string]bool{}
	for _, d := range reg.Demoted() {
		demoted[strings.SplitN(d, " (", 2)[0]] = true
	}

	var usage map[string]store.SkillUse
	if st, err := store.Open(cfg.Store.DBPath); err == nil {
		usage, err = st.SkillUsage(time.Now().AddDate(0, 0, -days))
		st.Close()
		if err != nil {
			fmt.Fprintf(w, "  (usage unavailable: %v)\n", err)
		}
	} else {
		fmt.Fprintf(w, "  (usage unavailable: %v)\n", err)
	}

	skills := reg.All()
	sort.Slice(skills, func(i, j int) bool {
		if skills[i].Own != skills[j].Own {
			return skills[i].Own
		}
		return skills[i].Name < skills[j].Name
	})
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  SKILL\tOWNER\tTIER\tPROMPT\tRUNS/%dd\tFAIL\tREADS\tLAST USE\tEDITED\n", days)
	for _, s := range skills {
		owner := "shared"
		if s.Own {
			owner = "own"
		}
		tier := s.Tier // resolved at load: legacy core, default lazy, drafts capped to lazy
		prompt := "catalog line"
		switch {
		case tier == skill.TierCore:
			prompt = "full"
		case tier == skill.TierHot && demoted[s.Name]:
			prompt = fmt.Sprintf("NOT LOADED (%d tok)", skill.HotTokens(s))
		case tier == skill.TierHot:
			prompt = fmt.Sprintf("%d tok", skill.HotTokens(s))
		}
		u := usage[s.Name]
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\n", s.Name, owner, tier, prompt,
			u.Runs, u.Failures, u.Reads, ago(u.Last), ago(modTime(filepath.Join(s.Dir, "SKILL.md"))))
	}
	tw.Flush()

	if ownDir == "" {
		return
	}
	var drafts []string
	if entries, err := os.ReadDir(filepath.Join(ownDir, "playground")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				u := usage["playground/"+e.Name()]
				drafts = append(drafts, fmt.Sprintf("%s (%d runs)", e.Name(), u.Runs))
			}
		}
	}
	if len(drafts) == 0 {
		fmt.Fprintln(w, "  playground: empty")
	} else {
		fmt.Fprintf(w, "  playground: %s\n", strings.Join(drafts, ", "))
	}
	fmt.Fprintf(w, "  self-authored commits: %s\n", selfCommits(ownDir, agent, days))
}

// selfCommits counts commits under the agent's skills dir authored by the
// agent (the bridge commits as the agent's name), total and in the window.
func selfCommits(dir, agent string, days int) string {
	count := func(extra ...string) (int, error) {
		args := append([]string{"-C", dir, "log", "--format=%h", "--author=^" + agent + " "}, extra...)
		out, err := exec.Command("git", append(args, "--", ".")...).Output()
		if err != nil {
			return 0, err
		}
		return len(strings.Fields(string(out))), nil
	}
	total, err := count()
	if err != nil {
		return "not a git repo"
	}
	recent, _ := count(fmt.Sprintf("--since=%d.days", days))
	return fmt.Sprintf("%d total, %d in %d days", total, recent, days)
}

func modTime(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
