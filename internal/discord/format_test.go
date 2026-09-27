package discord

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSplitShortIsOneChunk(t *testing.T) {
	if got := splitMessage("hi", 2000); len(got) != 1 || got[0] != "hi" {
		t.Fatalf("got %q", got)
	}
}

func TestSplitRespectsLimitAndKeepsText(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 300; i++ {
		b.WriteString("這是一段很長的中文段落，用來測試切割。 ")
		if i%10 == 9 {
			b.WriteString("\n\n")
		}
	}
	text := b.String()
	chunks := splitMessage(text, 2000)
	if len(chunks) < 2 {
		t.Fatalf("want several chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c); n > 2000 {
			t.Fatalf("chunk %d has %d chars", i, n)
		}
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d split a character", i)
		}
	}
	if strings.Join(chunks, "") != text {
		t.Fatal("chunks do not reassemble to the original text")
	}
}

func TestSplitInsideCodeBlockReopensFence(t *testing.T) {
	code := strings.Repeat("fmt.Println(\"line\")\n", 200) // ~4000 chars
	text := "Here is the code:\n\n```go\n" + code + "```\nDone."
	chunks := splitMessage(text, 2000)
	if len(chunks) < 2 {
		t.Fatalf("want several chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if utf8.RuneCountInString(c) > 2000 {
			t.Fatalf("chunk %d too long", i)
		}
		if _, open := openFence(c); open {
			t.Fatalf("chunk %d leaves a code fence open:\n%s", i, c[len(c)-40:])
		}
	}
	if !strings.HasPrefix(chunks[1], "```go\n") {
		t.Fatalf("second chunk should reopen the go fence, starts %q", chunks[1][:12])
	}
	if !strings.HasSuffix(chunks[len(chunks)-1], "Done.") {
		t.Fatal("text after the code block was lost")
	}
}

func TestOpenFence(t *testing.T) {
	cases := map[string]bool{
		"```py\nx":           true,
		"```py\nx\n```":      false,
		"inline ```x``` ok":  false,
		"a\n```\nb\n```\n```": true,
	}
	for in, want := range cases {
		if _, got := openFence(in); got != want {
			t.Errorf("openFence(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestStreamViewShowsTail(t *testing.T) {
	text := strings.Repeat("a", 2500) + "END"
	got := streamView(text, 2000)
	if n := utf8.RuneCountInString(got); n != 2000 {
		t.Fatalf("got %d chars", n)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "END") {
		t.Fatal("want an ellipsis and the latest text")
	}
	if streamView("short", 2000) != "short" {
		t.Fatal("short text must pass through")
	}
}

// Regression: a long first line after ``` used to become the "language" and
// be re-prepended to every chunk, starving the budget until the loop never
// advanced (hung) or sent ~100 messages.
func TestSplitLongFenceLineTerminates(t *testing.T) {
	inputs := []string{
		"```" + strings.Repeat("x", 2100) + "\nmore\n```\n",
		"```" + strings.Repeat("x", 5000) + "\nmore\n```\n",
		"intro\n```" + strings.Repeat("x", 2500),
		"```" + strings.Repeat("y", 1960) + "\n" + strings.Repeat("z\n", 200) + "```",
	}
	for i, in := range inputs {
		done := make(chan []string, 1)
		go func() { done <- splitMessage(in, 2000) }()
		select {
		case chunks := <-done:
			total := utf8.RuneCountInString(in)
			if max := total/1000 + 3; len(chunks) > max {
				t.Errorf("input %d (%d chars): %d chunks, want ≤ %d", i, total, len(chunks), max)
			}
			for _, c := range chunks {
				if utf8.RuneCountInString(c) > 2000 {
					t.Errorf("input %d: chunk over the limit", i)
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("input %d: splitMessage did not terminate", i)
		}
	}
}

func TestFenceTables(t *testing.T) {
	in := "Prices:\n| item | cost |\n|---|---|\n| milk | 3 |\nDone."
	want := "Prices:\n```\n| item | cost |\n|---|---|\n| milk | 3 |\n```\nDone."
	if got := fenceTables(in); got != want {
		t.Fatalf("got\n%s", got)
	}
	inFence := "```\n| a | b |\n```"
	if got := fenceTables(inFence); got != inFence {
		t.Fatal("a table already in a code block must be left alone")
	}
	if got := fenceTables("| x | y |"); got != "```\n| x | y |\n```" {
		t.Fatalf("table at end: %q", got)
	}
	if got := fenceTables("a|b is not a table"); got != "a|b is not a table" {
		t.Fatal("prose with a pipe is not a table")
	}
}
