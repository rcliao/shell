package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	memory "github.com/rcliao/ghost"
)

// migrate-meal-aliases prepends a "Date aliases" header to every meal-memo
// memory in agent:pikamini so date-conditional CJK questions can match via
// FTS5 / vector search.
//
// Example aliases line:
//   [aliases: 2026-05-11 5/11 5月11日 5月11號 週一 Monday May 11 May 11th]
//
// This is the meal-aliases ship targeting RF.contains meal-* from 0.00 → ≥0.50.
// LLM-free, idempotent — re-running detects existing aliases line and skips.
func runMigrateMealAliases(args []string) {
	fs := flag.NewFlagSet("migrate-meal-aliases", flag.ExitOnError)
	dbPath := fs.String("db", "/Users/pikamini/.shell/agents/pikamini/memory.db", "")
	ns := fs.String("ns", "agent:pikamini", "")
	dryRun := fs.Bool("dry-run", false, "print planned changes without writing")
	fs.Parse(args)

	store, err := memory.NewSQLiteStore(*dbPath)
	if err != nil {
		die(fmt.Errorf("open store: %w", err))
	}

	ctx := context.Background()
	mems, err := store.List(ctx, memory.ListParams{
		NS:    *ns,
		Limit: 1000,
	})
	if err != nil {
		die(fmt.Errorf("list: %w", err))
	}

	keyRe := regexp.MustCompile(`^meal-memo-(20\d\d)-(\d\d)-(\d\d)`)
	updated, skipped, errored := 0, 0, 0

	for _, m := range mems {
		mm := keyRe.FindStringSubmatch(m.Key)
		if mm == nil {
			continue
		}
		if strings.HasPrefix(m.Content, "[aliases:") {
			skipped++
			continue
		}
		date, err := time.Parse("2006-01-02", mm[1]+"-"+mm[2]+"-"+mm[3])
		if err != nil {
			errored++
			continue
		}
		header := buildAliasHeader(date)
		newContent := header + "\n" + m.Content

		if *dryRun {
			fmt.Printf("[dry-run] %s\n  +%s\n", m.Key, header)
			updated++
			continue
		}

		priority := m.Priority
		if priority == "" {
			priority = "normal"
		}
		_, err = store.Put(ctx, memory.PutParams{
			NS:         m.NS,
			Key:        m.Key,
			Content:    newContent,
			Kind:       m.Kind,
			Tags:       m.Tags,
			Priority:   priority,
			Importance: m.Importance,
			Tier:       m.Tier,
			Pinned:     m.Pinned,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "put %s failed: %v\n", m.Key, err)
			errored++
			continue
		}
		updated++
	}
	fmt.Printf("migrate-meal-aliases: %d updated, %d already-aliased, %d errored (ns=%s)\n",
		updated, skipped, errored, *ns)
}

var weekdayCN = []string{"週日", "週一", "週二", "週三", "週四", "週五", "週六"}

func buildAliasHeader(d time.Time) string {
	m := int(d.Month())
	day := d.Day()
	parts := []string{
		d.Format("2006-01-02"),
		fmt.Sprintf("%d/%d", m, day),
		fmt.Sprintf("%d月%d日", m, day),
		fmt.Sprintf("%d月%d號", m, day),
		weekdayCN[d.Weekday()],
		d.Format("Monday"),
		d.Format("January 2"),
		d.Format("Jan 2"),
	}
	return "[aliases: " + strings.Join(parts, " | ") + "]"
}
