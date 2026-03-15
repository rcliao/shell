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
	flag.Parse()

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
		fmt.Fprintf(os.Stderr, "search failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Print(search.Markdown(resp))
}
