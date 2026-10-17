package stewardeval

import (
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/store"
)

func TestFormatCheck(t *testing.T) {
	cases := []struct {
		change, lang string
		want         string // comma-joined failures
	}{
		{"要我每週查一次價格，降到 $200 以下就通知你們嗎？", "zh", ""},
		{"I'll compare the three hotels so you can pick one tonight. Want me to?", "en", ""},
		{"Which bed? I suggest the north one, because it gets sun.", "en", ""},
		{"I'll check prices. Then I'll compare them. Then I'll book. OK?", "en", "sentences"},
		{"I'll book the north bed for you.", "en", "question"},
		{"See https://example.com — ok?", "en", "link_or_id"},
		{"Proposal #21 again — ok?", "en", "link_or_id"},
		{"Should I check the price for the Corviknight build?", "zh", "language"},
		{"要我換成 Corviknight 嗎？", "", ""},
		{strings.Repeat("好", 201) + "？", "zh", "length"},
	}
	for _, c := range cases {
		if got := strings.Join(FormatCheck(c.change, c.lang), ","); got != c.want {
			t.Errorf("FormatCheck(%q, %q) = %q, want %q", c.change, c.lang, got, c.want)
		}
	}
}

func TestParseProposal(t *testing.T) {
	p, err := ParseProposal("Here:\n{\"kind\": \"GOAL\", \"title\": \"t\", \"change\": \"c?\", \"evidence\": \"GOAL e\"}")
	if err != nil || p.Kind != "goal" || p.Change != "c?" {
		t.Fatalf("got %+v, %v", p, err)
	}
	if p, err := ParseProposal(`{"kind":"none","reason":"nothing due"}`); err != nil || p.Kind != "none" {
		t.Fatalf("abstention: %+v, %v", p, err)
	}
	for _, bad := range []string{`{"kind":"do"}`, `{"kind":"maybe","change":"x"}`, "no json"} {
		if _, err := ParseProposal(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestFiledProposalKind(t *testing.T) {
	for ev, want := range map[string]string{"DO: research": "do", "DO. Doc rev": "do", "GOAL — weekly": "goal", "ASK stuck": "ask", "Doc rev 3dbaf22": "ask", "": "ask"} {
		if got := FiledProposal(store.Suggestion{Evidence: ev, Change: "c"}).Kind; got != want {
			t.Errorf("evidence %q → %q, want %q", ev, got, want)
		}
	}
}

func TestParseGrade(t *testing.T) {
	full := `{"new":{"pass":false,"why":"asked twice"},"agent_led":{"pass":true,"why":""},"useful":{"pass":true,"why":""},"concrete":{"pass":true,"why":""},"grounded":{"pass":true,"why":""}}`
	g, err := ParseGrade("```json\n" + full + "\n```")
	if err != nil || g["new"].Pass || g["new"].Why != "asked twice" || !g["grounded"].Pass {
		t.Fatalf("got %+v, %v", g, err)
	}
	if _, err := ParseGrade(`{"new":{"pass":true}}`); err == nil {
		t.Error("a grade missing checks parsed")
	}
}

func TestVariantsAndPrompts(t *testing.T) {
	v := Variants("on")
	if v["base"].History || v["base"].MaxChars != 0 || !v["a"].History || v["a"].Capabilities || !v["b"].Capabilities || !v["b"].SelfCheck || v["b"].MaxChars != 0 || v["c"].MaxChars != 200 || !v["c"].SelfCheck {
		t.Fatalf("variants: %+v", v)
	}
	for name, o := range v {
		if !o.IgnoreOpen || o.Mode != "on" {
			t.Errorf("%s: %+v", name, o)
		}
	}
	gen := GenPrompt("Agent", "- garden (project) — Garden\n", "# Garden doc", v["b"])
	for _, want := range []string{"You are Agent", "[Project steward]", "- garden (project)", "# Garden doc", `"kind": "none"`, "At most 1 proposals", "Prefer do and goal"} {
		if !strings.Contains(gen, want) {
			t.Errorf("gen prompt lacks %q", want)
		}
	}
	j := JudgePrompt(JudgeInput{
		Project: store.Project{Slug: "garden", Title: "Garden", Lang: "en"},
		Doc:     "# Garden doc",
		Thread:  []store.ThreadMessage{{Role: "assistant", At: time.Now(), Text: "Which bed?"}},
		Asked:   []store.Suggestion{{CreatedAt: time.Now(), Status: "delivered", Change: "Which bed?\nNorth?"}},
		At:      time.Now(),
		P:       Proposal{Kind: "ask", Change: "Which bed? I suggest north."},
	})
	for _, want := range []string{"- new:", "assistant: Which bed?", "posted to the family, delivered: Which bed? North?", "no thread of its own", "kind: ask", `"grounded"`} {
		if !strings.Contains(j, want) {
			t.Errorf("judge prompt lacks %q", want)
		}
	}
}
