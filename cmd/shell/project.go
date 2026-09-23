package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rcliao/shell/internal/config"
	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/store"
)

// shell project — owner ops for the project registry (P2, docs/
// PLAN-PROJECT-WORKSPACE.md): list/show/archive/bind/adopt. Direct store access,
// same as `shell status` and `shell session`; the agent-facing surface is the
// RPC + skill, and the family user gets natural language only.
func newProjectCmd() *cobra.Command {
	var configFlag string

	projectCmd := &cobra.Command{
		Use:   "project",
		Short: "Manage projects: list, show, archive, bind, adopt",
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

	var adoptTitle, adoptEmoji, adoptLang string
	var adoptReplace bool
	var adoptChat, adoptThread int64
	adoptCmd := &cobra.Command{
		Use:   "adopt <slug> <notion-url-or-page-id>",
		Short: "Watch an existing, human-made Notion page for comments (never rendered)",
		Long: `Bind a project to a Notion page a human made, WATCH-ONLY: comments on the
page reach the agent and get an in-thread reply; the page is never rendered or
reconciled, because it holds blocks (tables, checkboxes) the renderer cannot
express and would erase.

If <slug> does not exist it is created as a registry-only project (no managed
doc, no research schedule); --title and --chat are then required. The page
must be shared with the Notion integration first (page ••• menu → Connections).`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := args[0]
			pageID, err := project.ParseNotionPageID(args[1])
			if err != nil {
				return err
			}
			cfg, st, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()

			// Same token contract as the daemon: the configured secret, handed
			// to the client through the env var it reads. The store is opened
			// here because only the daemon opens it at startup — without this
			// a CLI run sees no secrets at all and reports Notion as
			// unconfigured.
			config.OpenSecretStore(cfg.Secrets)
			tokenName := cfg.Notion.TokenSecret
			if tokenName == "" {
				tokenName = "NOTION_TOKEN"
			}

			p, err := st.GetProjectBySlug(slug)
			if err != nil {
				return err
			}
			if p != nil {
				bm := project.ParseBlockMap(p.BlockMap)
				switch {
				case bm.Rendered() && !bm.Adopted:
					return fmt.Errorf("project %q already renders its own Notion page (%s) — adopting would orphan it; create a new slug for the human page", slug, p.ExportRef)
				case p.ExportRef != "" && p.ExportRef != pageID && !adoptReplace:
					// A hand-bound doc (the agent's only pointer to it), or a
					// first render still in flight whose map is not stored yet.
					// Either way, never overwrite a binding silently.
					return fmt.Errorf("project %q is already bound to %s %q — adopting would replace that binding. Use a new slug, or pass --replace if the old binding is really obsolete", slug, p.ExportKind, p.ExportRef)
				}
				if p.ExportRef != "" && p.ExportRef != pageID {
					fmt.Printf("Replacing previous binding: %s %s\n", p.ExportKind, p.ExportRef)
				}
			} else if adoptTitle == "" || adoptChat == 0 {
				return fmt.Errorf("project %q does not exist: pass --title and --chat to create it", slug)
			}

			// Verify access and read the block list BEFORE writing anything.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			blockMap, n, err := project.AdoptPage(ctx, project.NewNotionClient(func() string { return cfg.Secret(tokenName) }), pageID)
			if err != nil {
				return err
			}

			kind := "notion"
			if p == nil {
				if _, err := st.CreateProject(store.Project{
					Slug: slug, Title: adoptTitle, Emoji: adoptEmoji,
					ChatID: adoptChat, MessageThreadID: adoptThread, Lang: adoptLang,
					ExportKind: kind, ExportRef: pageID, BlockMap: blockMap,
				}); err != nil {
					return err
				}
			} else if err := st.UpdateProjectFields(slug, store.ProjectFieldUpdate{
				ExportKind: &kind, ExportRef: &pageID, BlockMap: &blockMap,
			}); err != nil {
				return err
			}

			got, err := st.GetProjectBySlug(slug)
			if err != nil {
				return err
			}
			if got == nil || got.ExportRef != pageID || !project.ParseBlockMap(got.BlockMap).Adopted {
				return fmt.Errorf("adopt read-back failed for %q", slug)
			}
			fmt.Printf("Project %s adopted page %s (watch-only, %d top-level blocks).\n", slug, pageID, n)
			fmt.Println("Comments are picked up by the next Notion poll tick (every 30 min).")
			return nil
		},
	}
	adoptCmd.Flags().StringVar(&adoptTitle, "title", "", "project title (when creating)")
	adoptCmd.Flags().StringVar(&adoptEmoji, "emoji", "", "project emoji (when creating)")
	adoptCmd.Flags().StringVar(&adoptLang, "lang", "", "language for in-thread replies, e.g. zh-TW (when creating; an existing project keeps its own)")
	adoptCmd.Flags().Int64Var(&adoptChat, "chat", 0, "chat id the project belongs to (when creating)")
	adoptCmd.Flags().BoolVar(&adoptReplace, "replace", false, "replace an existing, different export binding (prints the old one)")
	adoptCmd.Flags().Int64Var(&adoptThread, "thread", 0, "Telegram forum topic id (0 = main chat)")

	projectCmd.AddCommand(listCmd, showCmd, archiveCmd, bindCmd, adoptCmd)
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
