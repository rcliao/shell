package project

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The project context eval (docs/DESIGN-PROJECT-AREAS.md, part 4): does an
// agent's reply in a project's post use what the project's doc knows? An
// independent judge compares one exchange with the doc as it stood then.

// ContextVerdicts, in the order the report prints them.
var ContextVerdicts = []string{"used", "ignored", "contradicted", "reasked", "na"}

// ContextExcerpt is the part of a doc the judge sees: goals, decisions, and
// open questions, bounded.
func ContextExcerpt(doc string) string {
	var b strings.Builder
	for _, h := range []string{"目標", "決定", ToDecideSection} {
		var bodies []string
		for _, sec := range sectionsNamed(doc, h) {
			if body := strings.TrimSpace(strings.Join(sec.Body, "\n")); body != "" {
				bodies = append(bodies, body)
			}
		}
		if len(bodies) == 0 {
			continue
		}
		body := strings.Join(bodies, "\n")
		if r := []rune(body); len(r) > 1500 {
			body = string(r[:1500]) + "…"
		}
		fmt.Fprintf(&b, "## %s\n%s\n\n", h, body)
	}
	return strings.TrimSpace(b.String())
}

// ContextJudgePrompt asks for one verdict on one exchange.
func ContextJudgePrompt(title, excerpt, user, reply string) string {
	clip := func(s string, n int) string {
		if r := []rune(s); len(r) > n {
			return string(r[:n]) + "…"
		}
		return s
	}
	return fmt.Sprintf(`You are grading whether an assistant used its project notes.

Project: %s
The project's notes at that moment (goals, decisions, open questions):
---
%s
---

A family member wrote:
---
%s
---

The assistant replied:
---
%s
---

Pick ONE verdict:
- "used": the reply builds on the notes where they were relevant (a decision, a constraint, an open question).
- "ignored": the notes had something directly relevant that the reply should have used, and it did not.
- "contradicted": the reply states something the notes' decisions contradict.
- "reasked": the reply asks the family something the notes already settle.
- "na": the notes had nothing relevant to this message (small talk, an unrelated question).

Answer with JSON only: {"verdict": "...", "why": "<one short sentence>"}`,
		title, excerpt, clip(user, 1500), clip(reply, 2500))
}

// ParseContextVerdict reads the judge's JSON answer.
func ParseContextVerdict(out string) (verdict, why string, err error) {
	i, j := strings.Index(out, "{"), strings.LastIndex(out, "}")
	if i < 0 || j <= i {
		return "", "", fmt.Errorf("no JSON in judge output")
	}
	var v struct {
		Verdict string `json:"verdict"`
		Why     string `json:"why"`
	}
	if err := json.Unmarshal([]byte(out[i:j+1]), &v); err != nil {
		return "", "", err
	}
	for _, ok := range ContextVerdicts {
		if v.Verdict == ok {
			return v.Verdict, v.Why, nil
		}
	}
	return "", "", fmt.Errorf("unknown verdict %q", v.Verdict)
}
