package bridge

import "testing"

func TestDetectFableKeyword(t *testing.T) {
	cases := []struct {
		msg          string
		wantFable    bool
		wantStripped string
	}{
		// trailing keyword (owner usage) — triggers, keyword removed
		{"這株巴西木葉子黃了 fable", true, "這株巴西木葉子黃了"},
		{"how do I fix this fable", true, "how do I fix this"},
		{"fable what's the best watering schedule", true, "what's the best watering schedule"},
		{"FABLE compare these two laptops", true, "compare these two laptops"},
		// keyword butted against CJK (no space) still triggers via \b
		{"葉子黃了fable", true, "葉子黃了"},
		// no keyword
		{"這株巴西木葉子黃了", false, "這株巴西木葉子黃了"},
		{"how do I fix this", false, "how do I fix this"},
		// substring must NOT trigger
		{"I love fables and stories", false, "I love fables and stories"},
		{"the fabled sword", false, "the fabled sword"},
		// bare keyword only → left alone (no content to ask)
		{"fable", false, "fable"},
		{"  fable  ", false, "  fable  "},
	}
	for _, c := range cases {
		f, s := detectFableKeyword(c.msg)
		if f != c.wantFable {
			t.Errorf("detectFableKeyword(%q) fable=%v, want %v", c.msg, f, c.wantFable)
		}
		if f && s != c.wantStripped {
			t.Errorf("detectFableKeyword(%q) stripped=%q, want %q", c.msg, s, c.wantStripped)
		}
	}
}
