package route

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/rcliao/shell/internal/decide"
)

func TestDecideSticky(t *testing.T) {
	cases := []struct {
		prev   string
		c      Choice
		lane   string
		sticky bool
	}{
		{"", Choice{Lane: "japan", Confidence: 0.3}, "japan", false},          // no previous lane
		{"japan", Choice{Lane: "general", Confidence: 0.4}, "japan", true},    // unsure switch stays
		{"japan", Choice{Lane: "general", Confidence: 0.9}, "general", false}, // sure switch moves
		{"japan", Choice{Lane: "japan", Confidence: 0.1}, "japan", false},     // same lane is not sticky
		{"health", Choice{Lane: "", Confidence: 0.9}, "general", false},       // empty = general
	}
	for _, c := range cases {
		lane, sticky := Decide(c.prev, c.c, 0.6)
		if lane != c.lane || sticky != c.sticky {
			t.Errorf("Decide(%q, %+v) = %q,%v want %q,%v", c.prev, c.c, lane, sticky, c.lane, c.sticky)
		}
	}
}

func TestKeywordBaseline(t *testing.T) {
	cands := []Candidate{{Lane: "japan-2027", Title: "日本行程 2027"}, {Lane: "health", Title: "健康紀錄"},
		{Lane: "ai-research-briefing", Title: "AI Research Briefing"}}
	k := Keyword{}
	for msg, want := range map[string]string{
		"日本的行程要先訂哪一天":                         "japan-2027",
		"今天的健康紀錄在這":                           "health",
		"anything new in the briefing today?": "ai-research-briefing",
		"午餐 memo ox bone soup":                General, // no title words: the baseline's known blind spot
		"2027 is far away":                    General, // digits alone never match
	} {
		c, _ := k.Choose(context.Background(), Input{Text: msg, Candidates: cands})
		if c.Lane != want {
			t.Errorf("keyword(%q) = %q, want %q", msg, c.Lane, want)
		}
	}
	if got := keywordTokens("日本行程 2027"); !reflect.DeepEqual(got, []string{"日本", "本行", "行程"}) {
		t.Errorf("tokens = %v", got)
	}
}

type fakeDecider struct{ got map[string]decide.Question }

func (f *fakeDecider) Enabled() bool { return true }
func (f *fakeDecider) Ask(_ context.Context, _ any, q map[string]decide.Question) (decide.Result, error) {
	f.got = q
	return decide.Result{Answers: map[string]decide.Answer{"which_project": {Choice: "none", Confidence: 0.8}}}, nil
}

func TestJevAsksTheShadowQuestion(t *testing.T) {
	f := &fakeDecider{}
	c, err := Jev{D: f}.Choose(context.Background(), Input{ChatKind: "dm", Text: "hi",
		Candidates: []Candidate{{Lane: "health", Title: "Health log"}}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Lane != General || c.Confidence != 0.8 {
		t.Errorf("choice = %+v, want general 0.8 (none maps to general)", c)
	}
	want := decide.WhichProjectQuestion([]decide.Project{{Slug: "health", Title: "Health log"}})
	if !reflect.DeepEqual(f.got["which_project"], want) {
		t.Errorf("replay must ask exactly the live question:\n got %+v\nwant %+v", f.got["which_project"], want)
	}
}

func TestJudgePromptAndParse(t *testing.T) {
	cands := []Candidate{{Lane: "japan", Title: "日本行程", Desc: "spring trip\nplanning"}}
	p := JudgePrompt(cands, []JudgeItem{{N: 1, Text: "機票訂了嗎"}, {N: 2, Text: "那飯店呢"}})
	for _, want := range []string{"- general:", "- japan: 日本行程 — spring trip planning", "1. [", "2. [", "ONLY a JSON array"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	out := "Here you go:\n```json\n[{\"n\":1,\"lane\":\"japan\",\"sure\":true},{\"n\":2,\"lane\":\"hotels\",\"sure\":true},{\"n\":3,\"lane\":\"general\",\"sure\":false}]\n```"
	got, err := ParseJudge(out, cands)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Lane != "japan" || got[1].N != 3 || got[1].Sure {
		t.Errorf("parsed = %+v (an unknown lane must be dropped)", got)
	}
	if _, err := ParseJudge("no array here", cands); err == nil {
		t.Error("output without an array must be an error")
	}
}
