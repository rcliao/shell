package route

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// The judge labels messages for scoring (R0). It is a stronger model than
// any router backend, and it sees what a backend never does: each message in
// its thread's order, so a short follow-up ("and the hotel?") can be read
// against what came before. It is still not the truth; the report says which
// label source a number rests on, and the owner can override any label.

// JudgeItem is one message to label, in thread order.
type JudgeItem struct {
	N    int // position in the batch, 1-based
	At   time.Time
	Text string
}

// JudgeLabel is the judge's answer for one item.
type JudgeLabel struct {
	N    int    `json:"n"`
	Lane string `json:"lane"`
	Sure bool   `json:"sure"`
}

// JudgePrompt builds one batch prompt: the candidate lanes, then the
// messages of ONE thread in order.
func JudgePrompt(cands []Candidate, items []JudgeItem) string {
	var sb strings.Builder
	sb.WriteString(`You are labelling messages from a family chat with an assistant, to score a message router.
For each message, decide which lane it belongs to: one of the projects below, or "general" for
everything that is not about a project (small talk, unrelated questions, other topics).
Read the messages in order: a short follow-up belongs to the lane of what it follows up.
"sure" is false when a reasonable person could file it either way.

Lanes:
- general: anything not about one of the projects below
`)
	for _, c := range cands {
		desc := c.Title
		if c.Desc != "" {
			d := []rune(strings.Join(strings.Fields(c.Desc), " "))
			if len(d) > 200 {
				d = d[:200]
			}
			desc += " — " + string(d)
		}
		fmt.Fprintf(&sb, "- %s: %s\n", c.Lane, desc)
	}
	sb.WriteString("\nMessages (one thread, oldest first):\n")
	for _, it := range items {
		t := []rune(strings.Join(strings.Fields(it.Text), " "))
		if len(t) > 400 {
			t = append(t[:400], '…')
		}
		fmt.Fprintf(&sb, "%d. [%s] %s\n", it.N, it.At.Local().Format("Mon Jan 2 15:04"), string(t))
	}
	sb.WriteString(`
Answer with ONLY a JSON array, one object per message, no prose:
[{"n": 1, "lane": "<lane>", "sure": true}, ...]`)
	return sb.String()
}

// ParseJudge extracts the JSON array from the judge's output and keeps only
// labels whose lane is a real candidate or general.
func ParseJudge(out string, cands []Candidate) ([]JudgeLabel, error) {
	start, end := strings.Index(out, "["), strings.LastIndex(out, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON array in judge output")
	}
	var labels []JudgeLabel
	if err := json.Unmarshal([]byte(out[start:end+1]), &labels); err != nil {
		return nil, fmt.Errorf("judge output: %w", err)
	}
	valid := map[string]bool{General: true}
	for _, c := range cands {
		valid[c.Lane] = true
	}
	kept := labels[:0]
	for _, l := range labels {
		if valid[l.Lane] {
			kept = append(kept, l)
		}
	}
	return kept, nil
}

// ClaudeCLI runs one prompt through `claude -p` and returns its text.
func ClaudeCLI(ctx context.Context, model, prompt string, timeout time.Duration) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "claude", "-p", prompt, "--model", model, "--output-format", "text")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude cli: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
