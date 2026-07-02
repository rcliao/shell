package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	memory "github.com/rcliao/ghost"
)

// proposals reads agent-authored backlog proposals from each per-agent ghost DB
// (ns=loop:proposals) and emits a compact list for the loop driver.
//
// Agents emit proposals via the `propose-backlog` skill; this is the read side.
func runProposals(args []string) {
	fs := flag.NewFlagSet("proposals", flag.ExitOnError)
	dbs := fs.String("dbs",
		"/Users/pikamini/.shell/agents/pikamini/memory.db,/Users/pikamini/.shell/agents/umbreonmini/memory.db",
		"comma-separated agent memory.db paths to scan")
	status := fs.String("status", "open", "filter by status (open|shipped|rejected|deferred|all)")
	fs.Parse(args)

	type row struct {
		Agent    string
		Key      string
		Content  string
		Created  string
	}
	var all []row

	for _, db := range strings.Split(*dbs, ",") {
		db = strings.TrimSpace(db)
		if db == "" {
			continue
		}
		store, err := memory.NewSQLiteStore(db)
		if err != nil {
			fmt.Fprintf(fs.Output(), "skip %s: %v\n", db, err)
			continue
		}
		ms, err := store.List(context.Background(), memory.ListParams{
			NS:    "loop:proposals",
			Limit: 200,
		})
		if err != nil {
			fmt.Fprintf(fs.Output(), "list %s: %v\n", db, err)
			continue
		}
		agent := guessAgentFromPath(db)
		for _, m := range ms {
			if *status != "all" && !strings.Contains(m.Content, "status: "+*status) {
				continue
			}
			all = append(all, row{Agent: agent, Key: m.Key, Content: m.Content, Created: m.CreatedAt.Format("2006-01-02 15:04")})
		}
	}

	sort.Slice(all, func(i, j int) bool { return all[i].Created < all[j].Created })

	if len(all) == 0 {
		fmt.Println("(no proposals)")
		return
	}
	for _, r := range all {
		fmt.Printf("─── %s @ %s ─── %s\n", r.Agent, r.Created, r.Key)
		fmt.Println(r.Content)
		fmt.Println()
	}
	fmt.Printf("total: %d proposal(s) status=%s\n", len(all), *status)
}

func guessAgentFromPath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		if part == "agents" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return "unknown"
}
