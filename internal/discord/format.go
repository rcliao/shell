package discord

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// maxMessageLen is Discord's limit on message content, in characters.
const maxMessageLen = 2000

// fenceReserve is room kept free in each chunk for the fence lines added
// when a split lands inside a code block.
const fenceReserve = 24

// fenceLangRe is what a code-fence language tag looks like: go, c++, shell-session.
var fenceLangRe = regexp.MustCompile(`^[A-Za-z0-9_+#.-]{0,20}$`)

// splitMessage cuts text into chunks of at most max characters.
//
// Discord renders Markdown as written, so, unlike Telegram, nothing is escaped;
// what matters is where the cuts land. Paragraph breaks are preferred, then
// line breaks, then spaces. A cut inside a fenced code block closes the fence
// at the end of the chunk and reopens it (with its language) at the start of
// the next, so the code renders as code on both sides — at 2,000 characters a
// long code answer is split far more often than on Telegram.
func splitMessage(text string, max int) []string {
	if utf8.RuneCountInString(text) <= max {
		return []string{text}
	}
	var chunks []string
	reopen := ""
	for text != "" {
		budget := max - utf8.RuneCountInString(reopen)
		if utf8.RuneCountInString(text) <= budget {
			chunks = append(chunks, reopen+text)
			break
		}
		budget -= fenceReserve
		if budget < max/2 {
			budget = max / 2 // never let the reopened fence starve a chunk
		}
		end := byteIndexOfRune(text, budget)
		cut := bestCut(text[:end])
		if cut == 0 {
			cut = end // guaranteed progress
		}
		chunk, rest := text[:cut], text[cut:]

		body := reopen + chunk
		reopen = ""
		if lang, open := openFence(body); open {
			body = strings.TrimRight(body, "\n") + "\n```"
			reopen = "```" + lang + "\n"
			rest = strings.TrimLeft(rest, "\n")
		}
		chunks = append(chunks, body)
		text = rest
	}
	return chunks
}

// bestCut returns where to end a chunk within s: after the last paragraph
// break, else line break, else space — but only when that keeps at least half
// the chunk, so one early blank line doesn't produce a stub.
func bestCut(s string) int {
	half := len(s) / 2
	for _, sep := range []string{"\n\n", "\n", " "} {
		if i := strings.LastIndex(s, sep); i >= half {
			return i + len(sep)
		}
	}
	return len(s)
}

// openFence reports whether s ends inside a fenced code block, and the
// language tag of that block.
func openFence(s string) (lang string, open bool) {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "```") {
			continue
		}
		if open {
			open, lang = false, ""
			continue
		}
		open = true
		lang = strings.TrimSpace(strings.TrimPrefix(t, "```"))
		if strings.Contains(lang, "```") { // a one-line ```code``` span
			open, lang = false, ""
		}
		if !fenceLangRe.MatchString(lang) {
			// Only a real language tag is carried to the reopened fence. A
			// long first line after ``` (code, not a tag) would otherwise be
			// repeated at the top of every chunk.
			lang = ""
		}
	}
	return lang, open
}

// fenceTables wraps Markdown tables in a code block. Discord does not render
// tables, so a model's | a | b | table arrives as a wall of pipes; in a
// code block the columns at least line up. Tables already inside a fence are
// left alone.
func fenceTables(text string) string {
	lines := strings.Split(text, "\n")
	var out []string
	inFence, inTable := false, false
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			inFence = !inFence
		}
		isRow := !inFence && strings.HasPrefix(t, "|") && strings.Count(t, "|") >= 2
		if isRow && !inTable {
			out = append(out, "```")
			inTable = true
		} else if !isRow && inTable {
			out = append(out, "```")
			inTable = false
		}
		out = append(out, line)
	}
	if inTable {
		out = append(out, "```")
	}
	return strings.Join(out, "\n")
}

// streamView is what a streaming edit shows: the text so far, or its tail
// when it has outgrown one message. The final reply is chunked separately.
func streamView(text string, max int) string {
	n := utf8.RuneCountInString(text)
	if n <= max {
		return text
	}
	keep := max - 1
	return "…" + text[byteIndexOfRune(text, n-keep):]
}

// byteIndexOfRune returns the byte offset of the n-th rune in s (len(s) when
// s is shorter), so cuts never split a multi-byte character.
func byteIndexOfRune(s string, n int) int {
	if n <= 0 {
		return 0
	}
	i := 0
	for pos := range s {
		if i == n {
			return pos
		}
		i++
	}
	return len(s)
}
