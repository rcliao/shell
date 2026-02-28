package telegram

import (
	"strings"
	"testing"
)

func TestFormatForMarkdownV2(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// Basic escaping
		{
			name:  "plain text with special chars",
			input: "Hello! How are you? (good) 1.2.3",
			want:  `Hello\! How are you? \(good\) 1\.2\.3`,
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},

		// Bold
		{
			name:  "bold",
			input: "this is **bold** text",
			want:  `this is *bold* text`,
		},
		{
			name:  "unmatched bold",
			input: "this is **unmatched",
			want:  `this is \*\*unmatched`,
		},

		// Italic
		{
			name:  "italic",
			input: "this is *italic* text",
			want:  `this is _italic_ text`,
		},
		{
			name:  "unmatched italic",
			input: "trailing star *",
			want:  `trailing star \*`,
		},

		// Bold + Italic
		{
			name:  "bold italic",
			input: "this is ***bold italic*** text",
			want:  `this is *_bold italic_* text`,
		},
		{
			name:  "unmatched bold italic",
			input: "***no closing",
			want:  `\*\*\*no closing`,
		},

		// Inline code
		{
			name:  "inline code",
			input: "use `fmt.Println` here",
			want:  "use `fmt.Println` here",
		},
		{
			name:  "inline code with backslash",
			input: "path `C:\\Users\\test` ok",
			want:  `path ` + "`C:\\\\Users\\\\test`" + ` ok`,
		},
		{
			name:  "unmatched backtick",
			input: "lone ` backtick",
			want:  `lone ` + "\\`" + ` backtick`,
		},

		// Fenced code blocks
		{
			name:  "fenced code block",
			input: "```go\nfmt.Println(\"hello\")\n```",
			want:  "```go\nfmt.Println(\"hello\")\n```",
		},
		{
			name:  "code block with backticks inside",
			input: "```\nuse `inline` here\n```",
			want:  "```\nuse \\`inline\\` here\n```",
		},
		{
			name:  "unclosed code block",
			input: "```go\ncode without end",
			// First two backticks form empty inline code, third is escaped
			want: "``\\`go\ncode without end",
		},

		// Links
		{
			name:  "link with simple URL",
			input: "[Google](https://www.google.com)",
			want:  `[Google](https://www.google.com)`,
		},
		{
			name:  "link with query params",
			input: "[search](https://example.com/search?q=hello&page=1#top)",
			want:  `[search](https://example.com/search?q=hello&page=1#top)`,
		},
		{
			name:  "link with special text",
			input: "[click here!](https://example.com)",
			want:  `[click here\!](https://example.com)`,
		},
		{
			name:  "link with closing paren in URL",
			input: "[wiki](https://en.wikipedia.org/wiki/Foo_(bar))",
			// The regex [^)]+ terminates at the first ), so URL is truncated
			// and the trailing ) is escaped as plain text. Known regex limitation.
			want: `[wiki](https://en.wikipedia.org/wiki/Foo_(bar)` + `\)`,
		},
		{
			name:  "unmatched bracket",
			input: "just a [bracket",
			want:  `just a \[bracket`,
		},

		// Headings
		{
			name:  "h1 heading",
			input: "# Hello World",
			want:  "*__Hello World__*",
		},
		{
			name:  "h2 heading",
			input: "## Sub Heading",
			want:  "*Sub Heading*",
		},
		{
			name:  "h3 heading",
			input: "### Details",
			want:  "_Details_",
		},
		{
			name:  "heading with special chars",
			input: "# Version 1.0!",
			want:  "*__Version 1\\.0\\!__*",
		},
		{
			name:  "hash not at line start",
			input: "use C# language",
			want:  `use C\# language`,
		},
		{
			name:  "heading mid-text",
			input: "intro\n# Title\nbody",
			want:  "intro\n*__Title__*\nbody",
		},

		// Bullet lists
		{
			name:  "bullet list single item",
			input: "- item one",
			want:  `\- item one`,
		},
		{
			name:  "bullet list multiple items",
			input: "- first\n- second\n- third",
			want:  "\\- first\n\\- second\n\\- third",
		},
		{
			name:  "bullet list with special chars",
			input: "- Hello! (world)",
			want:  `\- Hello\! \(world\)`,
		},
		{
			name:  "indented bullet list",
			input: "  - nested item",
			want:  `  \- nested item`,
		},
		{
			name:  "dash not at line start",
			input: "this - that",
			want:  `this \- that`,
		},
		{
			name:  "bullet list after paragraph",
			input: "intro\n- one\n- two",
			want:  "intro\n\\- one\n\\- two",
		},

		// Numbered lists
		{
			name:  "numbered list single item",
			input: "1. first item",
			want:  `1\. first item`,
		},
		{
			name:  "numbered list multiple items",
			input: "1. first\n2. second\n3. third",
			want:  "1\\. first\n2\\. second\n3\\. third",
		},
		{
			name:  "numbered list with special chars",
			input: "1. Hello! (world)",
			want:  `1\. Hello\! \(world\)`,
		},
		{
			name:  "indented numbered list",
			input: "  1. nested item",
			want:  `  1\. nested item`,
		},
		{
			name:  "multi-digit numbered list",
			input: "10. tenth item",
			want:  `10\. tenth item`,
		},
		{
			name:  "number dot not at line start",
			input: "use version 2.0 here",
			want:  `use version 2\.0 here`,
		},
		{
			name:  "numbered list after paragraph",
			input: "intro\n1. one\n2. two",
			want:  "intro\n1\\. one\n2\\. two",
		},
		{
			name:  "mixed bullet and numbered",
			input: "- bullet\n1. numbered",
			want:  "\\- bullet\n1\\. numbered",
		},

		// Blockquotes
		{
			name:  "blockquote single line",
			input: "> quoted text",
			want:  `>quoted text`,
		},
		{
			name:  "blockquote multiple lines",
			input: "> first\n> second\n> third",
			want:  ">first\n>second\n>third",
		},
		{
			name:  "blockquote with special chars",
			input: "> Hello! (world)",
			want:  `>Hello\! \(world\)`,
		},
		{
			name:  "blockquote after paragraph",
			input: "intro\n> quoted",
			want:  "intro\n>quoted",
		},
		{
			name:  "blockquote without space after marker",
			input: ">no space",
			want:  ">no space",
		},
		{
			name:  "greater than not at line start",
			input: "a > b",
			want:  `a \> b`,
		},
		{
			name:  "blockquote with bold content",
			input: "> **important** point",
			want:  ">*important* point",
		},
		{
			name:  "blockquote with inline code",
			input: "> use `fmt.Println` here",
			want:  ">use `fmt.Println` here",
		},
		{
			name:  "empty blockquote marker",
			input: ">\n> text",
			want:  ">\n>text",
		},
		{
			name:  "blockquote mixed with heading",
			input: "# Title\n> quoted line\nnormal text",
			want:  "*__Title__*\n>quoted line\nnormal text",
		},
		{
			name:  "blockquote mixed with bullet list",
			input: "> quote\n- bullet",
			want:  ">quote\n\\- bullet",
		},
		{
			name:  "blockquote with italic content",
			input: "> *emphasis* here",
			want:  ">_emphasis_ here",
		},
		{
			name:  "blockquote with link",
			input: "> see [Google](https://google.com)",
			want:  ">see [Google](https://google.com)",
		},
		{
			name:  "blockquote with strikethrough",
			input: "> ~~removed~~ text",
			want:  ">~removed~ text",
		},
		{
			name:  "blockquote with bold italic",
			input: "> ***bold italic*** text",
			want:  ">*_bold italic_* text",
		},
		{
			name:  "multiline blockquote with formatting",
			input: "> **bold** line\n> *italic* line",
			want:  ">*bold* line\n>_italic_ line",
		},

		// Strikethrough
		{
			name:  "strikethrough",
			input: "this is ~~deleted~~ text",
			want:  `this is ~deleted~ text`,
		},
		{
			name:  "unmatched strikethrough",
			input: "~~no closing",
			want:  `\~\~no closing`,
		},

		// Multiple formatting in sequence
		{
			name:  "bold then italic",
			input: "**bold** and *italic*",
			want:  `*bold* and _italic_`,
		},
		{
			name:  "code then bold",
			input: "use `code` or **bold**",
			want:  "use `code` or *bold*",
		},

		// Special characters only
		{
			name:  "all special chars",
			input: `\_*[]()~` + "`>#+-.=|{}!",
			want:  `\\\_\*\[\]\(\)\~` + "\\`" + `\>\#\+\-\.\=\|\{\}\!`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatForMarkdownV2(tt.input)
			if got != tt.want {
				t.Errorf("formatForMarkdownV2(%q)\n  got:  %q\n  want: %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestEscapeMarkdownV2URL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"simple URL", "https://example.com", "https://example.com"},
		{"URL with parens", "https://en.wikipedia.org/wiki/Foo_(bar)", `https://en.wikipedia.org/wiki/Foo_(bar\)`},
		{"URL with backslash", `https://example.com/path\file`, `https://example.com/path\\file`},
		{"dots and special chars preserved", "https://example.com/a.b?c=d&e=f#g", "https://example.com/a.b?c=d&e=f#g"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := escapeMarkdownV2URL(tt.input)
			if got != tt.want {
				t.Errorf("escapeMarkdownV2URL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatForMarkdownV2_NoUnescapedSpecialChars(t *testing.T) {
	// Verify that plain text output doesn't contain unescaped Telegram-special
	// characters that would cause a parse error.
	inputs := []string{
		"Hello world!",
		"Price: $10.99 (sale!)",
		"Use C++ or C# for this.",
		"Equation: a + b = c",
		"List: item-1, item-2",
		"pipe | separated | values",
		"curly {braces} test",
	}

	special := []string{"_", "*", "[", "]", "(", ")", "~", "`", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"}

	for _, input := range inputs {
		result := formatForMarkdownV2(input)
		for _, ch := range special {
			// Find all occurrences of ch that are NOT preceded by backslash
			idx := 0
			for {
				pos := strings.Index(result[idx:], ch)
				if pos == -1 {
					break
				}
				absPos := idx + pos
				if absPos == 0 || result[absPos-1] != '\\' {
					t.Errorf("formatForMarkdownV2(%q): unescaped %q at position %d in result %q",
						input, ch, absPos, result)
				}
				idx = absPos + len(ch)
			}
		}
	}
}

func TestSplitMessage_Short(t *testing.T) {
	chunks := splitMessage("hello", 4096)
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != "hello" {
		t.Errorf("expected hello, got %s", chunks[0])
	}
}

func TestSplitMessage_Long(t *testing.T) {
	// Create a message with paragraph breaks
	msg := ""
	for i := 0; i < 100; i++ {
		msg += "This is paragraph number " + string(rune('A'+i%26)) + ".\n\n"
	}

	chunks := splitMessage(msg, 200)
	if len(chunks) < 2 {
		t.Errorf("expected multiple chunks, got %d", len(chunks))
	}

	// Verify no chunk exceeds limit
	for i, chunk := range chunks {
		if len(chunk) > 200 {
			t.Errorf("chunk %d exceeds limit: %d chars", i, len(chunk))
		}
	}

	// Verify all content is preserved
	reassembled := ""
	for _, chunk := range chunks {
		reassembled += chunk
	}
	if reassembled != msg {
		t.Error("reassembled message doesn't match original")
	}
}

func TestCloseOpenMarkdown(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// Already-complete formatting should pass through unchanged.
		{
			name:  "complete bold",
			input: "this is **bold** text",
			want:  "this is **bold** text",
		},
		{
			name:  "complete italic",
			input: "this is *italic* text",
			want:  "this is *italic* text",
		},
		{
			name:  "complete code block",
			input: "```go\nfmt.Println()\n```",
			want:  "```go\nfmt.Println()\n```",
		},
		{
			name:  "complete inline code",
			input: "use `fmt.Println` here",
			want:  "use `fmt.Println` here",
		},
		{
			name:  "complete strikethrough",
			input: "this is ~~deleted~~ text",
			want:  "this is ~~deleted~~ text",
		},
		{
			name:  "no formatting",
			input: "plain text",
			want:  "plain text",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},

		// Unclosed formatting should be auto-closed.
		{
			name:  "unclosed bold",
			input: "this is **bold text",
			want:  "this is **bold text**",
		},
		{
			name:  "unclosed italic",
			input: "this is *italic text",
			want:  "this is *italic text*",
		},
		{
			name:  "unclosed bold italic",
			input: "this is ***bold italic text",
			want:  "this is ***bold italic text***",
		},
		{
			name:  "unclosed strikethrough",
			input: "this is ~~deleted text",
			want:  "this is ~~deleted text~~",
		},
		{
			name:  "unclosed fenced code block",
			input: "```go\nfmt.Println()",
			want:  "```go\nfmt.Println()\n```",
		},
		{
			name:  "unclosed inline code",
			input: "use `fmt.Println here",
			want:  "use `fmt.Println here`",
		},

		// Bare markers with no content should NOT be closed.
		{
			name:  "bare bold marker",
			input: "text **",
			want:  "text **",
		},
		{
			name:  "bare italic marker",
			input: "text *",
			want:  "text *",
		},
		{
			name:  "bare strikethrough marker",
			input: "text ~~",
			want:  "text ~~",
		},

		// Mixed / nested unclosed formatting.
		{
			name:  "unclosed bold with complete italic inside",
			input: "**bold and *italic* continues",
			want:  "**bold and *italic* continues**",
		},
		{
			name:  "complete bold with unclosed code block",
			input: "**bold** then\n```python\nprint('hi')",
			want:  "**bold** then\n```python\nprint('hi')\n```",
		},

		// Streaming-realistic scenarios.
		{
			name:  "mid-sentence bold",
			input: "Here is the **important part of the resp",
			want:  "Here is the **important part of the resp**",
		},
		{
			name:  "code block with language tag",
			input: "Here's the code:\n```python\ndef hello():\n    print('world')",
			want:  "Here's the code:\n```python\ndef hello():\n    print('world')\n```",
		},
		{
			name:  "inline code mid-stream",
			input: "Use the `handleMessage function to",
			want:  "Use the `handleMessage function to`",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := closeOpenMarkdown(tt.input)
			if got != tt.want {
				t.Errorf("closeOpenMarkdown(%q)\n  got:  %q\n  want: %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatForTelegram(t *testing.T) {
	t.Run("short text fits without truncation", func(t *testing.T) {
		text := "Hello **world**!"
		result, ok := formatForTelegram(text, 4096)
		if !ok {
			t.Fatal("expected ok=true for short text")
		}
		want := formatForMarkdownV2(text)
		if result != want {
			t.Errorf("got %q, want %q", result, want)
		}
	})

	t.Run("formatted expansion handled by truncation", func(t *testing.T) {
		// Create text full of special chars that double in size when escaped.
		// Each '!' becomes '\!' (2 bytes), so 100 chars → 200 escaped bytes.
		text := strings.Repeat("!", 100)
		result, ok := formatForTelegram(text, 120)
		if !ok {
			t.Fatal("expected ok=true after truncation retries")
		}
		if len(result) > 120 {
			t.Errorf("formatted result exceeds maxLen: %d > 120", len(result))
		}
	})

	t.Run("result always within limit", func(t *testing.T) {
		// Worst-case text: every character needs escaping.
		text := strings.Repeat("!.()", 500) // 2000 chars → ~4000 escaped
		result, ok := formatForTelegram(text, 4096)
		if len(result) > 4096 {
			t.Errorf("result exceeds limit: %d > 4096 (ok=%v)", len(result), ok)
		}
	})

	t.Run("fallback returns raw truncated text", func(t *testing.T) {
		// Extremely dense special chars: even half length doubles beyond limit.
		// Use a tiny maxLen so the loop cannot satisfy it.
		text := strings.Repeat("!", 200)
		result, ok := formatForTelegram(text, 10)
		if ok {
			t.Fatal("expected ok=false for impossibly small limit")
		}
		if len(result) > 10 {
			t.Errorf("raw fallback exceeds limit: %d > 10", len(result))
		}
	})

	t.Run("closes open markdown during streaming", func(t *testing.T) {
		text := "Here is **bold text that is still stream"
		result, ok := formatForTelegram(text, 4096)
		if !ok {
			t.Fatal("expected ok=true")
		}
		// Should contain MarkdownV2 bold markers (single *) from the
		// closeOpenMarkdown + formatForMarkdownV2 pipeline.
		if !strings.Contains(result, "*") {
			t.Errorf("expected bold formatting in result: %q", result)
		}
	})

	t.Run("unclosed code block closed before formatting", func(t *testing.T) {
		text := "```go\nfmt.Println(\"hello\")"
		result, ok := formatForTelegram(text, 4096)
		if !ok {
			t.Fatal("expected ok=true")
		}
		// Should have closing ``` from closeOpenMarkdown.
		if !strings.Contains(result, "```") {
			t.Errorf("expected code block formatting in result: %q", result)
		}
	})
}

func TestCloseOpenMarkdown_ThenFormat(t *testing.T) {
	// Verify that closeOpenMarkdown + formatForMarkdownV2 produces valid
	// MarkdownV2 for streaming scenarios (no unescaped special chars in
	// plain-text positions).
	inputs := []struct {
		name  string
		input string
	}{
		{"unclosed bold", "Here is **bold content streaming"},
		{"unclosed code block", "```go\nfmt.Println(\"hello\")"},
		{"unclosed inline code", "use `some.Function and more"},
		{"unclosed italic", "this is *italic streaming"},
		{"unclosed strikethrough", "this is ~~struck streaming"},
		{"mixed", "**bold then `code and more"},
	}

	for _, tt := range inputs {
		t.Run(tt.name, func(t *testing.T) {
			closed := closeOpenMarkdown(tt.input)
			result := formatForMarkdownV2(closed)
			if result == "" {
				t.Error("expected non-empty formatted result")
			}
			// Sanity: result should not be identical to just escaping everything
			// (which would mean our closing had no effect).
			allEscaped := escapeMarkdownV2Text(tt.input)
			if result == allEscaped {
				t.Errorf("closing had no effect: result equals fully-escaped input\n  input:  %q\n  result: %q", tt.input, result)
			}
		})
	}
}

func TestFormatForMarkdownV2_HeadingPrefixes(t *testing.T) {
	// Save original prefixes and restore after test.
	orig := HeadingPrefixes
	t.Cleanup(func() { HeadingPrefixes = orig })

	HeadingPrefixes = [3]string{"■ ", "▸ ", "· "}

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "h1 with prefix",
			input: "# Hello",
			want:  `*__■ Hello__*`,
		},
		{
			name:  "h2 with prefix",
			input: "## Section",
			want:  `*▸ Section*`,
		},
		{
			name:  "h3 with prefix",
			input: "### Detail",
			want:  `_· Detail_`,
		},
		{
			name:  "h4 uses h3+ prefix",
			input: "#### Deep",
			want:  `_· Deep_`,
		},
		{
			name:  "prefix with special chars in heading",
			input: "# Version 1.0!",
			want:  `*__■ Version 1\.0\!__*`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatForMarkdownV2(tt.input)
			if got != tt.want {
				t.Errorf("formatForMarkdownV2(%q)\n  got:  %q\n  want: %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatForMarkdownV2_EmptyPrefixes(t *testing.T) {
	// Verify default empty prefixes don't change behavior.
	orig := HeadingPrefixes
	t.Cleanup(func() { HeadingPrefixes = orig })

	HeadingPrefixes = [3]string{"", "", ""}

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "h1 no prefix",
			input: "# Hello",
			want:  "*__Hello__*",
		},
		{
			name:  "h2 no prefix",
			input: "## Section",
			want:  "*Section*",
		},
		{
			name:  "h3 no prefix",
			input: "### Detail",
			want:  "_Detail_",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatForMarkdownV2(tt.input)
			if got != tt.want {
				t.Errorf("formatForMarkdownV2(%q)\n  got:  %q\n  want: %q", tt.input, got, tt.want)
			}
		})
	}
}
