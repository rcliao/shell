// Package route decides which lane a message belongs to: one of the agent's
// projects, or general (docs/DESIGN-ROUTER-AND-SUGGESTIONS.md, R0). A lane
// will pick a message's session, project context and model; in R0 the
// decision is only logged and scored, never acted on.
package route

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rcliao/shell/internal/decide"
)

// General is the lane of everything that is not about a project.
const General = "general"

// Candidate is one lane a message can go to.
type Candidate struct {
	Lane, Title, Desc string
}

// Input is what a backend sees: the message and where it was said. Never
// the transcript.
type Input struct {
	ChatKind   string // "dm" | "group"
	ThreadID   int64
	Text       string
	Candidates []Candidate
}

// Choice is a backend's answer before the sticky rule.
type Choice struct {
	Lane       string
	Confidence float64
	Latency    time.Duration
}

// Backend picks a lane.
type Backend interface {
	Name() string
	Choose(ctx context.Context, in Input) (Choice, error)
}

// Decide applies the sticky rule: a low-confidence switch away from the
// previous lane of the same thread stays in the previous lane. Chat moves in
// runs; flipping on every uncertain message would fragment the context.
func Decide(prev string, c Choice, threshold float64) (lane string, sticky bool) {
	lane = c.Lane
	if lane == "" {
		lane = General
	}
	if prev != "" && lane != prev && c.Confidence < threshold {
		return prev, true
	}
	return lane, false
}

// FromProjects turns the shadow's project list into candidates.
func FromProjects(ps []decide.Project) []Candidate {
	out := make([]Candidate, 0, len(ps))
	for _, p := range ps {
		out = append(out, Candidate{Lane: p.Slug, Title: p.Title, Desc: p.Instructions})
	}
	return out
}

// Keyword is a dumb local baseline: a project whose title or slug words
// appear in the message wins. It exists so the report can show what the
// hosted router adds over string matching.
type Keyword struct{}

func (Keyword) Name() string { return "keyword" }

func (Keyword) Choose(_ context.Context, in Input) (Choice, error) {
	text := strings.ToLower(in.Text)
	best, bestHits := General, 0
	for _, c := range in.Candidates {
		hits := 0
		for _, tok := range keywordTokens(c.Title + " " + strings.ReplaceAll(c.Lane, "-", " ")) {
			if strings.Contains(text, tok) {
				hits++
			}
		}
		if hits > bestHits {
			best, bestHits = c.Lane, hits
		}
	}
	if bestHits == 0 {
		return Choice{Lane: General, Confidence: 0.5}, nil
	}
	conf := 0.6 + 0.1*float64(bestHits)
	if conf > 0.95 {
		conf = 0.95
	}
	return Choice{Lane: best, Confidence: conf}, nil
}

// keywordTokens: lower-case ASCII words of 4+ letters (minus stopwords), and
// every two-rune window of CJK runs ("日本行程" → 日本, 本行, 行程). Digits
// never count on their own: "2027" would match any date.
func keywordTokens(s string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(t string) {
		if t != "" && !seen[t] && !stopwords[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	var word []rune
	var cjk []rune
	flush := func() {
		if len(word) >= 4 {
			add(strings.ToLower(string(word)))
		}
		word = word[:0]
		for i := 0; i+1 < len(cjk); i++ {
			add(string(cjk[i : i+2]))
		}
		cjk = cjk[:0]
	}
	for _, r := range s {
		switch {
		case r < utf8.RuneSelf && unicode.IsLetter(r):
			if len(cjk) > 0 {
				flush()
			}
			word = append(word, r)
		case r >= utf8.RuneSelf && unicode.IsLetter(r):
			if len(word) > 0 {
				flush()
			}
			cjk = append(cjk, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

var stopwords = map[string]bool{"with": true, "from": true, "this": true, "that": true, "project": true, "search": true}

// Jev asks the hosted typed-decision model the shadow's which_project
// question. Live routing does not call it: the shadow already asked, and
// its answer is handed over (decide.Shadow.OnWhichProject). Replay does.
type Jev struct {
	D decide.Decider
}

func (Jev) Name() string { return "jev" }

func (j Jev) Choose(ctx context.Context, in Input) (Choice, error) {
	projects := make([]decide.Project, 0, len(in.Candidates))
	for _, c := range in.Candidates {
		projects = append(projects, decide.Project{Slug: c.Lane, Title: c.Title, Instructions: c.Desc})
	}
	started := time.Now()
	res, err := j.D.Ask(ctx, decide.ShadowState(in.ChatKind, in.ThreadID, in.Text),
		map[string]decide.Question{"which_project": decide.WhichProjectQuestion(projects)})
	if err != nil {
		return Choice{}, err
	}
	a := res.Answers["which_project"]
	return Choice{Lane: LaneFromJev(a.Choice), Confidence: a.Confidence, Latency: time.Since(started)}, nil
}

// LaneFromJev maps Jev's "none" to the general lane.
func LaneFromJev(choice string) string {
	if choice == "" || choice == "none" {
		return General
	}
	return choice
}
