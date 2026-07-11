package bridge

import "testing"

func TestNoopMarkerBlanksTurn(t *testing.T) {
	cases := []struct {
		in   string
		noop bool
	}{
		{"[noop]", true},
		{"This is directed at the other agent, staying quiet [noop]", true},
		{"好的，我來處理 [NOOP]", true},
		{"a normal reply with no marker", false},
		{"", false},
	}
	for _, c := range cases {
		if got := noopMarkerRe.MatchString(c.in); got != c.noop {
			t.Errorf("noopMarkerRe.MatchString(%q) = %v, want %v", c.in, got, c.noop)
		}
	}
}
