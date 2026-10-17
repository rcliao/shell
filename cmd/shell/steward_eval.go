package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rcliao/shell/internal/bridge"
	"github.com/rcliao/shell/internal/config"
	"github.com/rcliao/shell/internal/judge"
	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/stewardeval"
	"github.com/rcliao/shell/internal/store"
	"github.com/spf13/cobra"
)

// newStewardCmd groups the project steward's owner tools.
func newStewardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "steward",
		Short: "Project steward tools",
	}
	cmd.AddCommand(newStewardEvalCmd())
	return cmd
}

// evalCell is one graded proposal: a generated one (project × variant ×
// sample) or, in baseline mode, a filed one.
type evalCell struct {
	Project  string               `json:"project"`
	Variant  string               `json:"variant"`
	Sample   int                  `json:"sample,omitempty"`
	FiledID  int64                `json:"filed_id,omitempty"`
	At       time.Time            `json:"at"`
	Prompt   string               `json:"prompt,omitempty"`
	Raw      string               `json:"raw,omitempty"`
	Proposal stewardeval.Proposal `json:"proposal"`
	Format   []string             `json:"format_fails"`
	Grade    stewardeval.Grade    `json:"grade,omitempty"`
	JudgeIn  string               `json:"judge_prompt,omitempty"`
	CostUSD  float64              `json:"cost_usd"`
	Error    string               `json:"error,omitempty"`
}

// newStewardEvalCmd grades steward proposals offline
// (docs/DESIGN-STEWARD-EVAL.md). It works on a read-only copy of the
// agent's shell.db and writes every cell to ~/.shell/steward-eval/.
func newStewardEvalCmd() *cobra.Command {
	var configFlag, variantsFlag, only, genModel, judgeModel, outDir string
	var samples, parallel int
	var budget float64
	var baseline bool
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Generate steward proposals per prompt variant and grade them (new / agent_led / useful / concrete / grounded / format)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfigFrom(configFlag)
			agent := cfg.Agent.Name
			if agent == "" {
				agent = "agent"
			}
			if genModel == "" {
				genModel = cfg.Claude.Model
			}
			if outDir == "" {
				outDir = filepath.Join(config.DefaultConfigDir(), "steward-eval", time.Now().Format("20060102-150405")+"-"+agent)
			}
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return err
			}
			st, err := openSnapshot(cfg.Store.DBPath, filepath.Join(outDir, "shell.db"))
			if err != nil {
				return err
			}
			defer st.Close()
			ws := workspaceDirFrom(cfg)
			projects, err := st.ListProjects(0)
			if err != nil {
				return err
			}
			bySlug := map[string]store.Project{}
			for _, p := range projects {
				bySlug[p.Slug] = p
			}
			mode := bridge.StewardInThreads
			if cfg.Scheduler.ProjectSteward == bridge.StewardShadow {
				mode = bridge.StewardShadow
			}

			// Plan the cells.
			var cells []*evalCell
			now := time.Now()
			if baseline {
				filed, err := st.StewardSince(time.Time{})
				if err != nil {
					return err
				}
				for _, s := range filed {
					if s.Project == "" {
						s.Project = slugFromTitle(s.Title, bySlug)
					}
					if only != "" && s.Project != only {
						continue
					}
					cells = append(cells, &evalCell{Project: s.Project, Variant: "filed", FiledID: s.ID, At: s.CreatedAt, Proposal: stewardeval.FiledProposal(s)})
				}
			} else {
				variants := stewardeval.Variants(mode)
				names := strings.Split(variantsFlag, ",")
				for _, v := range names {
					if _, ok := variants[v]; !ok {
						return fmt.Errorf("unknown variant %q (base, a, b, c)", v)
					}
				}
				for _, p := range projects {
					if p.Status != "active" || !bridge.OwnsDoc(ws, agent, p.Slug) || (only != "" && p.Slug != only) {
						continue
					}
					for _, v := range names {
						for i := 1; i <= samples; i++ {
							cells = append(cells, &evalCell{Project: p.Slug, Variant: v, Sample: i, At: now})
						}
					}
				}
			}
			fmt.Printf("Steward eval for %s: %d cells, generator %s, judge %s, budget $%.2f\nResults: %s\n\n", agent, len(cells), genModel, judgeModel, budget, outDir)

			gen, grade := judge.CLI(genModel, nil), judge.CLI(judgeModel, nil)
			var mu sync.Mutex
			spent := 0.0
			out, err := os.Create(filepath.Join(outDir, "cells.jsonl"))
			if err != nil {
				return err
			}
			defer out.Close()
			call := func(c *evalCell, run judge.Runner, prompt string) (string, bool) {
				mu.Lock()
				over := spent >= budget
				mu.Unlock()
				if over {
					c.Error = "budget spent"
					return "", false
				}
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
				defer cancel()
				raw, err := run(ctx, prompt)
				text, cost, perr := judge.Result(raw)
				mu.Lock()
				spent += cost
				mu.Unlock()
				c.CostUSD += cost
				if err == nil {
					err = perr
				}
				if err != nil {
					c.Error = err.Error()
					return "", false
				}
				return text, true
			}
			sem := make(chan struct{}, parallel)
			var wg sync.WaitGroup
			for _, c := range cells {
				wg.Add(1)
				sem <- struct{}{}
				go func(c *evalCell) {
					defer func() { <-sem; wg.Done() }()
					p := bySlug[c.Project]
					doc := bridge.ReadProjectDoc(ws, p.DocPath)
					if c.FiledID != 0 {
						if dir, ok := project.ManagedDocDir(ws, p.Slug); ok {
							if old, err := project.DocAt(dir, c.At); err == nil && old != "" {
								doc = old
							}
						}
					} else {
						opts := stewardeval.Variants(mode)[c.Variant]
						c.Prompt = stewardeval.GenPrompt(agent, bridge.StewardEntry(st, p, doc, c.At, opts), doc, opts)
						text, ok := call(c, gen, c.Prompt)
						if !ok {
							goto done
						}
						c.Raw = text
						pr, err := stewardeval.ParseProposal(text)
						if err != nil {
							c.Error = "proposal: " + err.Error()
							goto done
						}
						c.Proposal = pr
						if pr.Kind == "none" {
							goto done
						}
					}
					c.Format = stewardeval.FormatCheck(c.Proposal.Change, p.Lang)
					{
						in := stewardeval.JudgeInput{Project: p, Doc: doc, At: c.At, P: c.Proposal}
						var err error
						if in.Thread, err = st.ThreadMessagesBetween(p.ChatID, p.MessageThreadID, c.At.Add(-30*24*time.Hour), c.At, 60); err != nil {
							c.Error = "thread: " + err.Error()
							goto done
						}
						asked, err := st.StewardForProject(p.Slug, c.At.Add(-30*24*time.Hour), c.At)
						if err != nil {
							c.Error = "history: " + err.Error()
							goto done
						}
						for _, s := range asked {
							if s.ID != c.FiledID {
								in.Asked = append(in.Asked, s)
							}
						}
						c.JudgeIn = stewardeval.JudgePrompt(in)
						text, ok := call(c, grade, c.JudgeIn)
						if !ok {
							goto done
						}
						g, err := stewardeval.ParseGrade(text)
						if err != nil {
							c.Error = "grade: " + err.Error()
							goto done
						}
						c.Grade = g
					}
				done:
					line, _ := json.Marshal(c)
					mu.Lock()
					out.Write(append(line, '\n'))
					mark := "ok"
					if c.Error != "" {
						mark = "error: " + clip(c.Error, 80)
					} else if c.Proposal.Kind == "none" {
						mark = "none"
					}
					fmt.Printf("  %-6s %-40s %s\n", c.Variant, truncateRunes(c.Project, 40), mark)
					mu.Unlock()
				}(c)
			}
			wg.Wait()
			fmt.Println()
			printStewardTable(os.Stdout, cells)
			fmt.Printf("\nSpent $%.2f\n", spent)
			return nil
		},
	}
	cmd.Flags().StringVar(&configFlag, "config", "", "agent config path (e.g. ~/.shell/agents/<agent>/config.json)")
	cmd.Flags().StringVar(&variantsFlag, "variants", "base,a,b", "prompt variants to compare (base, a, b, c)")
	cmd.Flags().StringVar(&only, "project", "", "only this project slug")
	cmd.Flags().StringVar(&genModel, "gen-model", "", "generator model (default: the agent's claude.model)")
	cmd.Flags().StringVar(&judgeModel, "judge-model", "claude-sonnet-5-5", "judge model")
	cmd.Flags().StringVar(&outDir, "out", "", "results directory (default ~/.shell/steward-eval/<ts>-<agent>)")
	cmd.Flags().IntVar(&samples, "samples", 1, "generations per project and variant")
	cmd.Flags().IntVar(&parallel, "parallel", 4, "concurrent model calls")
	cmd.Flags().Float64Var(&budget, "budget", 5, "stop starting model calls once this many USD are spent")
	cmd.Flags().BoolVar(&baseline, "baseline", false, "grade the agent's filed steward proposals instead of generating")
	return cmd
}

// slugFromTitle finds the project of a shadow-era steward row, filed
// without one: its title starts "<slug>: …", sometimes with the slug cut short.
func slugFromTitle(title string, bySlug map[string]store.Project) string {
	head, _, ok := strings.Cut(title, ":")
	if !ok {
		return ""
	}
	head = strings.TrimSpace(head)
	if _, ok := bySlug[head]; ok {
		return head
	}
	for slug := range bySlug {
		if head != "" && strings.HasPrefix(slug, head) {
			return slug
		}
	}
	return ""
}

// openSnapshot copies the agent's live shell.db (read-only) into dst and
// opens the copy, so the eval never migrates or writes a live daemon's DB.
func openSnapshot(src, dst string) (*store.Store, error) {
	as, _ := filepath.Abs(src)
	ad, _ := filepath.Abs(dst)
	if as == ad {
		return nil, fmt.Errorf("snapshot would overwrite the live db %s; pick another --out", src)
	}
	uri := (&url.URL{Scheme: "file", Path: src, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	os.Remove(dst)
	if _, err := db.Exec(`VACUUM INTO ?`, dst); err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", src, err)
	}
	return store.Open(dst)
}

// printStewardTable prints one row per variant: counts, kind mix and the
// pass rate of each check over graded proposals.
func printStewardTable(w *os.File, cells []*evalCell) {
	type row struct {
		n, errs, none, graded, formatOK, allOK int
		kinds                                  map[string]int
		pass                                   map[string]int
		cost                                   float64
	}
	rows := map[string]*row{}
	for _, c := range cells {
		r := rows[c.Variant]
		if r == nil {
			r = &row{kinds: map[string]int{}, pass: map[string]int{}}
			rows[c.Variant] = r
		}
		r.n++
		r.cost += c.CostUSD
		switch {
		case c.Error != "":
			r.errs++
			continue
		case c.Proposal.Kind == "none":
			r.none++
			continue
		}
		r.kinds[c.Proposal.Kind]++
		r.graded++
		all := len(c.Format) == 0
		if all {
			r.formatOK++
		}
		for _, k := range stewardeval.Checks {
			if c.Grade[k].Pass {
				r.pass[k]++
			} else {
				all = false
			}
		}
		if all {
			r.allOK++
		}
	}
	names := make([]string, 0, len(rows))
	for v := range rows {
		names = append(names, v)
	}
	sort.Strings(names)
	pct := func(a, b int) string {
		if b == 0 {
			return "-"
		}
		return fmt.Sprintf("%d%%", a*100/b)
	}
	fmt.Fprintf(w, "%-7s %5s %4s %4s %-11s", "variant", "cells", "err", "none", "do/ask/goal")
	for _, k := range stewardeval.Checks {
		fmt.Fprintf(w, " %9s", k)
	}
	fmt.Fprintf(w, " %7s %7s %7s\n", "format", "all", "cost")
	for _, v := range names {
		r := rows[v]
		fmt.Fprintf(w, "%-7s %5d %4d %4d %-11s", v, r.n, r.errs, r.none, fmt.Sprintf("%d/%d/%d", r.kinds["do"], r.kinds["ask"], r.kinds["goal"]))
		for _, k := range stewardeval.Checks {
			fmt.Fprintf(w, " %9s", pct(r.pass[k], r.graded))
		}
		fmt.Fprintf(w, " %7s %7s %7s\n", pct(r.formatOK, r.graded), pct(r.allOK, r.graded), fmt.Sprintf("$%.2f", r.cost))
	}
}
