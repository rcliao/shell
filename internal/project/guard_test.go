package project

import (
	"strings"
	"testing"
)

func TestCheckDocShape(t *testing.T) {
	prev := "# Trip\n\n" + strings.Repeat("## 決定\n\n- a decision\n\n", 100)
	ok := []struct {
		next    string
		confirm bool
	}{
		{prev + "- one more\n", false},
		{prev[:len(prev)/2], false}, // a budget trim
		{"# Trip\n\nshort\n", true}, // a confirmed rewrite
		{"{not json but prose\n", true},
	}
	for i, c := range ok {
		if err := CheckDocShape(prev, c.next, c.confirm); err != nil {
			t.Errorf("ok %d refused: %v", i, err)
		}
	}
	bad := []string{
		`{"content":"# Trip\n\nx"}`,
		`{"content":"{\"content\":\"# 日本行程`,
		"rev 520f6da (projects/trip/doc.md)\n# Trip\n",
		"# Trip\n\nshort\n",
	}
	for i, next := range bad {
		if err := CheckDocShape(prev, next, false); err == nil {
			t.Errorf("bad %d accepted", i)
		}
	}
}
