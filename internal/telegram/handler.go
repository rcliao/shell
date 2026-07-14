package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/rcliao/shell/internal/bridge"
)

const streamEditInterval = time.Second // minimum interval between Telegram message edits

const maxMessageLength = 4096

// spinner frames for the thinking indicator.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// thinkingPhrases rotate to give the user a sense of progress.
var thinkingPhrases = []string{
	"Thinking",
	"Reasoning",
	"Working",
	"Processing",
	"Analyzing",
}

// thinkingMessage returns a Claude Code-style animated status for the given
// tick. The placeholder ticker runs every 2s, so tick doubles as elapsed
// seconds/2. Past ~20s and ~60s the phrasing switches to honest long-wait
// reassurance so a slow turn (large context on a heavier model, or a slow
// tool) reads as "still working" rather than a dead "Analyzing" — the symptom
// the owner reported for umbreon (V2-H13).
func thinkingMessage(tick int) string {
	frame := spinnerFrames[tick%len(spinnerFrames)]
	dots := strings.Repeat(".", (tick%3)+1)
	switch {
	case tick >= 30: // ~60s+
		return fmt.Sprintf("%s Still working — this one's taking a while, hang tight%s", frame, dots)
	case tick >= 10: // ~20s+
		return fmt.Sprintf("%s Still working (loading a lot of context)%s", frame, dots)
	default:
		phrase := thinkingPhrases[(tick/5)%len(thinkingPhrases)]
		return fmt.Sprintf("%s %s%s", frame, phrase, dots)
	}
}

// friendlyTurnError maps low-level turn failures (timeouts, cancellations) to a
// short retryable message. A raw "context deadline exceeded" reads as a crash;
// a slow/hung turn should tell the user to just ask again (V2-H13).
func friendlyTurnError(err error) string {
	if err == nil {
		return ""
	}
	e := strings.ToLower(err.Error())
	switch {
	case strings.Contains(e, "deadline exceeded"), strings.Contains(e, "timeout"), strings.Contains(e, "timed out"):
		return "⏳ That one took too long and timed out before I could answer — mind asking again? (usually a heavy-context turn or a slow tool.)"
	case strings.Contains(e, "context canceled"), strings.Contains(e, "signal: killed"):
		return "⚠️ That turn got interrupted before I finished — please ask again."
	}
	return ""
}

// albumCollectDelay is how long to wait for additional photos in a media group
// before processing the album. Telegram delivers album photos as separate
// messages in quick succession, so a short delay is sufficient.
const albumCollectDelay = 500 * time.Millisecond

// albumEntry holds buffered messages belonging to a single media group (album).
type albumEntry struct {
	messages []*models.Message
	timer    *time.Timer
}

// HeadingPrefixes controls text prefixes prepended to each heading level when
// rendered in Telegram MarkdownV2. Index 0 = H1, 1 = H2, 2 = H3+.
// Defaults provide visual hierarchy: 📌 for H1, ▸ for H2, · for H3+.
var HeadingPrefixes = [3]string{"📌 ", "▸ ", "· "}

const longRunningThreshold = 15 * time.Second // time before switching reaction from 👀 to ⏳

// formatErrorForMarkdownV2 formats an error message with a distinct visual
// style: a blockquote with ⚠️ prefix and bold "Error" label.
func formatErrorForMarkdownV2(msg string) string {
	escaped := escapeMarkdownV2Text(msg)
	lines := strings.Split(escaped, "\n")
	var b strings.Builder
	b.WriteString(">⚠️ *Error*\n>\n>")
	b.WriteString(strings.Join(lines, "\n>"))
	return b.String()
}

// Pre-compiled replacers for MarkdownV2 escaping, shared across calls.
var (
	mdV2TextReplacer = strings.NewReplacer(
		`\`, `\\`,
		`_`, `\_`,
		`*`, `\*`,
		`[`, `\[`,
		`]`, `\]`,
		`(`, `\(`,
		`)`, `\)`,
		`~`, `\~`,
		"`", "\\`",
		`>`, `\>`,
		`#`, `\#`,
		`+`, `\+`,
		`-`, `\-`,
		`=`, `\=`,
		`|`, `\|`,
		`{`, `\{`,
		`}`, `\}`,
		`.`, `\.`,
		`!`, `\!`,
	)
	mdV2CodeReplacer = strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
	)
	mdV2URLReplacer = strings.NewReplacer(
		`\`, `\\`,
		`)`, `\)`,
	)
)

// escapeMarkdownV2Text escapes special characters for Telegram MarkdownV2
// in plain text that should not be interpreted as formatting.
func escapeMarkdownV2Text(text string) string {
	return mdV2TextReplacer.Replace(text)
}

// escapeCodeContent escapes only \ and ` inside code blocks/inline code
// per Telegram MarkdownV2 rules.
func escapeCodeContent(text string) string {
	return mdV2CodeReplacer.Replace(text)
}

// escapeMarkdownV2URL escapes only \ and ) inside the URL part of an inline
// link, per Telegram MarkdownV2 rules.
func escapeMarkdownV2URL(url string) string {
	return mdV2URLReplacer.Replace(url)
}

// isLineStart reports whether position i in text is at the effective start
// of a line: either at position 0, right after a newline, or preceded only
// by whitespace since the last newline (or start of string).
func isLineStart(text string, i int) bool {
	if i == 0 || text[i-1] == '\n' {
		return true
	}
	for j := i - 1; j >= 0; j-- {
		if text[j] == '\n' {
			return true
		}
		if text[j] != ' ' && text[j] != '\t' {
			return false
		}
	}
	return true
}

// mdLinkRe matches standard Markdown links: [text](url)
var mdLinkRe = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)

// formatTableBlock parses markdown table lines and returns a monospace code
// block with aligned columns using Unicode box-drawing characters.
func formatTableBlock(lines []string) string {
	type tableRow struct {
		cells []string
		isSep bool
	}
	var rows []tableRow
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Remove leading/trailing pipes.
		inner := trimmed
		if strings.HasPrefix(inner, "|") {
			inner = inner[1:]
		}
		if strings.HasSuffix(inner, "|") {
			inner = inner[:len(inner)-1]
		}

		parts := strings.Split(inner, "|")
		cells := make([]string, len(parts))
		isSep := len(parts) > 0
		for j, p := range parts {
			cells[j] = strings.TrimSpace(p)
			if isSep {
				stripped := strings.Trim(cells[j], "-: ")
				if stripped != "" || !strings.Contains(cells[j], "-") {
					isSep = false
				}
			}
		}
		rows = append(rows, tableRow{cells: cells, isSep: isSep})
	}

	// Separate data rows from separator rows.
	var dataRows [][]string
	hasSep := false
	for _, r := range rows {
		if r.isSep {
			hasSep = true
			continue
		}
		dataRows = append(dataRows, r.cells)
	}

	if len(dataRows) == 0 {
		var b strings.Builder
		for i, line := range lines {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(escapeMarkdownV2Text(line))
		}
		return b.String()
	}

	// Escape cell contents for code blocks and calculate column widths.
	maxCols := 0
	for _, r := range dataRows {
		if len(r) > maxCols {
			maxCols = len(r)
		}
	}
	escaped := make([][]string, len(dataRows))
	for i, r := range dataRows {
		escaped[i] = make([]string, len(r))
		for j, cell := range r {
			escaped[i][j] = escapeCodeContent(cell)
		}
	}
	colWidths := make([]int, maxCols)
	for _, r := range escaped {
		for j, cell := range r {
			if len(cell) > colWidths[j] {
				colWidths[j] = len(cell)
			}
		}
	}
	for j := range colWidths {
		if colWidths[j] < 1 {
			colWidths[j] = 1
		}
	}

	// Helper to build a horizontal border line.
	borderLine := func(left, mid, right string) string {
		var b strings.Builder
		for j := 0; j < maxCols; j++ {
			if j == 0 {
				b.WriteString(left)
			} else {
				b.WriteString(mid)
			}
			b.WriteString(strings.Repeat("─", colWidths[j]+2))
		}
		b.WriteString(right)
		return b.String()
	}

	// Build aligned monospace table inside a code block.
	var content strings.Builder
	content.WriteByte('\n')
	content.WriteString(borderLine("┌", "┬", "┐"))
	content.WriteByte('\n')
	for ri, r := range escaped {
		content.WriteString("│")
		for j := 0; j < maxCols; j++ {
			cell := ""
			if j < len(r) {
				cell = r[j]
			}
			content.WriteByte(' ')
			content.WriteString(cell)
			for k := len(cell); k < colWidths[j]; k++ {
				content.WriteByte(' ')
			}
			content.WriteString(" │")
		}
		content.WriteByte('\n')
		// Draw separator after first row when the markdown had a separator.
		if ri == 0 && hasSep {
			content.WriteString(borderLine("├", "┼", "┤"))
			content.WriteByte('\n')
		}
	}
	content.WriteString(borderLine("└", "┴", "┘"))
	content.WriteByte('\n')

	var result strings.Builder
	result.WriteString("```")
	result.WriteString(content.String())
	result.WriteString("```")
	return result.String()
}

// formatForMarkdownV2 converts standard Markdown (as output by Claude) to
// Telegram MarkdownV2 format with selective escaping. It preserves bold,
// italic, code blocks, inline code, links, and nested lists. Nested lists
// (indented with spaces/tabs) are supported via isLineStart: leading
// whitespace passes through as plain text, and the list marker (-, *, +,
// or N.) at an indented position is recognized as a list item.
func formatForMarkdownV2(text string) string {
	var result strings.Builder
	i := 0
	n := len(text)

	for i < n {
		// 1. Fenced code blocks: ```lang\ncode```
		if i+2 < n && text[i] == '`' && text[i+1] == '`' && text[i+2] == '`' {
			end := strings.Index(text[i+3:], "```")
			if end != -1 {
				content := text[i+3 : i+3+end]
				result.WriteString("```")
				// Add language label (e.g., "// go") if language is specified
				if nlIdx := strings.Index(content, "\n"); nlIdx != -1 {
					lang := strings.TrimSpace(content[:nlIdx])
					if lang != "" {
						result.WriteString(escapeCodeContent(lang))
						result.WriteString("\n// ")
						result.WriteString(escapeCodeContent(lang))
						result.WriteString(escapeCodeContent(content[nlIdx:]))
					} else {
						result.WriteString(escapeCodeContent(content))
					}
				} else {
					result.WriteString(escapeCodeContent(content))
				}
				result.WriteString("```")
				i = i + 3 + end + 3
				continue
			}
		}

		// 2. Inline code: `code`
		if text[i] == '`' {
			end := strings.Index(text[i+1:], "`")
			if end != -1 && !strings.Contains(text[i+1:i+1+end], "\n") {
				content := text[i+1 : i+1+end]
				result.WriteByte('`')
				result.WriteString(escapeCodeContent(content))
				result.WriteByte('`')
				i = i + 1 + end + 1
				continue
			}
		}

		// 3. Bullet list item with * marker: "* text" at start of a line.
		// Must come before bold/italic checks — in CommonMark, "* " at line
		// start is always a bullet, never italic (space after * prevents
		// left-flanking delimiter).
		if text[i] == '*' && i+1 < n && text[i+1] == ' ' && isLineStart(text, i) {
			lineEnd := strings.Index(text[i+2:], "\n")
			var content string
			if lineEnd == -1 {
				content = text[i+2:]
				lineEnd = n - (i + 2)
			} else {
				content = text[i+2 : i+2+lineEnd]
			}
			result.WriteString("\\* ")
			result.WriteString(formatForMarkdownV2(content))
			i = i + 2 + lineEnd
			if i < n && text[i] == '\n' {
				result.WriteByte('\n')
				i++
			}
			continue
		}

		// 3a. Bold+Italic: ***text*** → *_text_* (Telegram MarkdownV2)
		if i+2 < n && text[i] == '*' && text[i+1] == '*' && text[i+2] == '*' {
			end := strings.Index(text[i+3:], "***")
			if end != -1 && end > 0 {
				inner := text[i+3 : i+3+end]
				result.WriteString("*_")
				result.WriteString(formatForMarkdownV2(inner))
				result.WriteString("_*")
				i = i + 3 + end + 3
				continue
			}
		}

		// 3b. Bold: **text** → *text* (Telegram MarkdownV2 bold)
		if i+1 < n && text[i] == '*' && text[i+1] == '*' {
			end := strings.Index(text[i+2:], "**")
			if end != -1 && end > 0 {
				inner := text[i+2 : i+2+end]
				result.WriteString("*")
				result.WriteString(formatForMarkdownV2(inner))
				result.WriteString("*")
				i = i + 2 + end + 2
				continue
			}
		}

		// 4. Italic: *text* → _text_ (Telegram MarkdownV2 italic)
		// Only match single * not preceded by another *
		if text[i] == '*' && (i == 0 || text[i-1] != '*') && (i+1 >= n || text[i+1] != '*') {
			// Search for a closing standalone * (not adjacent to another *)
			found := false
			searchFrom := i + 1
			for searchFrom < n {
				relEnd := strings.Index(text[searchFrom:], "*")
				if relEnd == -1 {
					break
				}
				closePos := searchFrom + relEnd
				if closePos <= i+1 {
					searchFrom = closePos + 1
					continue
				}
				// Closing * must be standalone: not preceded or followed by *
				if (closePos+1 >= n || text[closePos+1] != '*') && text[closePos-1] != '*' {
					inner := text[i+1 : closePos]
					result.WriteString("_")
					result.WriteString(formatForMarkdownV2(inner))
					result.WriteString("_")
					i = closePos + 1
					found = true
					break
				}
				searchFrom = closePos + 1
			}
			if found {
				continue
			}
		}

		// 5. Links: [text](url)
		if text[i] == '[' {
			remaining := text[i:]
			loc := mdLinkRe.FindStringIndex(remaining)
			if loc != nil && loc[0] == 0 {
				matches := mdLinkRe.FindStringSubmatch(remaining)
				linkText := matches[1]
				linkURL := matches[2]
				result.WriteString("[")
				result.WriteString(escapeMarkdownV2Text(linkText))
				result.WriteString("](")
				result.WriteString(escapeMarkdownV2URL(linkURL))
				result.WriteString(")")
				i += loc[1]
				continue
			}
		}

		// 6. Headings: # at start of line → formatting-based visual hierarchy
		if text[i] == '#' && (i == 0 || text[i-1] == '\n') {
			// Count heading level and skip # characters
			j := i
			level := 0
			for j < n && text[j] == '#' {
				j++
				level++
			}
			// Skip space after #
			if j < n && text[j] == ' ' {
				j++
			}
			// Find end of line
			lineEnd := strings.Index(text[j:], "\n")
			var heading string
			if lineEnd == -1 {
				heading = text[j:]
				lineEnd = n - j
			} else {
				heading = text[j : j+lineEnd]
			}
			escaped := escapeMarkdownV2Text(heading)
			// Determine prefix index: 0=H1, 1=H2, 2=H3+.
			pi := level - 1
			if pi > 2 {
				pi = 2
			}
			prefix := ""
			if HeadingPrefixes[pi] != "" {
				prefix = escapeMarkdownV2Text(HeadingPrefixes[pi])
			}
			switch {
			case level == 1:
				// H1: bold + underline (strongest emphasis)
				result.WriteString("*__")
				result.WriteString(prefix)
				result.WriteString(escaped)
				result.WriteString("__*")
			case level == 2:
				// H2: bold
				result.WriteString("*")
				result.WriteString(prefix)
				result.WriteString(escaped)
				result.WriteString("*")
			default:
				// H3+: italic
				result.WriteString("_")
				result.WriteString(prefix)
				result.WriteString(escaped)
				result.WriteString("_")
			}
			i = j + lineEnd
			if i < n && text[i] == '\n' {
				result.WriteByte('\n')
				i++
			}
			continue
		}

		// 7. Horizontal rule: 3+ dashes on a line by itself → visual separator
		if text[i] == '-' && isLineStart(text, i) {
			j := i
			for j < n && text[j] == '-' {
				j++
			}
			if j-i >= 3 {
				// Rest of line must be only whitespace
				k := j
				for k < n && text[k] != '\n' && (text[k] == ' ' || text[k] == '\t') {
					k++
				}
				if k == n || text[k] == '\n' {
					result.WriteString("———")
					i = k
					if i < n && text[i] == '\n' {
						result.WriteByte('\n')
						i++
					}
					continue
				}
			}
		}

		// 8a. Bullet list item: "- text" at start of a line (with optional leading
		// whitespace for nested lists — indentation is preserved in the output)
		if text[i] == '-' && i+1 < n && text[i+1] == ' ' && isLineStart(text, i) {
			lineEnd := strings.Index(text[i+2:], "\n")
			var content string
			if lineEnd == -1 {
				content = text[i+2:]
				lineEnd = n - (i + 2)
			} else {
				content = text[i+2 : i+2+lineEnd]
			}
			result.WriteString("\\- ")
			result.WriteString(formatForMarkdownV2(content))
			i = i + 2 + lineEnd
			if i < n && text[i] == '\n' {
				result.WriteByte('\n')
				i++
			}
			continue
		}

		// 8b. Bullet list item with + marker: "+ text" at start of a line
		if text[i] == '+' && i+1 < n && text[i+1] == ' ' && isLineStart(text, i) {
			lineEnd := strings.Index(text[i+2:], "\n")
			var content string
			if lineEnd == -1 {
				content = text[i+2:]
				lineEnd = n - (i + 2)
			} else {
				content = text[i+2 : i+2+lineEnd]
			}
			result.WriteString("\\+ ")
			result.WriteString(formatForMarkdownV2(content))
			i = i + 2 + lineEnd
			if i < n && text[i] == '\n' {
				result.WriteByte('\n')
				i++
			}
			continue
		}

		// 9. Numbered list item: "1. text" at start of a line (with optional leading whitespace)
		if text[i] >= '0' && text[i] <= '9' && isLineStart(text, i) {
			j := i + 1
			for j < n && text[j] >= '0' && text[j] <= '9' {
				j++
			}
			if j+1 < n && text[j] == '.' && text[j+1] == ' ' {
				lineEnd := strings.Index(text[j+2:], "\n")
				var content string
				if lineEnd == -1 {
					content = text[j+2:]
					lineEnd = n - (j + 2)
				} else {
					content = text[j+2 : j+2+lineEnd]
				}
				result.WriteString(text[i:j])
				result.WriteString("\\. ")
				result.WriteString(formatForMarkdownV2(content))
				i = j + 2 + lineEnd
				if i < n && text[i] == '\n' {
					result.WriteByte('\n')
					i++
				}
				continue
			}
		}

		// 10. Blockquote: consecutive "> text" lines merged into one blockquote block
		if text[i] == '>' && isLineStart(text, i) {
			var lines []string
			j := i
			consumedTrailingNewline := false
			for j < n {
				if text[j] != '>' || !isLineStart(text, j) {
					break
				}
				// Skip '>' and optional space
				k := j + 1
				if k < n && text[k] == ' ' {
					k++
				}
				lineEnd := strings.Index(text[k:], "\n")
				if lineEnd == -1 {
					lines = append(lines, text[k:])
					j = n
					consumedTrailingNewline = false
				} else {
					lines = append(lines, text[k:k+lineEnd])
					j = k + lineEnd + 1
					consumedTrailingNewline = true
				}
			}
			for idx, line := range lines {
				result.WriteString(">")
				result.WriteString(formatForMarkdownV2(line))
				if idx < len(lines)-1 || consumedTrailingNewline {
					result.WriteByte('\n')
				}
			}
			i = j
			continue
		}

		// 11. Table: consecutive lines starting with | (not ||) → monospace code block
		if text[i] == '|' && (i+1 < n && text[i+1] != '|') && isLineStart(text, i) {
			lineEnd := strings.Index(text[i:], "\n")
			lineLen := lineEnd
			if lineEnd == -1 {
				lineLen = n - i
			}
			firstLine := text[i : i+lineLen]
			if strings.Count(firstLine, "|") >= 2 {
				var tableLines []string
				j := i
				consumedTrailingNewline := false
				for j < n {
					le := strings.Index(text[j:], "\n")
					var line string
					if le == -1 {
						line = text[j:]
						le = n - j
					} else {
						line = text[j : j+le]
					}
					trimmed := strings.TrimSpace(line)
					if len(trimmed) == 0 || trimmed[0] != '|' || strings.Count(trimmed, "|") < 2 {
						break
					}
					tableLines = append(tableLines, line)
					j = j + le
					consumedTrailingNewline = false
					if j < n && text[j] == '\n' {
						j++
						consumedTrailingNewline = true
					}
				}
				if len(tableLines) >= 2 {
					result.WriteString(formatTableBlock(tableLines))
					if consumedTrailingNewline && j < n {
						result.WriteByte('\n')
					}
					i = j
					continue
				}
			}
		}

		// 12. Strikethrough: ~~text~~ → ~text~ (Telegram MarkdownV2)
		if i+1 < n && text[i] == '~' && text[i+1] == '~' {
			end := strings.Index(text[i+2:], "~~")
			if end != -1 && end > 0 {
				inner := text[i+2 : i+2+end]
				result.WriteString("~")
				result.WriteString(formatForMarkdownV2(inner))
				result.WriteString("~")
				i = i + 2 + end + 2
				continue
			}
		}

		// 13. Spoiler: ||text|| → ||text|| (Telegram MarkdownV2 expandable spoiler)
		if i+1 < n && text[i] == '|' && text[i+1] == '|' {
			end := strings.Index(text[i+2:], "||")
			if end != -1 && end > 0 {
				inner := text[i+2 : i+2+end]
				result.WriteString("||")
				result.WriteString(formatForMarkdownV2(inner))
				result.WriteString("||")
				i = i + 2 + end + 2
				continue
			}
		}

		// Plain text character — escape for MarkdownV2
		c := text[i]
		switch c {
		case '\\', '_', '*', '[', ']', '(', ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!':
			result.WriteByte('\\')
			result.WriteByte(c)
		default:
			result.WriteByte(c)
		}
		i++
	}

	return result.String()
}

// closeOpenMarkdown detects unclosed Markdown formatting in text (as occurs
// mid-stream) and appends closing markers so that formatForMarkdownV2 can
// produce valid, nicely formatted MarkdownV2 instead of escaping unclosed
// markers as literal characters.
func closeOpenMarkdown(text string) string {
	n := len(text)
	if n == 0 {
		return text
	}

	i := 0
	inFencedCode := false
	inInlineCode := false

	type marker struct {
		token string
		pos   int
	}
	var open []marker

	for i < n {
		// Fenced code blocks: ```
		if !inInlineCode && i+2 < n && text[i] == '`' && text[i+1] == '`' && text[i+2] == '`' {
			if inFencedCode {
				inFencedCode = false
				i += 3
				continue
			}
			inFencedCode = true
			i += 3
			// Skip past language tag / rest of opening line
			for i < n && text[i] != '\n' {
				i++
			}
			continue
		}

		// Inside fenced code block, just advance
		if inFencedCode {
			i++
			continue
		}

		// Inline code: `
		if text[i] == '`' {
			// Check for a closing backtick on the same line
			if i+1 < n {
				end := strings.Index(text[i+1:], "`")
				if end != -1 && !strings.Contains(text[i+1:i+1+end], "\n") {
					// Complete inline code, skip past it
					i = i + 1 + end + 1
					continue
				}
			}
			// Unclosed inline code
			inInlineCode = true
			i++
			continue
		}

		// Inside inline code, just advance
		if inInlineCode {
			i++
			continue
		}

		// Bold+Italic: ***
		if i+2 < n && text[i] == '*' && text[i+1] == '*' && text[i+2] == '*' {
			end := strings.Index(text[i+3:], "***")
			if end != -1 && end > 0 {
				i = i + 3 + end + 3
				continue
			}
			open = append(open, marker{"***", i})
			i += 3
			continue
		}

		// Bold: **
		if i+1 < n && text[i] == '*' && text[i+1] == '*' {
			end := strings.Index(text[i+2:], "**")
			if end != -1 && end > 0 {
				i = i + 2 + end + 2
				continue
			}
			open = append(open, marker{"**", i})
			i += 2
			continue
		}

		// Italic: * (single, not adjacent to another *)
		if text[i] == '*' && (i == 0 || text[i-1] != '*') && (i+1 >= n || text[i+1] != '*') {
			end := strings.Index(text[i+1:], "*")
			if end != -1 && end > 0 {
				closePos := i + 1 + end
				if closePos+1 >= n || text[closePos+1] != '*' {
					i = closePos + 1
					continue
				}
			}
			open = append(open, marker{"*", i})
			i++
			continue
		}

		// Strikethrough: ~~
		if i+1 < n && text[i] == '~' && text[i+1] == '~' {
			end := strings.Index(text[i+2:], "~~")
			if end != -1 && end > 0 {
				i = i + 2 + end + 2
				continue
			}
			open = append(open, marker{"~~", i})
			i += 2
			continue
		}

		// Spoiler: ||
		if i+1 < n && text[i] == '|' && text[i+1] == '|' {
			end := strings.Index(text[i+2:], "||")
			if end != -1 && end > 0 {
				i = i + 2 + end + 2
				continue
			}
			open = append(open, marker{"||", i})
			i += 2
			continue
		}

		i++
	}

	// Nothing to close
	if !inFencedCode && !inInlineCode && len(open) == 0 {
		return text
	}

	var suffix strings.Builder

	if inFencedCode {
		suffix.WriteString("\n```")
	} else {
		if inInlineCode {
			suffix.WriteByte('`')
		}
		// Close formatting markers in reverse order (innermost first).
		// Only close if there is actual content after the opening marker;
		// a bare marker with nothing after it (e.g. trailing "**") is left
		// for the formatter to escape normally.
		for j := len(open) - 1; j >= 0; j-- {
			m := open[j]
			if strings.TrimSpace(text[m.pos+len(m.token):]) != "" {
				suffix.WriteString(m.token)
			}
		}
	}

	if suffix.Len() == 0 {
		return text
	}

	return text + suffix.String()
}

// formatForTelegram converts Markdown text to Telegram MarkdownV2 format,
// ensuring the result fits within maxLen bytes. It closes any unclosed
// Markdown formatting before conversion (important for streaming content).
//
// Returns (text, mdv2-applied):
//   - When formatted fits → return formatted, true
//   - When formatted overflows but RAW text fits → return raw, false
//     (plain-text fallback preserves full content over partial formatted)
//   - When raw also overflows → return text[:maxLen-3]+"..." (head-keeping
//     truncation), false. Mami's "cut off" complaint (cycle 80) traced to
//     the previous tail-keeping behavior where setup was dropped.
func formatForTelegram(text string, maxLen int) (string, bool) {
	formatted := formatForMarkdownV2(closeOpenMarkdown(text))
	if telegramLen(formatted) <= maxLen {
		return formatted, true
	}

	// Cycle 80: prefer plain-text-full over formatted-partial. Heavy
	// MarkdownV2 escaping can 2-3x message size for CJK + bold/list content;
	// if the raw text fits unformatted, send it without markdown rather
	// than truncating mid-content.
	if telegramLen(text) <= maxLen {
		return text, false
	}

	// Both formatted and raw overflow. Truncate raw HEAD-FIRST (preserve
	// setup; drop tail), rune-safe and in UTF-16 units. Better than the
	// previous suffix-only behavior which made responses feel cut-off.
	if maxLen >= 3 {
		return truncateToTelegramLen(text, maxLen-3) + "...", false
	}
	return truncateToTelegramLen(text, maxLen), false
}

// telegramLen returns the length of s in UTF-16 code units — the unit
// Telegram uses for its 4096-character message limit. Go's len() counts
// bytes, which over-counts CJK ~3x (and emoji 4x), causing Chinese replies to
// be truncated/split at roughly one-third their real length.
func telegramLen(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2 // astral plane (most emoji) → UTF-16 surrogate pair
		} else {
			n++
		}
	}
	return n
}

// truncateToTelegramLen returns the longest rune-aligned prefix of s whose
// telegramLen is <= max. Never splits a multi-byte rune.
func truncateToTelegramLen(s string, max int) string {
	if max <= 0 {
		return ""
	}
	n := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if n+w > max {
			return s[:i]
		}
		n += w
	}
	return s
}

// byteEndForTelegramBudget returns the byte offset of the longest rune-aligned
// prefix of s whose telegramLen is <= budget (an upper bound for split search).
func byteEndForTelegramBudget(s string, budget int) int {
	n := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if n+w > budget {
			return i
		}
		n += w
	}
	return len(s)
}

// isNotModified reports whether a Telegram edit error is the benign
// "message is not modified" response (identical content) — treated as success
// so the final reconcile edit doesn't fall back to plain text and lose markup.
func isNotModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not modified")
}

// maxFloodRetryAfter bounds how long a single flood-control wait may be —
// beyond this the handler gives up rather than hanging on one edit.
const maxFloodRetryAfter = 65 * time.Second

// floodWait pauses for a flood-control retry window; overridable in tests.
var floodWait = func(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// withFloodRetry runs fn, retrying through Telegram flood-control (429)
// windows using the server-provided retry_after. Streaming edits are throttled
// best-effort, but the FINAL reconcile edit must land: if it is dropped the
// user is left looking at the last streamed partial — a user-visible
// truncation (7/12 「回答被截掉了」).
func withFloodRetry(ctx context.Context, fn func() error) error {
	const attempts = 3
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		err = fn()
		var flood *bot.TooManyRequestsError
		if err == nil || !errors.As(err, &flood) {
			return err
		}
		if attempt == attempts-1 {
			break // out of retries — don't wait for a window we won't use
		}
		wait := time.Duration(flood.RetryAfter) * time.Second
		if wait <= 0 {
			wait = time.Second
		}
		if wait > maxFloodRetryAfter || !floodWait(ctx, wait) {
			return err
		}
	}
	return err
}

// editFinal is withFloodRetry around a message edit, for final reconcile
// edits that must not be silently dropped.
func editFinal(ctx context.Context, b *bot.Bot, params *bot.EditMessageTextParams) error {
	return withFloodRetry(ctx, func() error {
		_, err := b.EditMessageText(ctx, params)
		return err
	})
}

// tailKeepTelegramLen returns the longest rune-aligned SUFFIX of s whose
// telegramLen is <= max (used by streaming edits that keep the latest content).
func tailKeepTelegramLen(s string, max int) string {
	if telegramLen(s) <= max {
		return s
	}
	rs := []rune(s)
	units, i := 0, len(rs)
	for i > 0 {
		w := 1
		if rs[i-1] > 0xFFFF {
			w = 2
		}
		if units+w > max {
			break
		}
		units += w
		i--
	}
	return string(rs[i:])
}

// botExchangeLimit is the max consecutive bot-to-bot messages before suppressing.
const botExchangeLimit = 3

// botCooldown is the minimum time between this bot's responses to peer bot messages.
const botCooldown = 30 * time.Second

// peerBroadcastProbability is a reduced probability for responding to peer bot messages
// (lower than normal broadcast to keep bot-to-bot exchanges sparse).
const peerBroadcastProbability = 0.15

type Handler struct {
	auth   *Auth
	bridge *bridge.Bridge

	// Multi-agent group chat support
	botUsername          string
	myAliases            []string // name variants for this agent (lowercased)
	broadcastProbability float64
	peerBotUsernames     map[string]bool
	peerAliases          []string // name variants for peer agents (lowercased)
	groupMode            string   // "autonomous" = always deliver, agent decides via [noop]
	groupDomain          string   // role-based routing domain for this agent ("practical"|"companionship"|"")

	// Bot-to-bot exchange tracking (per chat)
	botExchangeMu    sync.Mutex
	botExchangeCount map[int64]int       // consecutive bot-to-bot messages per chat
	botLastResponse  map[int64]time.Time // last time this bot responded to a peer

	albumsMu sync.Mutex
	albums   map[string]*albumEntry // keyed by MediaGroupID

	chatLocksMu sync.Mutex
	chatLocks   map[chatLockKey]*sync.Mutex // per (chat, thread) message serialization
}

// chatLockKey identifies a message serialization lock by (chat_id, message_thread_id).
// Topics within a chat serialize independently so replies in different topics
// don't queue behind each other.
type chatLockKey struct {
	chatID   int64
	threadID int64
}

// AgentConfig holds agent identity fields passed to the handler.
type AgentConfig struct {
	BotUsername          string
	Aliases              []string // name variants for this agent (e.g. "pika")
	BroadcastProbability float64
	PeerBots             []string
	PeerAliases          []string // name variants for peer agents (e.g. "umbreon", "小傘")
	GroupMode            string   // "autonomous" = agent decides, "" = legacy probability
	GroupDomain          string   // "practical" | "companionship" — role-based routing for general messages (empty = no routing)
}

func NewHandler(auth *Auth, br *bridge.Bridge, agentCfg AgentConfig) *Handler {
	peers := make(map[string]bool, len(agentCfg.PeerBots))
	for _, p := range agentCfg.PeerBots {
		peers[strings.ToLower(p)] = true
	}
	var myAliases []string
	for _, a := range agentCfg.Aliases {
		myAliases = append(myAliases, strings.ToLower(a))
	}
	var peerAliases []string
	for _, a := range agentCfg.PeerAliases {
		peerAliases = append(peerAliases, strings.ToLower(a))
	}
	return &Handler{
		auth:                 auth,
		bridge:               br,
		botUsername:          strings.ToLower(agentCfg.BotUsername),
		myAliases:            myAliases,
		broadcastProbability: agentCfg.BroadcastProbability,
		peerBotUsernames:     peers,
		peerAliases:          peerAliases,
		groupMode:            agentCfg.GroupMode,
		groupDomain:          agentCfg.GroupDomain,
		botExchangeCount:     make(map[int64]int),
		botLastResponse:      make(map[int64]time.Time),
		albums:               make(map[string]*albumEntry),
		chatLocks:            make(map[chatLockKey]*sync.Mutex),
	}
}

// mentionRegex matches @username mentions in message text.
var mentionRegex = regexp.MustCompile(`@(\w+)`)

// parseMentions extracts all @usernames from text (lowercased, without @).
func parseMentions(text string) []string {
	matches := mentionRegex.FindAllStringSubmatch(text, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, strings.ToLower(m[1]))
	}
	return out
}

// stripMention removes @username from text and trims whitespace.
func stripMention(text, username string) string {
	re := regexp.MustCompile(`(?i)@` + regexp.QuoteMeta(username) + `\b`)
	return strings.TrimSpace(re.ReplaceAllString(text, ""))
}

// messageAddressedToPeer checks if the message text starts with a peer agent's name or alias.
// This catches natural language addressing like "pika lunch memo" or "umbreon check this".
func (h *Handler) messageAddressedToPeer(text string) bool {
	return addressedTo(text, h.peerAliases)
}

// messageAddressedToMe checks if the message text starts with this agent's name or alias.
func (h *Handler) messageAddressedToMe(text string) bool {
	return addressedTo(text, h.myAliases)
}

// addressLeadRe strips leading punctuation/emoji/whitespace so "@name", "name~",
// "  Umbreon:" all normalize before we test the leading alias.
var addressLeadRe = regexp.MustCompile(`^[\s\p{P}\p{S}]+`)

// addressedTo reports whether the message opens by naming one of the aliases.
// Match is anchored to the start (after stripping lead punctuation/emoji) so a
// mere mention mid-sentence doesn't count — only an actual address does.
func addressedTo(text string, aliases []string) bool {
	lower := strings.ToLower(addressLeadRe.ReplaceAllString(text, ""))
	for _, alias := range aliases {
		if alias == "" {
			continue
		}
		if strings.HasPrefix(lower, strings.ToLower(alias)) {
			return true
		}
	}
	return false
}

// truncate returns the first n characters of a string, appending "..." if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// isPeerBot checks if the message sender is a known peer bot.
func (h *Handler) isPeerBot(msg *models.Message) bool {
	if msg.From == nil {
		return false
	}
	return h.peerBotUsernames[strings.ToLower(msg.From.Username)]
}

// recordBotExchange tracks that this bot responded to a peer bot message.
func (h *Handler) recordBotExchange(chatID int64) {
	h.botExchangeMu.Lock()
	defer h.botExchangeMu.Unlock()
	h.botExchangeCount[chatID]++
	h.botLastResponse[chatID] = time.Now()
}

// resetBotExchange resets the bot-to-bot exchange counter (called when a human messages).
func (h *Handler) resetBotExchange(chatID int64) {
	h.botExchangeMu.Lock()
	defer h.botExchangeMu.Unlock()
	h.botExchangeCount[chatID] = 0
}

// shouldHandleGroupMessage decides if this bot should process a group chat message.
// Returns (should handle, cleaned text).
func (h *Handler) shouldHandleGroupMessage(msg *models.Message, text string) (bool, string) {
	// If no bot username configured, always handle (legacy single-bot mode).
	if h.botUsername == "" {
		return true, text
	}

	// Reply-to routing: if replying to this bot's message, always handle.
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
		if strings.ToLower(msg.ReplyToMessage.From.Username) == h.botUsername {
			return true, stripMention(text, h.botUsername)
		}
	}

	mentions := parseMentions(text)
	mentionsMe := false
	mentionsPeer := false
	for _, m := range mentions {
		if m == h.botUsername {
			mentionsMe = true
		}
		if h.peerBotUsernames[m] {
			mentionsPeer = true
		}
	}

	// Explicitly @mentioned this bot → handle it.
	if mentionsMe {
		return true, stripMention(text, h.botUsername)
	}

	// Message explicitly @mentions another bot (not this one) → skip.
	if mentionsPeer {
		return false, text
	}

	// Both agents named anywhere in the message → both respond. Must precede
	// the prefix-anchored addressed-to-peer skip below: "Pika and Umbreon, ..."
	// starts with the peer's name, so without this check the second-named agent
	// yields here and the both-named branch in RouteDecision never runs (this
	// exact gap ate an owner both-agents ask on 7/12).
	if namedIn(text, h.myAliases) && namedIn(text, h.peerAliases) {
		return true, text
	}

	// Name-based routing: check if message starts with a peer's name/alias.
	// e.g., "pika lunch memo" → skip for umbreonmini because "pika" is a peer alias.
	if h.messageAddressedToPeer(text) && !h.messageAddressedToMe(text) {
		slog.Info("group: yielding, message addressed to peer by name", "chat_id", msg.Chat.ID, "text_prefix", truncate(text, 30))
		return false, text
	}

	// Role-based domain routing (deterministic, both daemons agree): a message
	// that CLEARLY belongs to the other agent's domain is left to them; own-
	// domain and AMBIGUOUS messages are handled (so neither agent vanishes from
	// the chat). A2A (peer-bot) messages bypass routing entirely.
	if h.groupDomain != "" && !h.isPeerBot(msg) {
		handle, reason := RouteDecision(RouteInput{
			Text: text, MyAliases: h.myAliases, PeerAliases: h.peerAliases, MyDomain: h.groupDomain,
		})
		if !handle {
			slog.Info("group: yielding per role routing",
				"chat_id", msg.Chat.ID, "reason", reason, "domain", ClassifyGroupDomain(text), "mine", h.groupDomain)
			return false, text
		}
	}

	// Autonomous mode: deliver all non-@peer messages to the agent.
	// The agent decides whether to respond or output [noop].
	// Bot-to-bot exchange limits still apply to prevent infinite loops.
	if h.groupMode == "autonomous" {
		if h.isPeerBot(msg) {
			h.botExchangeMu.Lock()
			count := h.botExchangeCount[msg.Chat.ID]
			lastResp := h.botLastResponse[msg.Chat.ID]
			h.botExchangeMu.Unlock()

			if count >= botExchangeLimit {
				slog.Debug("autonomous: bot exchange limit reached", "chat_id", msg.Chat.ID, "count", count)
				return false, text
			}
			if time.Since(lastResp) < botCooldown {
				slog.Debug("autonomous: bot cooldown active", "chat_id", msg.Chat.ID)
				return false, text
			}
		} else {
			h.resetBotExchange(msg.Chat.ID)
		}
		return true, text
	}

	// Legacy mode: probability-based gating.
	if h.isPeerBot(msg) {
		h.botExchangeMu.Lock()
		count := h.botExchangeCount[msg.Chat.ID]
		lastResp := h.botLastResponse[msg.Chat.ID]
		h.botExchangeMu.Unlock()

		if count >= botExchangeLimit {
			slog.Debug("bot-to-bot exchange limit reached", "chat_id", msg.Chat.ID, "count", count)
			return false, text
		}
		if time.Since(lastResp) < botCooldown {
			slog.Debug("bot-to-bot cooldown active", "chat_id", msg.Chat.ID, "since", time.Since(lastResp))
			return false, text
		}
		return rand.Float64() < peerBroadcastProbability, text
	}

	h.resetBotExchange(msg.Chat.ID)

	if h.broadcastProbability <= 0 {
		return false, text
	}
	if h.broadcastProbability >= 1.0 {
		return true, text
	}
	return rand.Float64() < h.broadcastProbability, text
}

// checkAuth performs policy-based authorization for a message.
// Returns true if the sender is allowed to proceed.
func (h *Handler) checkAuth(ctx context.Context, b *bot.Bot, msg *models.Message) bool {
	isGroup := msg.Chat.Type == "group" || msg.Chat.Type == "supergroup"
	sender := SenderInfo{
		UserID:    msg.From.ID,
		Username:  msg.From.Username,
		FirstName: msg.From.FirstName,
		LastName:  msg.From.LastName,
		ChatID:    msg.Chat.ID,
		IsGroup:   isGroup,
	}

	result := h.auth.Check(sender)
	switch result {
	case AuthAllowed:
		return true
	case AuthPairing:
		if h.auth.Pairing == nil {
			return false
		}
		code, err := h.auth.Pairing.RequestPairing(sender.UserID, sender.Username, sender.FirstName, sender.LastName, sender.ChatID)
		if err != nil {
			slog.Error("pairing request failed", "error", err, "user_id", sender.UserID)
			return false
		}
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: msg.Chat.ID,
			Text:   fmt.Sprintf("🔐 Pairing required.\n\nYour code: %s\n\nAsk the admin to run:\n  shell pairing approve %s", code, code),
		})
		return false
	case AuthRateLimited:
		// Silent drop.
		return false
	case AuthDenied:
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    msg.Chat.ID,
			Text:      formatErrorForMarkdownV2("Unauthorized."),
			ParseMode: models.ParseModeMarkdown,
		})
		return false
	}
	return false
}

// checkReactionAuth performs policy-based authorization for a reaction.
func (h *Handler) checkReactionAuth(reaction *models.MessageReactionUpdated) bool {
	if reaction.User == nil {
		return false
	}
	isGroup := reaction.Chat.Type == "group" || reaction.Chat.Type == "supergroup"
	sender := SenderInfo{
		UserID:    reaction.User.ID,
		Username:  reaction.User.Username,
		FirstName: reaction.User.FirstName,
		LastName:  reaction.User.LastName,
		ChatID:    reaction.Chat.ID,
		IsGroup:   isGroup,
	}
	return h.auth.Check(sender) == AuthAllowed
}

// getChatLock returns the per-(chat,thread) mutex, creating one if needed.
// Each forum topic serializes independently so a long reply in one topic
// doesn't block another.
func (h *Handler) getChatLock(chatID, threadID int64) *sync.Mutex {
	key := chatLockKey{chatID: chatID, threadID: threadID}
	h.chatLocksMu.Lock()
	defer h.chatLocksMu.Unlock()
	mu, ok := h.chatLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		h.chatLocks[key] = mu
	}
	return mu
}

// msgThreadID returns the Telegram forum topic ID for a message (0 for main
// chat / General topic). We trust MessageThreadID directly — Telegram clients
// don't always populate IsTopicMessage (notably on reply-in-thread events and
// on some older supergroup configurations), so gating on that flag causes
// replies to fall back to the main chat. A non-zero MessageThreadID is
// authoritative: the message lives in that topic.
func msgThreadID(msg *models.Message) int64 {
	if msg == nil {
		return 0
	}
	return int64(msg.MessageThreadID)
}

// setReaction sets an emoji reaction on a message, replacing any previous reaction.
func setReaction(ctx context.Context, b *bot.Bot, chatID any, messageID int, emoji string) {
	_, err := b.SetMessageReaction(ctx, &bot.SetMessageReactionParams{
		ChatID:    chatID,
		MessageID: messageID,
		Reaction: []models.ReactionType{
			{
				Type:              models.ReactionTypeTypeEmoji,
				ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: emoji},
			},
		},
	})
	if err != nil {
		slog.Debug("failed to set reaction", "error", err, "emoji", emoji)
	}
}

// sendPhoto sends an image to a chat/topic as a Telegram photo message.
func sendPhoto(ctx context.Context, b *bot.Bot, chatID, threadID int64, imageData []byte, caption string) {
	_, err := b.SendPhoto(ctx, &bot.SendPhotoParams{
		ChatID:          chatID,
		MessageThreadID: int(threadID),
		Photo: &models.InputFileUpload{
			Filename: "image.png",
			Data:     bytes.NewReader(imageData),
		},
		Caption: caption,
	})
	if err != nil {
		slog.Error("failed to send photo", "error", err, "chat_id", chatID, "thread_id", threadID)
	}
}

// sendVideo sends a video to a chat/topic as a Telegram video message.
func sendVideo(ctx context.Context, b *bot.Bot, chatID, threadID int64, videoData []byte, caption string) {
	_, err := b.SendVideo(ctx, &bot.SendVideoParams{
		ChatID:          chatID,
		MessageThreadID: int(threadID),
		Video: &models.InputFileUpload{
			Filename: "video.mp4",
			Data:     bytes.NewReader(videoData),
		},
		Caption: caption,
	})
	if err != nil {
		slog.Error("failed to send video", "error", err, "chat_id", chatID, "thread_id", threadID)
	}
}

// looksLikeClarification checks if a response appears to be asking the user
// for clarification (i.e. it ends with a question mark).
func looksLikeClarification(response string) bool {
	trimmed := strings.TrimSpace(response)
	return strings.HasSuffix(trimmed, "?")
}

func (h *Handler) HandleReaction(ctx context.Context, b *bot.Bot, reaction *models.MessageReactionUpdated) {
	slog.Info("received message reaction",
		"chat_id", reaction.Chat.ID,
		"message_id", reaction.MessageID,
		"new_reaction", reaction.NewReaction,
		"old_reaction", reaction.OldReaction,
	)

	// Auth check: only process reactions from allowed users.
	if !h.checkReactionAuth(reaction) {
		return
	}

	// Extract the first emoji from new reactions.
	if len(reaction.NewReaction) == 0 {
		return
	}
	emoji := ""
	for _, r := range reaction.NewReaction {
		if r.ReactionTypeEmoji != nil {
			emoji = r.ReactionTypeEmoji.Emoji
			break
		}
	}
	if emoji == "" {
		return
	}

	chatID := reaction.Chat.ID
	// Reactions don't carry message_thread_id directly; look up via the
	// message_map entry (the bot response we mapped earlier remembers which
	// thread it belongs to via the session). For now reactions reply on the
	// main thread — Telegram's MessageReactionUpdated doesn't expose the
	// topic the reacted-to message lives in.
	var threadID int64 = 0
	if mm, err := h.bridge.GetMessageMapByBotMsg(chatID, reaction.MessageID); err == nil && mm != nil {
		// Fetching the session gets us the thread_id the exchange belongs to.
		threadID = h.bridge.SessionThreadID(chatID, mm.SessionID)
	}

	// Regenerate is handled specially: stream the new response into the
	// existing bot message instead of sending a separate reply.
	if h.bridge.ReactionAction(emoji) == "regenerate" {
		h.handleRegenerate(ctx, b, chatID, threadID, reaction.MessageID)
		return
	}

	response, err := h.bridge.HandleReaction(ctx, chatID, threadID, reaction.MessageID, emoji)
	if err != nil {
		slog.Error("bridge handle reaction failed", "error", err, "chat_id", chatID, "thread_id", threadID, "emoji", emoji)
		setReaction(ctx, b, chatID, reaction.MessageID, "❌")
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          chatID,
			MessageThreadID: int(threadID),
			Text:            formatErrorForMarkdownV2(err.Error()),
			ParseMode:       models.ParseModeMarkdown,
		})
		return
	}
	if response == "" {
		return
	}

	setReaction(ctx, b, chatID, reaction.MessageID, "✅")
	ids := h.sendChunked(ctx, b, chatID, threadID, response)
	action := h.bridge.ReactionAction(emoji)
	if (action == "remember" || action == "forget") && len(ids) > 0 {
		setReaction(ctx, b, chatID, ids[len(ids)-1], "✅")
	}
}

// handleRegenerate re-sends the original user message to Claude and streams
// the new response into the existing bot message, replacing its content.
func (h *Handler) handleRegenerate(ctx context.Context, b *bot.Bot, chatID, threadID int64, botMessageID int) {
	// Serialize with other messages for this (chat, thread).
	chatMu := h.getChatLock(chatID, threadID)
	if !chatMu.TryLock() {
		// Edit the target message to indicate it's queued.
		b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    chatID,
			MessageID: botMessageID,
			Text:      escapeMarkdownV2Text("Queued for regeneration..."),
			ParseMode: models.ParseModeMarkdown,
		})
		chatMu.Lock()
	}
	defer chatMu.Unlock()

	// Show a "Regenerating..." placeholder while Claude processes.
	b.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:    chatID,
		MessageID: botMessageID,
		Text:      escapeMarkdownV2Text("Regenerating..."),
		ParseMode: models.ParseModeMarkdown,
	})

	// Set up streaming state to throttle edits.
	var mu sync.Mutex
	var accumulated strings.Builder
	lastEdit := time.Time{}
	markdownFailed := false
	lastSentContent := ""
	lastUsedMarkdown := false

	onUpdate := func(chunk string) {
		mu.Lock()
		accumulated.WriteString(chunk)
		current := accumulated.String()
		now := time.Now()
		shouldEdit := now.Sub(lastEdit) >= streamEditInterval
		if shouldEdit {
			lastEdit = now
		}
		failed := markdownFailed
		mu.Unlock()

		if !shouldEdit {
			return
		}

		if !failed {
			formatted, ok := formatForTelegram(current, maxMessageLength)
			if ok {
				_, editErr := b.EditMessageText(ctx, &bot.EditMessageTextParams{
					ChatID:    chatID,
					MessageID: botMessageID,
					Text:      formatted,
					ParseMode: models.ParseModeMarkdown,
				})
				if editErr == nil {
					mu.Lock()
					lastSentContent = current
					lastUsedMarkdown = true
					mu.Unlock()
					return
				}
				slog.Debug("regenerate markdown edit failed, disabling", "error", editErr)
				mu.Lock()
				markdownFailed = true
				mu.Unlock()
			}
		}

		plain := current
		if telegramLen(plain) > maxMessageLength {
			plain = "..." + tailKeepTelegramLen(plain, maxMessageLength-3)
		}
		_, editErr := b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    chatID,
			MessageID: botMessageID,
			Text:      plain,
		})
		if editErr != nil {
			slog.Debug("failed to edit regenerate message", "error", editErr)
		} else {
			mu.Lock()
			lastSentContent = current
			lastUsedMarkdown = false
			mu.Unlock()
		}
	}

	resp, err := h.bridge.RegenerateStreaming(ctx, chatID, threadID, botMessageID, onUpdate)
	if err != nil {
		slog.Error("regenerate failed", "error", err, "chat_id", chatID)
		b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    chatID,
			MessageID: botMessageID,
			Text:      formatErrorForMarkdownV2(err.Error()),
			ParseMode: models.ParseModeMarkdown,
		})
		return
	}

	// Send any collected photos.
	for _, photo := range resp.Photos {
		sendPhoto(ctx, b, chatID, threadID, photo.Data, photo.Caption)
	}
	for _, video := range resp.Videos {
		sendVideo(ctx, b, chatID, threadID, video.Data, video.Caption)
	}

	response := resp.Text

	mu.Lock()
	streamedContent := lastSentContent
	streamedMarkdown := lastUsedMarkdown
	mu.Unlock()

	if response == "" && streamedContent != "" {
		response = streamedContent
	}
	if response == "" {
		response = "(empty response)"
	}

	setReaction(ctx, b, chatID, botMessageID, "✅")

	// Final edit with fully formatted response. Always reconcile (push the
	// complete text) rather than trusting that streaming already displayed it —
	// a throttled or dropped streaming edit can leave a stale/truncated view.
	// A benign "message is not modified" means the streamed view was already
	// correct; treat it as success so we don't fall back to plain and lose
	// markup. Lengths are in UTF-16 units (telegramLen) to match Telegram.
	_ = streamedContent
	_ = streamedMarkdown
	formatted := formatForMarkdownV2(response)

	if telegramLen(formatted) <= maxMessageLength {
		editErr := editFinal(ctx, b, &bot.EditMessageTextParams{
			ChatID:    chatID,
			MessageID: botMessageID,
			Text:      formatted,
			ParseMode: models.ParseModeMarkdown,
		})
		if editErr != nil && !isNotModified(editErr) {
			slog.Warn("final markdown edit failed, falling back to plain (regenerate)", "error", editErr, "chat_id", chatID, "msg_id", botMessageID)
			plainErr := editFinal(ctx, b, &bot.EditMessageTextParams{
				ChatID:    chatID,
				MessageID: botMessageID,
				Text:      response,
			})
			if plainErr != nil && !isNotModified(plainErr) {
				slog.Warn("final reconcile edit failed — streamed partial left on screen (regenerate)", "error", plainErr, "chat_id", chatID, "msg_id", botMessageID)
			}
		}
	} else if telegramLen(response) <= maxMessageLength {
		if editErr := editFinal(ctx, b, &bot.EditMessageTextParams{
			ChatID:    chatID,
			MessageID: botMessageID,
			Text:      response,
		}); editErr != nil && !isNotModified(editErr) {
			slog.Warn("final reconcile edit failed — streamed partial left on screen (regenerate)", "error", editErr, "chat_id", chatID, "msg_id", botMessageID)
		}
	} else {
		// Response too long — delete original and send chunked.
		b.DeleteMessage(ctx, &bot.DeleteMessageParams{
			ChatID:    chatID,
			MessageID: botMessageID,
		})
		h.sendChunked(ctx, b, chatID, threadID, response)
	}
}

func (h *Handler) HandleMessage(ctx context.Context, b *bot.Bot, msg *models.Message) {
	if msg.From == nil {
		return
	}
	if !h.checkAuth(ctx, b, msg) {
		return
	}

	threadID := msgThreadID(msg)
	if threadID != 0 || msg.IsTopicMessage {
		slog.Info("telegram: message in forum topic",
			"chat_id", msg.Chat.ID,
			"thread_id", threadID,
			"is_topic_message", msg.IsTopicMessage,
			"raw_thread_id", msg.MessageThreadID,
		)
	}
	text := strings.TrimSpace(msg.Text)

	// Group chat @mention filtering and broadcast probability.
	isGroup := msg.Chat.Type == "group" || msg.Chat.Type == "supergroup"
	senderIsPeerBot := false
	if isGroup {
		// Use caption as text source for mention parsing if text is empty (photo/sticker messages).
		mentionText := text
		if mentionText == "" {
			mentionText = strings.TrimSpace(msg.Caption)
		}
		shouldHandle, cleaned := h.shouldHandleGroupMessage(msg, mentionText)
		if !shouldHandle {
			return
		}
		senderIsPeerBot = h.isPeerBot(msg)
		// If text was cleaned (mention stripped), update it.
		if text != "" && cleaned != mentionText {
			text = cleaned
		}
	}

	// If this message contains a photo, document image, or PDF,
	// download it and pass the info to the bridge so it can augment the
	// Claude message with the file path and metadata.
	var images []bridge.ImageInfo
	var pdfs []bridge.PDFInfo
	if len(msg.Photo) > 0 {
		if text == "" {
			text = strings.TrimSpace(msg.Caption)
		}
		if text == "" {
			text = "(photo)"
		}
		img, err := DownloadPhoto(ctx, b, msg.Photo)
		if err != nil {
			slog.Error("failed to download photo", "error", err, "chat_id", msg.Chat.ID)
			setReaction(ctx, b, msg.Chat.ID, msg.ID, "❌")
			return
		}
		// V2-H19: archive before the turn (mutates img.Path to the persistent
		// dest). Only unarchived temps are cleaned up after the turn.
		h.bridge.ArchiveInboundMedia(msg.Chat.ID, threadID, msg.ID, strings.TrimSpace(msg.Caption), &img)
		if img.MediaID == 0 {
			defer func() { os.Remove(img.Path) }()
		}
		images = []bridge.ImageInfo{img}
	} else if msg.Sticker != nil {
		if text == "" {
			text = strings.TrimSpace(msg.Caption)
		}
		if text == "" {
			text = "(sticker)"
		}
		// Annotate animated/video stickers so Claude knows it's seeing a thumbnail.
		if msg.Sticker.IsAnimated {
			text += " [animated sticker]"
		} else if msg.Sticker.IsVideo {
			text += " [video sticker]"
		}
		// Append emoji and sticker set context so Claude can interpret the sticker.
		var ctxParts []string
		if msg.Sticker.Emoji != "" {
			ctxParts = append(ctxParts, "emoji: "+msg.Sticker.Emoji)
		}
		if msg.Sticker.SetName != "" {
			ctxParts = append(ctxParts, "set: "+msg.Sticker.SetName)
		}
		if len(ctxParts) > 0 {
			text += " [" + strings.Join(ctxParts, ", ") + "]"
		}
		// For animated/video stickers, download the thumbnail instead of the
		// full sticker file (which Claude can't interpret).
		if (msg.Sticker.IsAnimated || msg.Sticker.IsVideo) && msg.Sticker.Thumbnail != nil {
			img, err := DownloadPhoto(ctx, b, []models.PhotoSize{*msg.Sticker.Thumbnail})
			if err != nil {
				slog.Warn("failed to download sticker thumbnail, falling back to text-only", "error", err, "chat_id", msg.Chat.ID)
			} else {
				defer func() { os.Remove(img.Path) }()
				images = []bridge.ImageInfo{img}
			}
		} else if msg.Sticker.IsAnimated || msg.Sticker.IsVideo {
			// No thumbnail available; proceed with text-only context.
			slog.Info("animated/video sticker has no thumbnail, falling back to text-only", "chat_id", msg.Chat.ID)
		} else {
			img, err := DownloadSticker(ctx, b, msg.Sticker)
			if err != nil {
				slog.Warn("failed to download sticker", "error", err, "chat_id", msg.Chat.ID)
				// Try the thumbnail as a fallback before going text-only.
				if msg.Sticker.Thumbnail != nil {
					thumbImg, thumbErr := DownloadPhoto(ctx, b, []models.PhotoSize{*msg.Sticker.Thumbnail})
					if thumbErr != nil {
						slog.Warn("failed to download sticker thumbnail, falling back to text-only", "error", thumbErr, "chat_id", msg.Chat.ID)
					} else {
						defer func() { os.Remove(thumbImg.Path) }()
						images = []bridge.ImageInfo{thumbImg}
					}
				}
			} else {
				defer func() { os.Remove(img.Path) }()
				images = []bridge.ImageInfo{img}
			}
		}
	} else if IsImageDocument(msg.Document) {
		if text == "" {
			text = strings.TrimSpace(msg.Caption)
		}
		if text == "" {
			text = "(photo)"
		}
		img, err := DownloadDocument(ctx, b, msg.Document)
		if err != nil {
			slog.Error("failed to download document image", "error", err, "chat_id", msg.Chat.ID)
			setReaction(ctx, b, msg.Chat.ID, msg.ID, "❌")
			return
		}
		h.bridge.ArchiveInboundMedia(msg.Chat.ID, threadID, msg.ID, strings.TrimSpace(msg.Caption), &img)
		if img.MediaID == 0 {
			defer func() { os.Remove(img.Path) }()
		}
		images = []bridge.ImageInfo{img}
	} else if IsPDFDocument(msg.Document) {
		if text == "" {
			text = strings.TrimSpace(msg.Caption)
		}
		if text == "" {
			text = "(pdf)"
		}
		pdfInfo, err := DownloadPDF(ctx, b, msg.Document)
		if err != nil {
			slog.Error("failed to download pdf", "error", err, "chat_id", msg.Chat.ID)
			setReaction(ctx, b, msg.Chat.ID, msg.ID, "❌")
			return
		}
		defer func() { os.Remove(pdfInfo.Path) }()
		pdfs = []bridge.PDFInfo{{Path: pdfInfo.Path, Size: pdfInfo.Size}}
	}

	if text == "" {
		return
	}

	// Build sender display name for Claude to identify who is speaking.
	senderName := msg.From.FirstName
	if senderName == "" {
		senderName = msg.From.Username
	}

	// E2E clock (V2-H33): everything below is measured from handler entry so
	// the message_map row records what the OWNER experienced, not just the
	// model process.
	e2eStart := time.Now()
	var e2eRecvLagMs int64
	if msg.Date > 0 {
		if lag := time.Since(time.Unix(int64(msg.Date), 0)); lag > 0 {
			e2eRecvLagMs = lag.Milliseconds()
			if lag > 3*time.Second {
				slog.Info("turn: receipt lag", "chat_id", msg.Chat.ID, "lag_ms", e2eRecvLagMs)
			}
		}
	}

	// Instant receipt (V2-H33): react before any work so the unavoidable
	// model-side seconds read as "on it", not "did it hear me?".
	setReaction(ctx, b, msg.Chat.ID, msg.ID, "👀")

	// Serialize messages per (chat, thread) so concurrent sends in different
	// topics run in parallel and only same-topic messages queue.
	chatMu := h.getChatLock(msg.Chat.ID, threadID)
	e2ePreLock := time.Now()
	if !chatMu.TryLock() {
		setReaction(ctx, b, msg.Chat.ID, msg.ID, "🕐")
		chatMu.Lock()
	}
	e2eLockWaitMs := time.Since(e2ePreLock).Milliseconds()
	defer chatMu.Unlock()

	// React with 👀 to acknowledge receipt.
	setReaction(ctx, b, msg.Chat.ID, msg.ID, "👀")

	// Switch to ⏳ if processing takes a while.
	longRunning := time.AfterFunc(longRunningThreshold, func() {
		setReaction(ctx, b, msg.Chat.ID, msg.ID, "⏳")
	})
	defer longRunning.Stop()

	// Send an initial placeholder message that we'll edit with streaming updates.
	placeholder, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          msg.Chat.ID,
		MessageThreadID: int(threadID),
		Text:            escapeMarkdownV2Text("Thinking..."),
		ParseMode:       models.ParseModeMarkdown,
	})
	if err != nil {
		slog.Error("failed to send placeholder", "error", err, "chat_id", msg.Chat.ID, "thread_id", threadID)
		return
	}
	msgID := placeholder.ID

	// Update the placeholder with fun messages until the first chunk arrives.
	firstChunk := make(chan struct{})
	var firstChunkOnce sync.Once
	stopThinking := func() { firstChunkOnce.Do(func() { close(firstChunk) }) }
	defer stopThinking()
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		tick := 0
		for {
			select {
			case <-firstChunk:
				return
			case <-ticker.C:
				tick++
				text := thinkingMessage(tick)
				b.EditMessageText(ctx, &bot.EditMessageTextParams{
					ChatID:    msg.Chat.ID,
					MessageID: msgID,
					Text:      text,
				})
			}
		}
	}()

	// Set up streaming state: accumulate text and throttle edits.
	var mu sync.Mutex
	var accumulated strings.Builder
	markdownFailed := false   // set when MarkdownV2 is rejected during streaming
	lastSentContent := ""     // raw text of last successful streaming edit
	lastUsedMarkdown := false // whether last streaming edit used MarkdownV2
	var e2eFirstVisible time.Time // when the owner first saw real content

	// Streaming edit loop: a separate goroutine periodically flushes
	// accumulated text to Telegram. This keeps onUpdate non-blocking
	// so the stdout scanner isn't stalled by Telegram API latency.
	dirty := make(chan struct{}, 1) // signal that new text is available
	editDone := make(chan struct{})
	go func() {
		defer close(editDone)
		firstFlush := true
		for range dirty {
			// Throttle: wait for the edit interval — EXCEPT the first flush,
			// which goes out immediately so first-visible = TTFT + one edit
			// call instead of + up to a full throttle interval (V2-H33).
			if !firstFlush {
				time.Sleep(streamEditInterval)
			}
			firstFlush = false

			mu.Lock()
			current := accumulated.String()
			failed := markdownFailed
			mu.Unlock()

			if current == lastSentContent {
				continue
			}

			if !failed {
				formatted, ok := formatForTelegram(current, maxMessageLength)
				if ok {
					_, editErr := b.EditMessageText(ctx, &bot.EditMessageTextParams{
						ChatID:    msg.Chat.ID,
						MessageID: msgID,
						Text:      formatted,
						ParseMode: models.ParseModeMarkdown,
					})
					if editErr == nil {
						lastSentContent = current
						lastUsedMarkdown = true
						mu.Lock()
						if e2eFirstVisible.IsZero() {
							e2eFirstVisible = time.Now()
						}
						mu.Unlock()
						continue
					}
					slog.Debug("streaming markdown edit failed, disabling for remaining edits", "error", editErr)
					mu.Lock()
					markdownFailed = true
					mu.Unlock()
				}
			}

			// Fallback: send without formatting.
			plain := current
			if telegramLen(plain) > maxMessageLength {
				plain = "..." + tailKeepTelegramLen(plain, maxMessageLength-3)
			}
			_, editErr := b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:    msg.Chat.ID,
				MessageID: msgID,
				Text:      plain,
			})
			if editErr != nil {
				slog.Debug("failed to edit streaming message", "error", editErr)
			} else {
				lastSentContent = current
				lastUsedMarkdown = false
				mu.Lock()
				if e2eFirstVisible.IsZero() {
					e2eFirstVisible = time.Now()
				}
				mu.Unlock()
			}
		}
	}()

	onUpdate := func(chunk string) {
		stopThinking()
		mu.Lock()
		accumulated.WriteString(chunk)
		mu.Unlock()

		// Signal the edit goroutine (non-blocking).
		select {
		case dirty <- struct{}{}:
		default:
		}
	}

	// Detach the turn from the poller context: during a drain-restart the
	// poller ctx is cancelled to stop new updates, but in-flight turns must
	// run to completion (the whole point of draining).
	turnCtx := context.WithoutCancel(ctx)
	resp, err := h.bridge.HandleMessageStreaming(turnCtx, msg.Chat.ID, threadID, text, senderName, images, pdfs, onUpdate)

	// Stop the streaming edit goroutine and wait for it to finish.
	// Send one final signal so the goroutine flushes any remaining text
	// before we close the channel.
	select {
	case dirty <- struct{}{}:
	default:
	}
	close(dirty)
	<-editDone

	if err != nil {
		slog.Error("bridge handle message failed", "error", err, "chat_id", msg.Chat.ID)
		setReaction(ctx, b, msg.Chat.ID, msg.ID, "❌")
		// Prefer a friendly retryable message for timeouts/cancellations
		// (sent as plain text — it contains () and — that MarkdownV2 rejects);
		// fall back to the escaped raw error for genuine faults.
		if friendly := friendlyTurnError(err); friendly != "" {
			b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:    msg.Chat.ID,
				MessageID: msgID,
				Text:      friendly,
			})
		} else {
			b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:    msg.Chat.ID,
				MessageID: msgID,
				Text:      formatErrorForMarkdownV2(err.Error()),
				ParseMode: models.ParseModeMarkdown,
			})
		}
		return
	}

	// Track bot-to-bot exchange if we just responded to a peer bot.
	if senderIsPeerBot {
		h.recordBotExchange(msg.Chat.ID)
	}

	response := resp.Text

	// Autonomous group noop: agent decided not to speak — suppress silently.
	if isGroup && h.groupMode == "autonomous" && response == "" && len(resp.Photos) == 0 && len(resp.Videos) == 0 {
		slog.Info("autonomous noop: agent chose not to speak", "chat_id", msg.Chat.ID, "bot", h.botUsername)
		b.DeleteMessage(ctx, &bot.DeleteMessageParams{
			ChatID:    msg.Chat.ID,
			MessageID: msgID,
		})
		return
	}

	// Send any collected photos (generated images, artifacts).
	for _, photo := range resp.Photos {
		sendPhoto(ctx, b, msg.Chat.ID, threadID, photo.Data, photo.Caption)
	}
	for _, video := range resp.Videos {
		sendVideo(ctx, b, msg.Chat.ID, threadID, video.Data, video.Caption)
	}

	// If final response is empty but we already streamed content to the user,
	// keep what was displayed rather than overwriting with "(empty response)".
	mu.Lock()
	streamedContent := lastSentContent
	streamedMarkdown := lastUsedMarkdown
	mu.Unlock()

	if response == "" && streamedContent != "" {
		response = streamedContent
	}
	if response == "" {
		response = "(empty response)"
	}

	// Final response: always reconcile by pushing the complete formatted text
	// (a throttled/dropped streaming edit can leave a stale, truncated-looking
	// view). A benign "message is not modified" means streaming already showed
	// it correctly — treat as success so we don't fall back to plain and lose
	// markup. Lengths in UTF-16 units (telegramLen) to match Telegram's limit.
	_ = streamedContent
	_ = streamedMarkdown
	formatted := formatForMarkdownV2(response)

	// Track which bot message IDs correspond to this exchange.
	var botMsgIDs []int

	if telegramLen(formatted) <= maxMessageLength {
		editErr := editFinal(ctx, b, &bot.EditMessageTextParams{
			ChatID:    msg.Chat.ID,
			MessageID: msgID,
			Text:      formatted,
			ParseMode: models.ParseModeMarkdown,
		})
		if editErr != nil && !isNotModified(editErr) {
			// Fallback: try without markdown formatting
			slog.Warn("final markdown edit failed, falling back to plain", "error", editErr, "chat_id", msg.Chat.ID, "msg_id", msgID)
			setReaction(ctx, b, msg.Chat.ID, msg.ID, "🔄")
			plainErr := editFinal(ctx, b, &bot.EditMessageTextParams{
				ChatID:    msg.Chat.ID,
				MessageID: msgID,
				Text:      response,
			})
			if plainErr != nil && !isNotModified(plainErr) {
				slog.Warn("final reconcile edit failed — streamed partial left on screen", "error", plainErr, "chat_id", msg.Chat.ID, "msg_id", msgID)
			}
		}
		botMsgIDs = []int{msgID}
	} else if telegramLen(response) <= maxMessageLength {
		// Formatted text exceeds the limit but raw text fits — send unformatted.
		if editErr := editFinal(ctx, b, &bot.EditMessageTextParams{
			ChatID:    msg.Chat.ID,
			MessageID: msgID,
			Text:      response,
		}); editErr != nil && !isNotModified(editErr) {
			slog.Warn("final reconcile edit failed — streamed partial left on screen", "error", editErr, "chat_id", msg.Chat.ID, "msg_id", msgID)
		}
		botMsgIDs = []int{msgID}
	} else {
		// Delete placeholder and send chunked formatted response.
		_, delErr := b.DeleteMessage(ctx, &bot.DeleteMessageParams{
			ChatID:    msg.Chat.ID,
			MessageID: msgID,
		})
		if delErr != nil {
			slog.Warn("failed to delete placeholder before chunked send", "error", delErr, "chat_id", msg.Chat.ID, "msg_id", msgID)
		}
		botMsgIDs = h.sendChunked(ctx, b, msg.Chat.ID, threadID, response)
	}

	// Persist message-to-response mapping so reactions can target specific exchanges.
	for _, botID := range botMsgIDs {
		if err := h.bridge.SaveMessageMap(msg.Chat.ID, threadID, msg.ID, botID, text, response); err != nil {
			slog.Warn("failed to save message map", "error", err, "chat_id", msg.Chat.ID, "thread_id", threadID)
		}
	}

	// E2E stamp (V2-H33): what the owner experienced, handler-relative.
	// first_visible falls back to final-delivery when nothing streamed.
	mu.Lock()
	fv := e2eFirstVisible
	mu.Unlock()
	e2eTotalMs := time.Since(e2eStart).Milliseconds()
	e2eFirstVisibleMs := e2eTotalMs
	if !fv.IsZero() {
		e2eFirstVisibleMs = fv.Sub(e2eStart).Milliseconds()
	}
	h.bridge.SaveTurnE2E(msg.Chat.ID, msg.ID, e2eRecvLagMs, e2eLockWaitMs, e2eFirstVisibleMs, e2eTotalMs)
	slog.Info("turn: e2e",
		"chat_id", msg.Chat.ID,
		"recv_lag_ms", e2eRecvLagMs,
		"lock_wait_ms", e2eLockWaitMs,
		"first_visible_s", float64(e2eFirstVisibleMs)/1000,
		"total_s", float64(e2eTotalMs)/1000)

	// Pick a finishing reaction: 🤔 when Claude is asking for clarification, ✅ otherwise.
	finalEmoji := "✅"
	if looksLikeClarification(response) {
		finalEmoji = "🤔"
	}
	setReaction(ctx, b, msg.Chat.ID, msg.ID, finalEmoji)
}

// HandleSticker processes an incoming sticker message by downloading it (or its
// thumbnail for animated/video stickers), converting to PNG, and delegating to
// HandleMessage with emoji/set-name context.
func (h *Handler) HandleSticker(ctx context.Context, b *bot.Bot, msg *models.Message) {
	h.HandleMessage(ctx, b, msg)
}

// HandlePDF processes an incoming PDF document message by delegating to
// HandleMessage, which will detect the PDF document and download it.
func (h *Handler) HandlePDF(ctx context.Context, b *bot.Bot, msg *models.Message) {
	h.HandleMessage(ctx, b, msg)
}

// formatBytes formats a byte count as a human-readable string.
func formatBytes(bytes int64) string {
	switch {
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// HandlePhoto processes an incoming photo message. If the message belongs to a
// media group (album), it buffers the message and waits for more photos before
// processing them together. Single photos are delegated to HandleMessage.
func (h *Handler) HandlePhoto(ctx context.Context, b *bot.Bot, msg *models.Message) {
	if msg.MediaGroupID == "" {
		h.HandleMessage(ctx, b, msg)
		return
	}

	h.albumsMu.Lock()
	entry, ok := h.albums[msg.MediaGroupID]
	if !ok {
		entry = &albumEntry{}
		h.albums[msg.MediaGroupID] = entry
	}
	entry.messages = append(entry.messages, msg)

	// Reset (or start) the debounce timer. When it fires we process the
	// complete album. We capture the bot pointer and a background context
	// so the timer goroutine doesn't depend on the per-request context.
	if entry.timer != nil {
		entry.timer.Stop()
	}
	groupID := msg.MediaGroupID
	entry.timer = time.AfterFunc(albumCollectDelay, func() {
		h.processAlbum(context.Background(), b, groupID)
	})
	h.albumsMu.Unlock()
}

// processAlbum downloads all photos in a buffered album and sends them to
// Claude as a single message with multiple attached images.
func (h *Handler) processAlbum(ctx context.Context, b *bot.Bot, groupID string) {
	h.albumsMu.Lock()
	entry, ok := h.albums[groupID]
	if !ok {
		h.albumsMu.Unlock()
		return
	}
	messages := entry.messages
	delete(h.albums, groupID)
	h.albumsMu.Unlock()

	if len(messages) == 0 {
		return
	}

	// Use the first message for metadata (chat, sender, auth).
	first := messages[0]
	if first.From == nil {
		return
	}
	if !h.checkAuth(ctx, b, first) {
		return
	}

	// Collect caption: use the first non-empty caption found.
	text := ""
	for _, m := range messages {
		if c := strings.TrimSpace(m.Caption); c != "" {
			text = c
			break
		}
	}
	if text == "" {
		text = fmt.Sprintf("(%d photos)", len(messages))
	}

	// Download all photos.
	var images []bridge.ImageInfo
	for _, m := range messages {
		if len(m.Photo) == 0 {
			continue
		}
		img, err := DownloadPhoto(ctx, b, m.Photo)
		if err != nil {
			slog.Error("failed to download album photo", "error", err, "chat_id", first.Chat.ID)
			continue
		}
		h.bridge.ArchiveInboundMedia(first.Chat.ID, msgThreadID(first), m.ID, strings.TrimSpace(m.Caption), &img)
		images = append(images, img)
	}
	defer func() {
		for _, img := range images {
			if img.MediaID == 0 {
				os.Remove(img.Path)
			}
		}
	}()

	if len(images) == 0 {
		slog.Error("no photos downloaded from album", "chat_id", first.Chat.ID)
		setReaction(ctx, b, first.Chat.ID, first.ID, "❌")
		return
	}

	senderName := first.From.FirstName
	if senderName == "" {
		senderName = first.From.Username
	}

	threadID := msgThreadID(first)

	// Serialize messages per (chat, thread) so album replies queue per-topic.
	chatMu := h.getChatLock(first.Chat.ID, threadID)
	if !chatMu.TryLock() {
		setReaction(ctx, b, first.Chat.ID, first.ID, "🕐")
		chatMu.Lock()
	}
	defer chatMu.Unlock()

	// React with 👀 on the first message to acknowledge receipt.
	setReaction(ctx, b, first.Chat.ID, first.ID, "👀")

	// Switch to ⏳ if processing takes a while.
	longRunning := time.AfterFunc(longRunningThreshold, func() {
		setReaction(ctx, b, first.Chat.ID, first.ID, "⏳")
	})
	defer longRunning.Stop()

	// Send an initial placeholder.
	placeholder, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          first.Chat.ID,
		MessageThreadID: int(threadID),
		Text:            escapeMarkdownV2Text("Thinking..."),
		ParseMode:       models.ParseModeMarkdown,
	})
	if err != nil {
		slog.Error("failed to send placeholder (album)", "error", err, "chat_id", first.Chat.ID, "thread_id", threadID)
		return
	}
	msgID := placeholder.ID

	// Update the placeholder with elapsed time until the first chunk arrives.
	thinkingStart := time.Now()
	firstChunk := make(chan struct{})
	var firstChunkOnce sync.Once
	stopThinking := func() { firstChunkOnce.Do(func() { close(firstChunk) }) }
	defer stopThinking()
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-firstChunk:
				return
			case t := <-ticker.C:
				elapsed := int(t.Sub(thinkingStart).Seconds())
				txt := fmt.Sprintf("Thinking\\.\\.\\. \\(%ds\\)", elapsed)
				b.EditMessageText(ctx, &bot.EditMessageTextParams{
					ChatID:    first.Chat.ID,
					MessageID: msgID,
					Text:      txt,
					ParseMode: models.ParseModeMarkdown,
				})
			}
		}
	}()

	// Set up streaming state.
	var mu sync.Mutex
	var accumulated strings.Builder
	lastEdit := time.Time{}
	markdownFailed := false
	lastSentContent := ""
	lastUsedMarkdown := false

	onUpdate := func(chunk string) {
		stopThinking()
		mu.Lock()
		accumulated.WriteString(chunk)
		current := accumulated.String()
		now := time.Now()
		shouldEdit := now.Sub(lastEdit) >= streamEditInterval
		if shouldEdit {
			lastEdit = now
		}
		failed := markdownFailed
		mu.Unlock()

		if !shouldEdit {
			return
		}

		if !failed {
			formatted, ok := formatForTelegram(current, maxMessageLength)
			if ok {
				_, editErr := b.EditMessageText(ctx, &bot.EditMessageTextParams{
					ChatID:    first.Chat.ID,
					MessageID: msgID,
					Text:      formatted,
					ParseMode: models.ParseModeMarkdown,
				})
				if editErr == nil {
					mu.Lock()
					lastSentContent = current
					lastUsedMarkdown = true
					mu.Unlock()
					return
				}
				slog.Debug("streaming markdown edit failed (album)", "error", editErr)
				mu.Lock()
				markdownFailed = true
				mu.Unlock()
			}
		}

		plain := current
		if telegramLen(plain) > maxMessageLength {
			plain = "..." + tailKeepTelegramLen(plain, maxMessageLength-3)
		}
		_, editErr := b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    first.Chat.ID,
			MessageID: msgID,
			Text:      plain,
		})
		if editErr != nil {
			slog.Debug("failed to edit streaming message (album)", "error", editErr)
		} else {
			mu.Lock()
			lastSentContent = current
			lastUsedMarkdown = false
			mu.Unlock()
		}
	}

	turnCtx := context.WithoutCancel(ctx) // survive poller-ctx cancel during drain-restart
	resp, err := h.bridge.HandleMessageStreaming(turnCtx, first.Chat.ID, threadID, text, senderName, images, nil, onUpdate)
	if err != nil {
		slog.Error("bridge handle message failed (album)", "error", err, "chat_id", first.Chat.ID, "thread_id", threadID)
		setReaction(ctx, b, first.Chat.ID, first.ID, "❌")
		b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    first.Chat.ID,
			MessageID: msgID,
			Text:      formatErrorForMarkdownV2(err.Error()),
			ParseMode: models.ParseModeMarkdown,
		})
		return
	}

	// Send any collected photos.
	for _, photo := range resp.Photos {
		sendPhoto(ctx, b, first.Chat.ID, threadID, photo.Data, photo.Caption)
	}
	for _, video := range resp.Videos {
		sendVideo(ctx, b, first.Chat.ID, threadID, video.Data, video.Caption)
	}

	response := resp.Text

	mu.Lock()
	streamedContent := lastSentContent
	streamedMarkdown := lastUsedMarkdown
	mu.Unlock()

	if response == "" && streamedContent != "" {
		response = streamedContent
	}
	if response == "" {
		response = "(empty response)"
	}

	_ = streamedContent
	_ = streamedMarkdown
	formatted := formatForMarkdownV2(response)

	var botMsgIDs []int

	if telegramLen(formatted) <= maxMessageLength {
		editErr := editFinal(ctx, b, &bot.EditMessageTextParams{
			ChatID:    first.Chat.ID,
			MessageID: msgID,
			Text:      formatted,
			ParseMode: models.ParseModeMarkdown,
		})
		if editErr != nil && !isNotModified(editErr) {
			slog.Warn("final markdown edit failed, falling back to plain (album)", "error", editErr, "chat_id", first.Chat.ID, "msg_id", msgID)
			setReaction(ctx, b, first.Chat.ID, first.ID, "🔄")
			plainErr := editFinal(ctx, b, &bot.EditMessageTextParams{
				ChatID:    first.Chat.ID,
				MessageID: msgID,
				Text:      response,
			})
			if plainErr != nil && !isNotModified(plainErr) {
				slog.Warn("final reconcile edit failed — streamed partial left on screen (album)", "error", plainErr, "chat_id", first.Chat.ID, "msg_id", msgID)
			}
		}
		botMsgIDs = []int{msgID}
	} else if telegramLen(response) <= maxMessageLength {
		if editErr := editFinal(ctx, b, &bot.EditMessageTextParams{
			ChatID:    first.Chat.ID,
			MessageID: msgID,
			Text:      response,
		}); editErr != nil && !isNotModified(editErr) {
			slog.Warn("final reconcile edit failed — streamed partial left on screen (album)", "error", editErr, "chat_id", first.Chat.ID, "msg_id", msgID)
		}
		botMsgIDs = []int{msgID}
	} else {
		_, delErr := b.DeleteMessage(ctx, &bot.DeleteMessageParams{
			ChatID:    first.Chat.ID,
			MessageID: msgID,
		})
		if delErr != nil {
			slog.Warn("failed to delete placeholder before chunked send (album)", "error", delErr, "chat_id", first.Chat.ID, "msg_id", msgID)
		}
		botMsgIDs = h.sendChunked(ctx, b, first.Chat.ID, threadID, response)
	}

	// Map all album messages to the bot response for reaction support.
	for _, m := range messages {
		for _, botID := range botMsgIDs {
			if err := h.bridge.SaveMessageMap(first.Chat.ID, threadID, m.ID, botID, text, response); err != nil {
				slog.Warn("failed to save message map (album)", "error", err, "chat_id", first.Chat.ID, "thread_id", threadID)
			}
		}
	}

	finalEmoji := "✅"
	if looksLikeClarification(response) {
		finalEmoji = "🤔"
	}
	setReaction(ctx, b, first.Chat.ID, first.ID, finalEmoji)
}

func (h *Handler) HandleCommand(ctx context.Context, b *bot.Bot, msg *models.Message) {
	if msg.From == nil {
		return
	}
	if !h.checkAuth(ctx, b, msg) {
		return
	}

	threadID := msgThreadID(msg)

	parts := strings.SplitN(msg.Text, " ", 2)
	cmd := strings.TrimPrefix(parts[0], "/")
	// Strip bot username suffix (e.g., /start@mybotname)
	if idx := strings.Index(cmd, "@"); idx != -1 {
		cmd = cmd[:idx]
	}

	args := ""
	if len(parts) > 1 {
		args = parts[1]
	}

	response, err := h.bridge.HandleCommand(ctx, msg.Chat.ID, threadID, cmd, args)
	if err != nil {
		slog.Error("bridge handle command failed", "error", err, "cmd", cmd)
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          msg.Chat.ID,
			MessageThreadID: int(threadID),
			Text:            formatErrorForMarkdownV2(err.Error()),
			ParseMode:       models.ParseModeMarkdown,
		})
		return
	}

	ids := h.sendChunked(ctx, b, msg.Chat.ID, threadID, response)
	if (cmd == "remember" || cmd == "forget") && len(ids) > 0 {
		setReaction(ctx, b, msg.Chat.ID, ids[len(ids)-1], "✅")
	}
}

// sendChunked splits long messages at paragraph boundaries and sends them
// sequentially to the given chat/topic. Returns the Telegram message IDs of
// the sent messages.
func (h *Handler) sendChunked(ctx context.Context, b *bot.Bot, chatID, threadID int64, text string) []int {
	if text == "" {
		text = "(empty response)"
	}

	chunks := splitMessage(text, maxMessageLength)
	var ids []int
	for _, chunk := range chunks {
		sent, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          chatID,
			MessageThreadID: int(threadID),
			Text:            formatForMarkdownV2(chunk),
			ParseMode:       models.ParseModeMarkdown,
		})
		if err != nil {
			slog.Warn("MarkdownV2 send failed, retrying as plain text", "error", err, "chat_id", chatID, "thread_id", threadID)
			sent, err = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:          chatID,
				MessageThreadID: int(threadID),
				Text:            chunk,
			})
			if err != nil {
				slog.Error("failed to send message", "error", err, "chat_id", chatID, "thread_id", threadID)
			}
		}
		if sent != nil {
			ids = append(ids, sent.ID)
		}
	}
	return ids
}

// codeBlockStart returns the start index of the fenced code block (```)
// containing pos, or -1 if pos is not inside a fenced code block.
func codeBlockStart(text string, pos int) int {
	i := 0
	for {
		idx := strings.Index(text[i:], "```")
		if idx == -1 || i+idx > pos {
			return -1
		}
		start := i + idx
		closeIdx := strings.Index(text[start+3:], "```")
		if closeIdx == -1 {
			// Unclosed code block extends to end of text.
			return start
		}
		end := start + 3 + closeIdx + 3
		if pos < end {
			return start
		}
		i = end
	}
}

// splitMessage splits text into chunks such that each chunk, after being
// formatted with formatForMarkdownV2, fits within maxLen bytes. It prefers
// to split at paragraph boundaries (\n\n), then line boundaries (\n).
func splitMessage(text string, maxLen int) []string {
	if telegramLen(formatForMarkdownV2(text)) <= maxLen {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if telegramLen(formatForMarkdownV2(text)) <= maxLen {
			chunks = append(chunks, text)
			break
		}

		// Find the largest raw prefix whose formatted length (in UTF-16 units,
		// matching Telegram's limit) fits in maxLen. Start at the byte offset
		// for maxLen units, then shrink proportionally.
		end := byteEndForTelegramBudget(text, maxLen)
		for end > 1 {
			fmtLen := telegramLen(formatForMarkdownV2(text[:end]))
			if fmtLen <= maxLen {
				break
			}
			// Shrink proportionally with guaranteed progress.
			newEnd := end * maxLen / fmtLen
			if newEnd >= end {
				newEnd = end - 1
			}
			if newEnd < 1 {
				newEnd = 1
			}
			end = newEnd
		}
		// Snap end down to a rune boundary so a hard split never cuts a
		// multi-byte (CJK/emoji) character into invalid UTF-8.
		for end > 0 && end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
		if end < 1 {
			end = 1
		}

		// Try to split at a nice boundary within [0, end].
		chunk := text[:end]
		splitIdx := strings.LastIndex(chunk, "\n\n")
		if splitIdx == -1 {
			// Try line boundary
			splitIdx = strings.LastIndex(chunk, "\n")
		}
		if splitIdx == -1 {
			// Try space
			splitIdx = strings.LastIndex(chunk, " ")
		}
		if splitIdx == -1 {
			// Hard split
			splitIdx = end - 1
		}

		// Avoid splitting inside a fenced code block.
		if cbStart := codeBlockStart(text, splitIdx); cbStart >= 0 {
			if cbStart > 0 {
				splitIdx = cbStart - 1
			} else {
				// Code block starts at position 0. Include the entire
				// block if it fits within the limit.
				closeIdx := strings.Index(text[3:], "```")
				if closeIdx != -1 {
					cbEnd := 3 + closeIdx + 2
					if len(formatForMarkdownV2(text[:cbEnd+1])) <= maxLen {
						splitIdx = cbEnd
					}
				}
			}
		}

		chunks = append(chunks, text[:splitIdx+1])
		text = text[splitIdx+1:]
	}
	return chunks
}

func (h *Handler) sendTypingPeriodically(ctx context.Context, b *bot.Bot, chatID, threadID int64) {
	// Send immediately
	b.SendChatAction(ctx, &bot.SendChatActionParams{
		ChatID:          chatID,
		MessageThreadID: int(threadID),
		Action:          models.ChatActionTyping,
	})

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.SendChatAction(ctx, &bot.SendChatActionParams{
				ChatID:          chatID,
				MessageThreadID: int(threadID),
				Action:          models.ChatActionTyping,
			})
		}
	}
}
