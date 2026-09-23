package telegram

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestToolFamilyHidesRawNames(t *testing.T) {
	cases := map[string]string{
		"Bash": "shell", "WebSearch": "search", "WebFetch": "browse",
		"mcp__ghost__ghost_put": "memory", "Read": "file", "Edit": "file",
		"mcp__shell-bridge__shell_pm": "shell", "SomethingNew": "default",
	}
	for tool, want := range cases {
		if got := toolFamily(tool); got != want {
			t.Errorf("toolFamily(%q) = %q, want %q", tool, got, want)
		}
	}
	msg := newProgressVoice("").toolMessage(1, "mcp__ghost__ghost_put")
	if strings.Contains(msg, "mcp__") || !strings.Contains(msg, "Checking my notes") {
		t.Errorf("raw tool name leaked or family phrase missing: %q", msg)
	}
}

func TestParseProgressPhrasesSanitises(t *testing.T) {
	data := []byte(`{"_note":"seed","thinking":["想一下","  ","two\nlines","` + strings.Repeat("長", 61) + `","查資料中"],
	  "tools":{"search":["翻翻網路"],"bogus":["ignored family"]},"long":[],"very_long":["再等我一下下"]}`)
	ph, problems := parseProgressPhrases(data)
	if got := ph.Thinking; len(got) != 2 || got[0] != "想一下" || got[1] != "查資料中" {
		t.Errorf("thinking = %v, want the two valid phrases", got)
	}
	if len(problems) != 3 {
		t.Errorf("problems = %v, want 3 dropped phrases", problems)
	}
	if ph.Tools["search"][0] != "翻翻網路" || ph.Tools["shell"][0] != "Running a command" {
		t.Errorf("tools = %v: custom search, default shell", ph.Tools)
	}
	if _, ok := ph.Tools["bogus"]; ok {
		t.Error("unknown tool family must not be kept")
	}
	if ph.Long[0] != "Still working (loading a lot of context)" || ph.VeryLong[0] != "再等我一下下" {
		t.Errorf("long/very_long = %v / %v", ph.Long, ph.VeryLong)
	}
	if got, _ := parseProgressPhrases([]byte("{not json")); got.Thinking[0] != "Thinking" {
		t.Error("invalid JSON must yield the defaults")
	}
}

func TestProgressVoiceReloadsAndFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), ProgressPhrasesFile)
	v := newProgressVoice(path)
	if got := v.thinkingMessage(1); !strings.Contains(got, "Thinking") {
		t.Fatalf("missing file must use defaults: %q", got)
	}
	if err := os.WriteFile(path, []byte(`{"thinking":["小小想一下"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	v.nextStat = time.Time{} // skip the 30 s stat throttle
	if got := v.thinkingMessage(1); !strings.Contains(got, "小小想一下") {
		t.Errorf("file not picked up: %q", got)
	}
	os.Remove(path)
	v.nextStat = time.Time{}
	if got := v.thinkingMessage(1); !strings.Contains(got, "Thinking") {
		t.Errorf("removed file must fall back to defaults: %q", got)
	}
	// Long-wait slots still switch on tick.
	if got := v.thinkingMessage(35); !strings.Contains(got, "taking a while") {
		t.Errorf("very_long slot: %q", got)
	}
}
