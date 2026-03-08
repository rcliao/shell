// Package search provides web search via Brave, Tavily, or DuckDuckGo.
package search

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Result represents a single search result.
type Result struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	Age         string `json:"age,omitempty"`
	ExtraText   string `json:"extra_text,omitempty"`
}

// Response holds the full search response.
type Response struct {
	Query    string   `json:"query"`
	Provider string   `json:"provider"`
	Results  []Result `json:"results"`
}

// Options configures a search request.
type Options struct {
	Query     string
	Count     int
	Freshness string // pd=24h, pw=7d, pm=31d, py=1yr
	Country   string
}

// Search runs a web search using the best available provider.
// It checks for BRAVE_SEARCH_API_KEY, then TAVILY_API_KEY, then falls back to ddgr.
func Search(ctx context.Context, braveKey, tavilyKey string, opts Options) (*Response, error) {
	if opts.Count <= 0 {
		opts.Count = 5
	}
	if braveKey != "" {
		return searchBrave(ctx, braveKey, opts)
	}
	if tavilyKey != "" {
		return searchTavily(ctx, tavilyKey, opts)
	}
	return searchDDGR(ctx, opts)
}

// Markdown formats a Response as agent-friendly markdown.
func Markdown(resp *Response) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Search Results for %q\n\n", resp.Query)

	if len(resp.Results) == 0 {
		b.WriteString("No results found.\n")
		return b.String()
	}

	for i, r := range resp.Results {
		fmt.Fprintf(&b, "### %d. %s\n", i+1, r.Title)
		fmt.Fprintf(&b, "**URL:** %s\n", r.URL)
		if r.Age != "" {
			fmt.Fprintf(&b, "**Age:** %s\n", r.Age)
		}
		b.WriteString("\n")
		b.WriteString(r.Description)
		b.WriteString("\n")
		if r.ExtraText != "" {
			b.WriteString("\n")
			b.WriteString(r.ExtraText)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("---\n\n**Sources:**\n")
	for _, r := range resp.Results {
		fmt.Fprintf(&b, "- %s\n", r.URL)
	}

	return b.String()
}

// --- Brave Search API ---

func searchBrave(ctx context.Context, apiKey string, opts Options) (*Response, error) {
	u, _ := url.Parse("https://api.search.brave.com/res/v1/web/search")
	q := u.Query()
	q.Set("q", opts.Query)
	q.Set("count", strconv.Itoa(opts.Count))
	q.Set("extra_snippets", "true")
	if opts.Freshness != "" {
		q.Set("freshness", opts.Freshness)
	}
	if opts.Country != "" {
		q.Set("country", opts.Country)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Subscription-Token", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("brave request: %w", err)
	}
	defer resp.Body.Close()

	var body io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer gr.Close()
		body = gr
	}

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(body)
		return nil, fmt.Errorf("brave API error (status %d): %s", resp.StatusCode, string(data))
	}

	var raw struct {
		Web struct {
			Results []struct {
				Title         string   `json:"title"`
				URL           string   `json:"url"`
				Description   string   `json:"description"`
				Age           string   `json:"age"`
				ExtraSnippets []string `json:"extra_snippets"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decoding brave response: %w", err)
	}

	sr := &Response{Query: opts.Query, Provider: "brave"}
	for _, r := range raw.Web.Results {
		sr.Results = append(sr.Results, Result{
			Title:       r.Title,
			URL:         r.URL,
			Description: r.Description,
			Age:         r.Age,
			ExtraText:   strings.Join(r.ExtraSnippets, "\n"),
		})
	}
	return sr, nil
}

// --- Tavily Search API ---

func searchTavily(ctx context.Context, apiKey string, opts Options) (*Response, error) {
	depth := "basic"
	if opts.Count > 5 {
		depth = "advanced"
	}

	reqBody, err := json.Marshal(map[string]any{
		"api_key":      apiKey,
		"query":        opts.Query,
		"search_depth": depth,
		"max_results":  opts.Count,
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling tavily request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", strings.NewReader(string(reqBody)))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tavily request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("tavily API error (status %d): %s", resp.StatusCode, string(data))
	}

	var raw struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decoding tavily response: %w", err)
	}

	sr := &Response{Query: opts.Query, Provider: "tavily"}
	for _, r := range raw.Results {
		sr.Results = append(sr.Results, Result{
			Title:       r.Title,
			URL:         r.URL,
			Description: r.Content,
		})
	}
	return sr, nil
}

// --- DuckDuckGo via ddgr CLI ---

func searchDDGR(ctx context.Context, opts Options) (*Response, error) {
	args := []string{"--json", "-n", strconv.Itoa(opts.Count)}
	if opts.Freshness != "" {
		f := opts.Freshness
		switch f {
		case "pd":
			f = "d"
		case "pw":
			f = "w"
		case "pm":
			f = "m"
		case "py":
			f = "y"
		}
		args = append(args, "-t", f)
	}
	args = append(args, opts.Query)

	cmd := execCommand(ctx, "ddgr", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ddgr failed: %w", err)
	}

	var raw []struct {
		Title    string `json:"title"`
		URL      string `json:"url"`
		Abstract string `json:"abstract"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parsing ddgr output: %w", err)
	}

	sr := &Response{Query: opts.Query, Provider: "ddgr"}
	for _, r := range raw {
		sr.Results = append(sr.Results, Result{
			Title:       r.Title,
			URL:         r.URL,
			Description: r.Abstract,
		})
	}
	return sr, nil
}
