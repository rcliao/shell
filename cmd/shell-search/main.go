// shell-search is a standalone CLI for web search, used as a skill script.
// It reuses the internal/search package and reads API keys from env vars.
//
// Usage: shell-search [-n count] [-f freshness] <query...>
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/search"
)

func main() {
	count := flag.Int("n", 5, "number of results")
	freshness := flag.String("f", "", "freshness filter: pd (24h), pw (7d), pm (31d), py (1yr)")
	// The skill documents `web-search "<query>" -n 6`, flags AFTER the query,
	// but the flag package stops at the first positional argument — so "-n 6"
	// used to be searched as part of the query text. Parse flags anywhere.
	flag.CommandLine.Parse(reorderFlags(os.Args[1:], map[string]bool{"n": true, "f": true}))

	query := strings.Join(flag.Args(), " ")
	if query == "" {
		fmt.Fprintln(os.Stderr, "usage: shell-search [-n count] [-f freshness] <query...>")
		os.Exit(1)
	}

	braveKey := os.Getenv("BRAVE_SEARCH_API_KEY")
	tavilyKey := os.Getenv("TAVILY_API_KEY")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := search.Search(ctx, braveKey, tavilyKey, search.Options{
		Query:     query,
		Count:     *count,
		Freshness: *freshness,
	})
	if err != nil {
		// Loud and actionable: this is "the search did not run", not "the
		// web has nothing". The built-in WebSearch tool needs no key.
		fmt.Printf("SEARCH UNAVAILABLE — %v\nUse the built-in WebSearch tool for this query instead. Do not treat this as \"no results\".\n", err)
		fmt.Fprintf(os.Stderr, "search failed: %v\n", err)
		os.Exit(2)
	}

	fmt.Print(search.Markdown(resp))
}

// reorderFlags moves known flags (and their values) ahead of positional
// arguments so they parse wherever they appear. valued names flags that take
// a value. A "--" stops reordering.
func reorderFlags(args []string, valued map[string]bool) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}
		name := strings.TrimLeft(a, "-")
		if strings.HasPrefix(a, "-") && len(a) > 1 && !strings.Contains(name, "=") && valued[name] && i+1 < len(args) {
			flags = append(flags, a, args[i+1])
			i++
			continue
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 && strings.Contains(name, "=") && valued[strings.SplitN(name, "=", 2)[0]] {
			flags = append(flags, a)
			continue
		}
		rest = append(rest, a)
	}
	// "--" so a query word that starts with "-" ("-5 weather") is text,
	// not an unknown flag.
	return append(append(flags, "--"), rest...)
}
