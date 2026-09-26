package discord

import (
	"strings"
	"testing"
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
