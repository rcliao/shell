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
			want:  `*Hello World*`,
		},
		{
			name:  "h2 heading",
			input: "## Sub Heading",
			want:  `*Sub Heading*`,
		},
		{
			name:  "heading with special chars",
			input: "# Version 1.0!",
			want:  `*Version 1\.0\!*`,
		},
		{
			name:  "hash not at line start",
			input: "use C# language",
			want:  `use C\# language`,
		},
		{
			name:  "heading mid-text",
			input: "intro\n# Title\nbody",
			want:  "intro\n*Title*\nbody",
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
