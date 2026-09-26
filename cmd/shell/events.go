package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/rcliao/shell/internal/store"
)

// shell events — external events (docs/DESIGN-HEARTBEAT-AGENDA-EVENTS.md).
// list shows them; inject records one by hand, to test the path end to end
// before any producer exists (it appears in the agent's next heartbeat
// agenda). Producers normally drop JSON files into <agent dir>/events/inbox/.
func newEventsCmd() *cobra.Command {
	var configFlag string
	cmd := &cobra.Command{Use: "events", Short: "External events: list, inject (test)"}
	cmd.PersistentFlags().StringVar(&configFlag, "config", "",
		"agent config path (e.g. ~/.shell/agents/<agent>/config.json); default ~/.shell/config.json")

	var all bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List events (open ones by default)",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := store.Open(loadConfigFrom(configFlag).Store.DBPath)
			if err != nil {
				return err
			}
			defer st.Close()
			statuses := []string{store.EventNew, store.EventSeen}
			if all {
				statuses = nil
			}
			evs, err := st.ListEvents(statuses, 100)
			if err != nil {
				return err
			}
			if len(evs) == 0 {
				fmt.Println("no events")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTATUS\tSOURCE/KIND\tWHEN\tSUMMARY\tNOTE")
			for _, e := range evs {
				fmt.Fprintf(tw, "%d\t%s\t%s/%s\t%s\t%s\t%s\n", e.ID, e.Status, e.Source, e.Kind,
					e.OccurredAt.Local().Format("Jan 2 15:04"), clip(e.Summary, 60), clip(e.Note, 40))
			}
			return tw.Flush()
		},
	}
	list.Flags().BoolVar(&all, "all", false, "include done and ignored")

	var source, kind, dedup, ref string
	var chatID int64
	inject := &cobra.Command{
		Use:   "inject <summary>",
		Short: "Record an event by hand (testing: it appears in the next heartbeat agenda)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := store.Open(loadConfigFrom(configFlag).Store.DBPath)
			if err != nil {
				return err
			}
			defer st.Close()
			if dedup == "" {
				dedup = fmt.Sprintf("manual-%d", time.Now().UnixNano())
			}
			id, created, err := st.AddEvent(store.Event{Source: source, Kind: kind, DedupID: dedup,
				Summary: strings.Join(args, " "), Ref: ref, ChatID: chatID, OccurredAt: time.Now()})
			if err != nil {
				return err
			}
			if !created {
				fmt.Println("duplicate: an event with that source and dedup id already exists")
				return nil
			}
			fmt.Printf("event #%d recorded; it appears in the agent's next heartbeat agenda\n", id)
			return nil
		},
	}
	inject.Flags().StringVar(&source, "source", "manual", "producer name")
	inject.Flags().StringVar(&kind, "kind", "note", "event kind, e.g. email.received")
	inject.Flags().StringVar(&dedup, "dedup", "", "dedup id (default: unique)")
	inject.Flags().StringVar(&ref, "ref", "", "reference: an id or URL the agent can fetch")
	inject.Flags().Int64Var(&chatID, "chat", 0, "the chat it concerns (optional)")

	cmd.AddCommand(list, inject)
	return cmd
}
