package discord

import (
	"strings"
	"testing"
)

func TestMentionsRender(t *testing.T) {
	m := NewMentions(map[string]string{"alex": "300000000000000002", "sam": "300000000000000001"})
	cases := []struct {
		in, want string
		users    int
	}{
		{"@alex 記得 6 點吃藥", "<@300000000000000002> 記得 6 點吃藥", 1},
		{"@Alex and @sam, dinner?", "<@300000000000000002> and <@300000000000000001>, dinner?", 2},
		{"@alex @alex", "<@300000000000000002> <@300000000000000002>", 1},
		{"mail me at x@alex.com", "mail me at x@alex.com", 0},
		{"no one: alex said hi", "no one: alex said hi", 0},
		{"`@alex` stays code", "`@alex` stays code", 0},
		{"```\n@sam in a block\n``` then @sam", "```\n@sam in a block\n``` then <@300000000000000001>", 1},
		{"@alexko is someone else", "@alexko is someone else", 0},
	}
	for _, c := range cases {
		got, users := m.Render(c.in)
		if got != c.want || len(users) != c.users {
			t.Errorf("Render(%q) = %q (%d users), want %q (%d)", c.in, got, len(users), c.want, c.users)
		}
	}
	var none *Mentions
	if got, users := none.Render("@alex"); got != "@alex" || users != nil {
		t.Fatal("a nil converter must leave text alone")
	}
}

func TestPlatformNote(t *testing.T) {
	note := PlatformNote([]string{"alex", "sam"})
	for _, want := range []string{"Discord", "`@alex`", "`@sam`", "<t:UNIX:R>"} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q", want)
		}
	}
	if strings.Contains(PlatformNote(nil), "@") {
		t.Fatal("with no linked people the note must not offer mentions")
	}
}
