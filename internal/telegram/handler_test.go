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
			want:  "```go\n// go\nfmt.Println(\"hello\")\n```",
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
			want:  "*__📌 Hello World__*",
		},
		{
			name:  "h2 heading",
			input: "## Sub Heading",
			want:  "*▸ Sub Heading*",
		},
		{
			name:  "h3 heading",
			input: "### Details",
			want:  "_· Details_",
		},
		{
			name:  "heading with special chars",
			input: "# Version 1.0!",
			want:  "*__📌 Version 1\\.0\\!__*",
		},
		{
			name:  "hash not at line start",
			input: "use C# language",
			want:  `use C\# language`,
		},
		{
			name:  "heading mid-text",
			input: "intro\n# Title\nbody",
			want:  "intro\n*__📌 Title__*\nbody",
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

		// Bullet lists with inline formatting
		{
			name:  "bullet list with bold",
			input: "- **important** item",
			want:  "\\- *important* item",
		},
		{
			name:  "bullet list with italic",
			input: "- *emphasis* here",
			want:  "\\- _emphasis_ here",
		},
		{
			name:  "bullet list with inline code",
			input: "- use `fmt.Println` here",
			want:  "\\- use `fmt.Println` here",
		},
		{
			name:  "bullet list with link",
			input: "- see [Google](https://google.com)",
			want:  "\\- see [Google](https://google.com)",
		},
		{
			name:  "bullet list with bold italic",
			input: "- ***bold italic*** text",
			want:  "\\- *_bold italic_* text",
		},
		{
			name:  "bullet list with strikethrough",
			input: "- ~~removed~~ text",
			want:  "\\- ~removed~ text",
		},
		{
			name:  "bullet list with spoiler",
			input: "- ||hidden|| text",
			want:  "\\- ||hidden|| text",
		},
		{
			name:  "bullet list with multiple formatting",
			input: "- **bold** and *italic* and `code`",
			want:  "\\- *bold* and _italic_ and `code`",
		},
		{
			name:  "multiline bullet list with formatting",
			input: "- **bold** line\n- *italic* line",
			want:  "\\- *bold* line\n\\- _italic_ line",
		},

		// Numbered lists with inline formatting
		{
			name:  "numbered list with bold",
			input: "1. **important** item",
			want:  "1\\. *important* item",
		},
		{
			name:  "numbered list with italic",
			input: "1. *emphasis* here",
			want:  "1\\. _emphasis_ here",
		},
		{
			name:  "numbered list with inline code",
			input: "1. use `fmt.Println` here",
			want:  "1\\. use `fmt.Println` here",
		},
		{
			name:  "numbered list with link",
			input: "1. see [Google](https://google.com)",
			want:  "1\\. see [Google](https://google.com)",
		},
		{
			name:  "numbered list with multiple formatting",
			input: "1. **bold** and `code`\n2. *italic* text",
			want:  "1\\. *bold* and `code`\n2\\. _italic_ text",
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
			want:  "*__📌 Title__*\n>quoted line\nnormal text",
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
		{
			name:  "blockquote block then text then blockquote",
			input: "> block1 line1\n> block1 line2\ntext\n> block2",
			want:  ">block1 line1\n>block1 line2\ntext\n>block2",
		},
		{
			name:  "blockquote with empty line in middle",
			input: "> first\n>\n> third",
			want:  ">first\n>\n>third",
		},

		// Horizontal rules
		{
			name:  "horizontal rule with three dashes",
			input: "---",
			want:  "———",
		},
		{
			name:  "horizontal rule with many dashes",
			input: "-----",
			want:  "———",
		},
		{
			name:  "horizontal rule between paragraphs",
			input: "above\n---\nbelow",
			want:  "above\n———\nbelow",
		},
		{
			name:  "horizontal rule with trailing spaces",
			input: "---   ",
			want:  "———",
		},
		{
			name:  "two dashes is not a horizontal rule",
			input: "--",
			want:  `\-\-`,
		},
		{
			name:  "dash with text is not a horizontal rule",
			input: "--- text",
			want:  `\-\-\- text`,
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

		// Spoiler
		{
			name:  "spoiler",
			input: "this is ||hidden|| text",
			want:  `this is ||hidden|| text`,
		},
		{
			name:  "spoiler with special chars",
			input: "||secret! (info)||",
			want:  `||secret\! \(info\)||`,
		},
		{
			name:  "unmatched spoiler",
			input: "||no closing",
			want:  `\|\|no closing`,
		},
		{
			name:  "spoiler with bold inside",
			input: "see ||**hidden**|| here",
			want:  `see ||*hidden*|| here`,
		},
		{
			name:  "single pipe not spoiler",
			input: "a | b",
			want:  `a \| b`,
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

		// Nested inline formatting
		{
			name:  "bold with italic inside",
			input: "**bold *italic* text**",
			want:  "*bold _italic_ text*",
		},
		{
			name:  "bold with code inside",
			input: "**bold `code` text**",
			want:  "*bold `code` text*",
		},
		{
			name:  "italic with bold inside",
			input: "*italic **bold** text*",
			want:  "_italic *bold* text_",
		},
		{
			name:  "italic with code inside",
			input: "*italic `code` text*",
			want:  "_italic `code` text_",
		},
		{
			name:  "bold italic with code inside",
			input: "***bold italic `code` text***",
			want:  "*_bold italic `code` text_*",
		},
		{
			name:  "strikethrough with bold inside",
			input: "~~strike **bold** text~~",
			want:  "~strike *bold* text~",
		},
		{
			name:  "strikethrough with code inside",
			input: "~~strike `code` text~~",
			want:  "~strike `code` text~",
		},
		{
			name:  "bold with link inside",
			input: "**see [Google](https://google.com) here**",
			want:  "*see [Google](https://google.com) here*",
		},
		{
			name:  "italic with link inside",
			input: "*see [Google](https://google.com) here*",
			want:  "_see [Google](https://google.com) here_",
		},
		{
			name:  "bold with strikethrough inside",
			input: "**bold ~~strike~~ text**",
			want:  "*bold ~strike~ text*",
		},

		// Nested bullet lists
		{
			name:  "nested bullet list two levels",
			input: "- parent\n  - child",
			want:  "\\- parent\n  \\- child",
		},
		{
			name:  "nested bullet list three levels",
			input: "- level 1\n  - level 2\n    - level 3",
			want:  "\\- level 1\n  \\- level 2\n    \\- level 3",
		},
		{
			name:  "nested bullet list with multiple children",
			input: "- parent\n  - child 1\n  - child 2\n- sibling",
			want:  "\\- parent\n  \\- child 1\n  \\- child 2\n\\- sibling",
		},
		{
			name:  "nested bullet list with inline formatting",
			input: "- **bold parent**\n  - *italic child*\n  - `code child`",
			want:  "\\- *bold parent*\n  \\- _italic child_\n  \\- `code child`",
		},
		{
			name:  "nested bullet list 4-space indent",
			input: "- top\n    - nested",
			want:  "\\- top\n    \\- nested",
		},
		{
			name:  "nested bullet list with tab indent",
			input: "- top\n\t- nested",
			want:  "\\- top\n\t\\- nested",
		},

		// Nested numbered lists
		{
			name:  "nested numbered list two levels",
			input: "1. parent\n   1. child",
			want:  "1\\. parent\n   1\\. child",
		},
		{
			name:  "nested numbered list three levels",
			input: "1. level 1\n   1. level 2\n      1. level 3",
			want:  "1\\. level 1\n   1\\. level 2\n      1\\. level 3",
		},
		{
			name:  "nested numbered list with multiple children",
			input: "1. parent\n   1. child 1\n   2. child 2\n2. sibling",
			want:  "1\\. parent\n   1\\. child 1\n   2\\. child 2\n2\\. sibling",
		},
		{
			name:  "nested numbered list with formatting",
			input: "1. **bold parent**\n   1. *italic child*",
			want:  "1\\. *bold parent*\n   1\\. _italic child_",
		},

		// Mixed nested lists (bullet under numbered, numbered under bullet)
		{
			name:  "bullet under numbered",
			input: "1. numbered\n   - bullet child",
			want:  "1\\. numbered\n   \\- bullet child",
		},
		{
			name:  "numbered under bullet",
			input: "- bullet\n  1. numbered child",
			want:  "\\- bullet\n  1\\. numbered child",
		},
		{
			name:  "deeply mixed nesting",
			input: "- bullet\n  1. number\n    - sub-bullet\n      2. deep number",
			want:  "\\- bullet\n  1\\. number\n    \\- sub\\-bullet\n      2\\. deep number",
		},

		// Nested lists with surrounding content
		{
			name:  "nested list after heading",
			input: "## Items\n- parent\n  - child",
			want:  "*▸ Items*\n\\- parent\n  \\- child",
		},
		{
			name:  "nested list between paragraphs",
			input: "intro\n- parent\n  - child\noutro",
			want:  "intro\n\\- parent\n  \\- child\noutro",
		},
		{
			name:  "nested list with link child",
			input: "- see below\n  - [Google](https://google.com)",
			want:  "\\- see below\n  \\- [Google](https://google.com)",
		},

		// Asterisk bullet lists
		{
			name:  "asterisk bullet single item",
			input: "* item one",
			want:  `\* item one`,
		},
		{
			name:  "asterisk bullet multiple items",
			input: "* first\n* second\n* third",
			want:  "\\* first\n\\* second\n\\* third",
		},
		{
			name:  "asterisk bullet with inline formatting",
			input: "* **bold** and *italic*",
			want:  "\\* *bold* and _italic_",
		},
		{
			name:  "nested asterisk bullets",
			input: "* parent\n  * child\n    * grandchild",
			want:  "\\* parent\n  \\* child\n    \\* grandchild",
		},
		{
			name:  "asterisk not a bullet mid-line",
			input: "text * not a bullet",
			want:  `text \* not a bullet`,
		},
		{
			name:  "asterisk bullet with bold child",
			input: "* top\n  * **bold child**",
			want:  "\\* top\n  \\* *bold child*",
		},

		// Plus bullet lists
		{
			name:  "plus bullet single item",
			input: "+ item one",
			want:  `\+ item one`,
		},
		{
			name:  "plus bullet multiple items",
			input: "+ first\n+ second",
			want:  "\\+ first\n\\+ second",
		},
		{
			name:  "nested plus bullets",
			input: "+ parent\n  + child",
			want:  "\\+ parent\n  \\+ child",
		},
		{
			name:  "plus not a bullet mid-line",
			input: "a + b",
			want:  `a \+ b`,
		},
		{
			name:  "plus bullet with formatting",
			input: "+ **bold** and `code`",
			want:  "\\+ *bold* and `code`",
		},

		// Mixed marker nested lists
		{
			name:  "dash parent with asterisk children",
			input: "- parent\n  * child 1\n  * child 2",
			want:  "\\- parent\n  \\* child 1\n  \\* child 2",
		},
		{
			name:  "asterisk parent with dash children",
			input: "* parent\n  - child 1\n  - child 2",
			want:  "\\* parent\n  \\- child 1\n  \\- child 2",
		},
		{
			name:  "all three markers nested",
			input: "- dash\n  * asterisk\n    + plus",
			want:  "\\- dash\n  \\* asterisk\n    \\+ plus",
		},
		{
			name:  "numbered with asterisk children",
			input: "1. parent\n   * child",
			want:  "1\\. parent\n   \\* child",
		},

		// Tables
		{
			name:  "simple table with separator",
			input: "| Name | Age |\n|------|-----|\n| Alice | 30 |\n| Bob | 25 |",
			want:  "```\n┌───────┬─────┐\n│ Name  │ Age │\n├───────┼─────┤\n│ Alice │ 30  │\n│ Bob   │ 25  │\n└───────┴─────┘\n```",
		},
		{
			name:  "table with header separator",
			input: "| Header 1 | Header 2 |\n|----------|----------|\n| Cell 1 | Cell 2 |",
			want:  "```\n┌──────────┬──────────┐\n│ Header 1 │ Header 2 │\n├──────────┼──────────┤\n│ Cell 1   │ Cell 2   │\n└──────────┴──────────┘\n```",
		},
		{
			name:  "table without separator",
			input: "| A | B |\n| C | D |",
			want:  "```\n┌───┬───┐\n│ A │ B │\n│ C │ D │\n└───┴───┘\n```",
		},
		{
			name:  "table between paragraphs",
			input: "intro\n| X | Y |\n|---|---|\n| 1 | 2 |\noutro",
			want:  "intro\n```\n┌───┬───┐\n│ X │ Y │\n├───┼───┤\n│ 1 │ 2 │\n└───┴───┘\n```\noutro",
		},
		{
			name:  "table with backtick in cell",
			input: "| Code | Desc |\n|------|------|\n| `x` | test |",
			want:  "```\n┌───────┬──────┐\n│ Code  │ Desc │\n├───────┼──────┤\n│ \\`x\\` │ test │\n└───────┴──────┘\n```",
		},
		{
			name:  "table with alignment markers",
			input: "| Left | Center | Right |\n|:-----|:------:|------:|\n| L | C | R |",
			want:  "```\n┌──────┬────────┬───────┐\n│ Left │ Center │ Right │\n├──────┼────────┼───────┤\n│ L    │ C      │ R     │\n└──────┴────────┴───────┘\n```",
		},
		{
			name:  "three column table",
			input: "| A | B | C |\n|---|---|---|\n| 1 | 2 | 3 |\n| 4 | 5 | 6 |",
			want:  "```\n┌───┬───┬───┐\n│ A │ B │ C │\n├───┼───┼───┤\n│ 1 │ 2 │ 3 │\n│ 4 │ 5 │ 6 │\n└───┴───┴───┘\n```",
		},
		{
			name:  "single pipe line not a table",
			input: "| just a pipe",
			want:  `\| just a pipe`,
		},
		{
			name:  "pipe not at line start not a table",
			input: "text | more | text",
			want:  `text \| more \| text`,
		},
		{
			name:  "table after heading",
			input: "## Data\n| K | V |\n|---|---|\n| a | b |",
			want:  "*▸ Data*\n```\n┌───┬───┐\n│ K │ V │\n├───┼───┤\n│ a │ b │\n└───┴───┘\n```",
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

	// Verify no chunk exceeds limit after formatting
	for i, chunk := range chunks {
		fmtLen := len(formatForMarkdownV2(chunk))
		if fmtLen > 200 {
			t.Errorf("chunk %d formatted length exceeds limit: %d chars", i, fmtLen)
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

func TestSplitMessage_FormattedLengthRespected(t *testing.T) {
	// Text dense with special chars: each '!' becomes '\!' (2x expansion).
	// With maxLen=100 and 200 '!' chars, raw text fits in two 100-char chunks
	// but each formatted chunk would be 200 chars — exceeding the limit.
	// splitMessage must account for the formatted length.
	msg := strings.Repeat("!", 200)
	chunks := splitMessage(msg, 100)

	for i, chunk := range chunks {
		fmtLen := len(formatForMarkdownV2(chunk))
		if fmtLen > 100 {
			t.Errorf("chunk %d formatted length exceeds limit: %d > 100", i, fmtLen)
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

func TestSplitMessage_CodeBlockIntact(t *testing.T) {
	// The code block contains a \n\n inside it. Without code-block awareness,
	// splitMessage would split at that \n\n, breaking the code block.
	prefix := strings.Repeat("x", 50)
	codeBlock := "```\nfoo\n\nbar\n```"
	suffix := strings.Repeat("y", 200)
	msg := prefix + "\n" + codeBlock + "\n" + suffix

	chunks := splitMessage(msg, 100)

	// Verify no chunk has unbalanced code fences.
	for i, chunk := range chunks {
		if strings.Count(chunk, "```")%2 != 0 {
			t.Errorf("chunk %d has unbalanced code fences: %q", i, chunk)
		}
	}

	// Verify all content is preserved.
	reassembled := strings.Join(chunks, "")
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
			name:  "complete spoiler",
			input: "this is ||hidden|| text",
			want:  "this is ||hidden|| text",
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
			name:  "unclosed spoiler",
			input: "this is ||hidden text",
			want:  "this is ||hidden text||",
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
		{
			name:  "bare spoiler marker",
			input: "text ||",
			want:  "text ||",
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
		{"unclosed spoiler", "this is ||hidden streaming"},
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

func TestFormatErrorForMarkdownV2(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want string
	}{
		{
			name: "simple error",
			msg:  "something went wrong",
			want: ">⚠️ *Error*\n>\n>something went wrong",
		},
		{
			name: "error with special chars",
			msg:  "failed to parse config.yaml: unexpected token",
			want: ">⚠️ *Error*\n>\n>failed to parse config\\.yaml: unexpected token",
		},
		{
			name: "multiline error",
			msg:  "line one\nline two",
			want: ">⚠️ *Error*\n>\n>line one\n>line two",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatErrorForMarkdownV2(tt.msg)
			if got != tt.want {
				t.Errorf("formatErrorForMarkdownV2(%q)\n  got:  %q\n  want: %q", tt.msg, got, tt.want)
			}
		})
	}
}
