package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/rcliao/shell/internal/store"
)

// shell suggestions — the owner's side of the suggestion loop (S0,
// docs/DESIGN-ROUTER-AND-SUGGESTIONS.md), usable without Telegram: list what
// an agent asked for, decide it, or run the weekly review now.
func newSuggestionsCmd() *cobra.Command {
	var configFlag string
	var all bool
	cmd := &cobra.Command{
		Use:   "suggestions",
		Short: "List, decide, or trigger an agent's suggestions to its owner",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := store.Open(loadConfigFrom(configFlag).Store.DBPath)
			if err != nil {
				return err
			}
			defer st.Close()
			var statuses []string
			if !all {
				statuses = []string{store.SuggestionProposed, store.SuggestionDelivered, store.SuggestionAccepted}
			}
			list, err := st.ListSuggestions(statuses, 100)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Println("no suggestions")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTATUS\tFILED\tTITLE\tCHANGE\tNOTE")
			for _, s := range list {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Status, s.CreatedAt.Local().Format("Jan 2"),
					clip(s.Title, 50), clip(s.Change, 60), clip(s.Note, 40))
			}
			return tw.Flush()
		},
	}
	cmd.PersistentFlags().StringVar(&configFlag, "config", "",
		"agent config path (e.g. ~/.shell/agents/<agent>/config.json); default ~/.shell/config.json")
	cmd.Flags().BoolVar(&all, "all", false, "include declined, done and withdrawn")

	var note string
	decide := &cobra.Command{
		Use:   "decide <id> <accepted|declined|done>",
		Short: "Record the owner's decision on a suggestion",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("id: %w", err)
			}
			status := strings.ToLower(args[1])
			switch status {
			case "accept":
				status = store.SuggestionAccepted
			case "decline":
				status = store.SuggestionDeclined
			}
			st, err := store.Open(loadConfigFrom(configFlag).Store.DBPath)
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.DecideSuggestion(id, status, "owner (cli)", note); err != nil {
				return err
			}
			fmt.Printf("suggestion #%d marked %s\n", id, status)
			return nil
		},
	}
	decide.Flags().StringVar(&note, "note", "", "the reason, in your words — the agent sees it next review")

	reviewNow := &cobra.Command{
		Use:   "review-now",
		Short: "Run the agent's weekly review on the daemon's next scheduler tick",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := store.Open(loadConfigFrom(configFlag).Store.DBPath)
			if err != nil {
				return err
			}
			defer st.Close()
			sc, err := st.FindScheduleByDedupKey("agent:review")
			if err != nil {
				return err
			}
			if sc == nil || !sc.Enabled {
				return fmt.Errorf("no live review schedule — set agent.owner_chat_id and restart the daemon")
			}
			now := time.Now().UTC()
			if err := st.UpdateScheduleNextRun(sc.ID, now, now); err != nil {
				return err
			}
			fmt.Printf("review schedule #%d will fire on the next scheduler tick; results go to the owner's chat\n", sc.ID)
			return nil
		},
	}
	cmd.AddCommand(decide, reviewNow)
	return cmd
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
