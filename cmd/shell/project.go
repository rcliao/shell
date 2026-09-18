package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rcliao/shell/internal/config"
	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/store"
)

// shell project — owner ops for the project registry (P2, docs/
// PLAN-PROJECT-WORKSPACE.md): list/show/archive/bind. Direct store access,
// same as `shell status` and `shell session`; the agent-facing surface is the
// RPC + skill, and the family user gets natural language only.
func newProjectCmd() *cobra.Command {
	var configFlag string

	projectCmd := &cobra.Command{
		Use:   "project",
		Short: "Manage projects: list, show, archive, bind",
	}
	projectCmd.PersistentFlags().StringVar(&configFlag, "config", "",
		"agent config path (e.g. ~/.shell/agents/<agent>/config.json); default ~/.shell/config.json")

	openStore := func() (config.Config, *store.Store, error) {
		cfg := loadConfigFrom(configFlag)
		st, err := store.Open(cfg.Store.DBPath)
		return cfg, st, err
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List projects across all chats",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, st, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()

			projects, err := st.ListProjects(0)
			if err != nil {
				return err
			}
			if len(projects) == 0 {
				fmt.Println("No projects.")
				return nil
			}
			for _, p := range projects {
				emoji := p.Emoji
				if emoji == "" {
					emoji = "•"
				}
				fmt.Printf("%s %-24s [%s]  chat %d", emoji, p.Slug, p.Status, p.ChatID)
				if p.MessageThreadID != 0 {
					fmt.Printf(" thread %d", p.MessageThreadID)
				}
				fmt.Printf("  %s\n", p.Title)
				if p.DocPath != "" {
					fmt.Printf("      doc: %s @ %s\n", p.DocPath, shortRev(p.DocRev))
				}
				if p.LastResearchAt != nil {
					fmt.Printf("      last research: %s\n", p.LastResearchAt.Local().Format("2006-01-02 15:04"))
				}
			}
			return nil
		},
	}

	showCmd := &cobra.Command{
		Use:   "show <slug>",
		Short: "Show one project: row, doc history, research schedule state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, st, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()

			p, err := st.GetProjectBySlug(args[0])
			if err != nil {
				return err
			}
			if p == nil {
				return fmt.Errorf("project %q does not exist", args[0])
			}

			fmt.Printf("%s %s [%s] — %s\n", p.Emoji, p.Slug, p.Status, p.Title)
			fmt.Printf("  chat: %d  thread: %d  lang: %s  notify: %s\n",
				p.ChatID, p.MessageThreadID, stringOr(p.Lang, "-"), p.NotifyPolicy)
			if p.Instructions != "" {
				fmt.Printf("  instructions: %s\n", p.Instructions)
			}
			if p.ExportRef != "" {
				fmt.Printf("  export: %s:%s\n", p.ExportKind, p.ExportRef)
			}
			if p.DocPath != "" {
				fmt.Printf("  doc: %s @ %s\n", p.DocPath, shortRev(p.DocRev))
			}
			if p.LastResearchAt != nil {
				fmt.Printf("  last research: %s\n", p.LastResearchAt.Local().Format("2006-01-02 15:04"))
			}
			if p.LastHumanActivityAt != nil {
				fmt.Printf("  last human activity: %s\n", p.LastHumanActivityAt.Local().Format("2006-01-02 15:04"))
			}

			// Comment loop state (Wave D): processed discussions + the poll
			// watermark (the page's last seen last_edited_time).
			handled := store.ParseHandledDiscussions(p.HandledDiscussions)
			if len(handled) > 0 {
				line := fmt.Sprintf("  comments handled: %d", len(handled))
				if last := handled[len(handled)-1]; !last.At.IsZero() {
					line += ", last " + last.At.Local().Format("2006-01-02 15:04")
				}
				fmt.Println(line)
			}
			if p.NotionWatermark != "" {
				fmt.Printf("  notion watermark: %s\n", p.NotionWatermark)
			}

			// Research schedule state, found by its explicit dedup key.
			key := p.ScheduleDedupKey
			if key == "" {
				key = project.ScheduleDedupKey(p.Slug)
			}
			if sc, err := st.FindScheduleByDedupKey(key); err == nil && sc != nil {
				state := "enabled"
				if !sc.Enabled {
					state = "disabled"
					if sc.PausedReason != "" {
						state = "paused:" + sc.PausedReason
					}
				}
				next := sc.NextRunAt
				if sc.ExpectedNextAt != nil {
					next = *sc.ExpectedNextAt
				}
				fmt.Printf("  schedule: #%d %s (%s) next %s\n",
					sc.ID, state, sc.Schedule, next.Local().Format("2006-01-02 15:04"))
			} else {
				fmt.Println("  schedule: none")
			}

			// Doc history — only a MANAGED doc has one here.
			if dir, ok := project.ManagedDocDir(workspaceDirFrom(cfg), p.Slug); ok {
				if log, err := project.DocLog(dir, 10); err == nil && log != "" {
					fmt.Println("  doc history:")
					fmt.Println(indent(log, "    "))
				}
			}
			return nil
		},
	}

	archiveCmd := &cobra.Command{
		Use:   "archive <slug>",
		Short: "Archive a project and disable its research schedule",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, st, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()

			p, err := st.GetProjectBySlug(args[0])
			if err != nil {
				return err
			}
			if p == nil {
				return fmt.Errorf("project %q does not exist", args[0])
			}
			if err := st.UpdateProjectStatus(p.Slug, "archived"); err != nil {
				return err
			}
			fmt.Printf("Project %s archived.\n", p.Slug)

			key := p.ScheduleDedupKey
			if key == "" {
				key = project.ScheduleDedupKey(p.Slug)
			}
			sc, err := st.FindScheduleByDedupKey(key)
			if err != nil {
				return err
			}
			if sc != nil && sc.Enabled {
				if err := st.PauseSchedule(sc.ID, "project_archived"); err != nil {
					return err
				}
				fmt.Printf("Research schedule #%d disabled.\n", sc.ID)
			}
			// The pinned 📋 list is refreshed by the daemon on its next
			// trigger (or /projects) — the CLI writes the store only.
			return nil
		},
	}

	var bindChatFlag, bindThreadFlag int64
	bindCmd := &cobra.Command{
		Use:   "bind <slug> --chat <id> [--thread <id>]",
		Short: "Re-bind a project to a chat (and optional forum topic)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if bindChatFlag == 0 {
				return fmt.Errorf("--chat is required")
			}
			_, st, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()

			if err := st.UpdateProjectFields(args[0], store.ProjectFieldUpdate{
				ChatID: &bindChatFlag, MessageThreadID: &bindThreadFlag,
			}); err != nil {
				return err
			}
			// Read back rather than reporting success from the write call.
			p, err := st.GetProjectBySlug(args[0])
			if err != nil || p == nil {
				return fmt.Errorf("bind read-back failed for %q", args[0])
			}
			fmt.Printf("Project %s bound to chat %d thread %d.\n", p.Slug, p.ChatID, p.MessageThreadID)
			fmt.Println("Note: the research schedule's event payload still carries the old chat; archive + re-create the project to move schedules across chats.")
			return nil
		},
	}
	bindCmd.Flags().Int64Var(&bindChatFlag, "chat", 0, "target chat id")
	bindCmd.Flags().Int64Var(&bindThreadFlag, "thread", 0, "Telegram forum topic id (0 = main chat)")

	projectCmd.AddCommand(listCmd, showCmd, archiveCmd, bindCmd)
	return projectCmd
}

// workspaceDirFrom mirrors the daemon's derivation: the agent workspace lives
// beside the pid file (<agent-home>/workspace).
func workspaceDirFrom(cfg config.Config) string {
	pidDir := filepath.Dir(cfg.Daemon.PIDFile)
	if pidDir == "" || pidDir == "." {
		pidDir = config.DefaultConfigDir()
	}
	return filepath.Join(pidDir, "workspace")
}

func shortRev(rev string) string {
	if len(rev) > 8 {
		return rev[:8]
	}
	if rev == "" {
		return "-"
	}
	return rev
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}
