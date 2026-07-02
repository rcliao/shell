package topic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ClaudeCLIHaiku is a real HaikuClient that shells out to `claude -p
// --model <model>` for one-shot topic classification. Cycle 66.
//
// Failure modes are designed to be safe — any error returns a HaikuResult
// with empty topic and is_new=false, so the caller falls back to keyword.
// The bridge never blocks the user's actual turn on classifier failure.
type ClaudeCLIHaiku struct {
	Binary  string        // claude CLI path (default "claude")
	Model   string        // e.g. "claude-haiku-4-5"
	Timeout time.Duration // default 10s
}

func NewClaudeCLIHaiku(binary, model string, timeout time.Duration) *ClaudeCLIHaiku {
	if binary == "" {
		binary = "claude"
	}
	if timeout == 0 {
		// Cycle 77: 10s → 8s. Production p95=8.8s — 8s timeout means slow
		// calls fall back to keyword rather than block goroutine longer.
		// Real fix (HTTP API or subprocess pool) deferred to cycle 78+.
		timeout = 8 * time.Second
	}
	return &ClaudeCLIHaiku{Binary: binary, Model: model, Timeout: timeout}
}

// ClassifyTopic builds a prompt with the existing topic registry + the user
// message, invokes Claude CLI in one-shot mode, parses JSON output.
func (c *ClaudeCLIHaiku) ClassifyTopic(ctx context.Context, msg string, existing []Topic) (HaikuResult, error) {
	prompt := buildPrompt(msg, existing)

	cctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, c.Binary,
		"-p", prompt,
		"--model", c.Model,
		"--output-format", "text",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return HaikuResult{}, fmt.Errorf("claude cli: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}

	out := strings.TrimSpace(stdout.String())
	res, err := parseHaikuJSON(out)
	if err != nil {
		return HaikuResult{}, fmt.Errorf("parse haiku output: %w (raw: %q)", err, truncateForErr(out, 200))
	}
	return res, nil
}

// buildPrompt formats the registry list and message into PromptTemplate.
func buildPrompt(msg string, existing []Topic) string {
	var b strings.Builder
	if len(existing) == 0 {
		b.WriteString("(none yet — first topic for this chat)\n")
	} else {
		for _, t := range existing {
			fmt.Fprintf(&b, "- %s: %s\n", t.Name, t.Description)
		}
	}
	return fmt.Sprintf(PromptTemplate, b.String(), msg)
}

// parseHaikuJSON extracts the JSON object from Haiku's output. Tolerant of
// surrounding text, code fences, and quote variations.
func parseHaikuJSON(raw string) (HaikuResult, error) {
	s := strings.TrimSpace(raw)
	// Strip code fences if present.
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	// Find the first { and last } to handle any leading/trailing prose.
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < 0 || end <= start {
		return HaikuResult{}, fmt.Errorf("no JSON object found")
	}
	var r HaikuResult
	if err := json.Unmarshal([]byte(s[start:end+1]), &r); err != nil {
		return HaikuResult{}, err
	}
	if r.Topic == "" {
		return HaikuResult{}, fmt.Errorf("empty topic field")
	}
	return r, nil
}

func truncateForErr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
