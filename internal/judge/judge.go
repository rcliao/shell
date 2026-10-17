// Package judge is the DoneJudge of docs/DESIGN-AGENT-LOOPS.md (step 3): a
// separate model call that rules whether a goal's done criteria are met,
// from the evidence the agent surfaced and nothing else. The agent claims
// done; the judge confirms it. Every failure is "not met".
package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Verdicts.
const (
	Met        = "met"
	NotMet     = "not_met"
	Impossible = "impossible"
)

// Verdict is the judge's ruling and what the call cost.
type Verdict struct {
	Verdict      string
	Reason       string
	CostUSD      float64
	InputTokens  int
	OutputTokens int
}

// Runner runs the judge prompt and returns the CLI's stdout.
type Runner func(ctx context.Context, prompt string) ([]byte, error)

// DefaultTimeout keeps one judge call inside the MCP client's 60 s RPC limit.
const DefaultTimeout = 45 * time.Second

// CLI returns a Runner that calls `claude -p` with no tools at all (Bash,
// Write and Edit named as disallowed too), no MCP servers, JSON output, a
// scratch working directory and the given child environment.
func CLI(model string, env func() []string) Runner {
	return func(ctx context.Context, prompt string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "claude", "-p", prompt, "--model", model,
			"--output-format", "json", "--tools", "", "--disallowedTools", "Bash,Write,Edit",
			"--strict-mcp-config", "--max-turns", "1")
		cmd.Dir = os.TempDir()
		if env != nil {
			cmd.Env = env()
		}
		cmd.WaitDelay = 5 * time.Second
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("claude cli: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
		}
		return stdout.Bytes(), nil
	}
}

// Done rules on a done claim. It never returns met on an error: the error
// comes back with a not-met verdict whose reason says the judge failed.
func Done(ctx context.Context, run Runner, timeout time.Duration, goal, criteria, evidence string) (Verdict, error) {
	if strings.TrimSpace(evidence) == "" {
		return Verdict{Verdict: NotMet, Reason: "no evidence given"}, nil
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := run(cctx, Prompt(goal, criteria, evidence))
	if err != nil {
		return Verdict{Verdict: NotMet, Reason: "judge failed: " + clip(err.Error(), 200)}, err
	}
	v, err := Parse(out)
	if err != nil {
		v.Verdict, v.Reason = NotMet, "judge failed: "+clip(err.Error(), 200)
		return v, err
	}
	return v, nil
}

// Prompt is the judge's whole input: the goal, the criteria and the
// evidence, as text.
func Prompt(goal, criteria, evidence string) string {
	return fmt.Sprintf(`You are a strict judge. An agent says it has reached a goal. Decide from the evidence below ONLY — you cannot look anything up, and anything not shown in the evidence did not happen.

Goal: %s
Done criteria: %s

Evidence the agent gave:
<<<
%s
>>>

Rules:
- "met" only if the evidence shows every part of the done criteria concretely (numbers, names, links, quotes). A claim without its proof is not met.
- "impossible" only if the evidence shows the goal cannot be reached at all (e.g. the thing no longer exists).
- Otherwise "not_met", with the one thing still missing.

Answer with ONLY this JSON, no prose:
{"verdict": "met" | "not_met" | "impossible", "reason": "<one sentence>"}`, goal, criteria, evidence)
}

// cliResult is the result event of `claude -p --output-format json`. The
// CLI prints either that one object or an array of events ending in it.
type cliResult struct {
	Type         string  `json:"type"`
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// Result returns the text and cost of one `claude -p --output-format json`
// call, for callers that parse the text themselves.
func Result(out []byte) (text string, costUSD float64, err error) {
	r, err := envelope(out)
	return r.Result, r.TotalCostUSD, err
}

// envelope reads the CLI's JSON output (one result object, or an event
// array holding one); an errored call is an error with its cost kept.
func envelope(out []byte) (cliResult, error) {
	out = bytes.TrimSpace(out)
	var r cliResult
	if bytes.HasPrefix(out, []byte("[")) {
		var events []cliResult
		if err := json.Unmarshal(out, &events); err != nil {
			return r, fmt.Errorf("unparseable judge output: %w", err)
		}
		found := false
		for _, e := range events {
			if e.Type == "result" {
				r, found = e, true
			}
		}
		if !found {
			return r, fmt.Errorf("judge output has no result event")
		}
	} else if err := json.Unmarshal(out, &r); err != nil {
		return r, fmt.Errorf("unparseable judge output: %w", err)
	}
	if r.IsError {
		return r, fmt.Errorf("judge call errored: %s", clip(r.Result, 200))
	}
	return r, nil
}

// Parse reads the CLI's JSON output and the verdict JSON inside its result.
// Anything else is an error (and so not met). Cost is filled in whenever
// the CLI envelope parsed.
func Parse(out []byte) (Verdict, error) {
	r, err := envelope(out)
	v := Verdict{CostUSD: r.TotalCostUSD, InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens}
	if err != nil {
		return v, err
	}
	start, end := strings.Index(r.Result, "{"), strings.LastIndex(r.Result, "}")
	if start < 0 || end <= start {
		return v, fmt.Errorf("no verdict JSON in %q", clip(r.Result, 120))
	}
	var body struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(r.Result[start:end+1]), &body); err != nil {
		return v, fmt.Errorf("verdict JSON: %w", err)
	}
	switch body.Verdict {
	case Met, NotMet, Impossible:
	default:
		return v, fmt.Errorf("unknown verdict %q", body.Verdict)
	}
	v.Verdict, v.Reason = body.Verdict, strings.TrimSpace(body.Reason)
	return v, nil
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
