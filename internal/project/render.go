// Markdown → Notion blocks converter (P3 Wave C, plan C2). Deliberately
// fine-grained: block-level comment anchoring (Wave D) needs one block per
// option/section element, so every bullet, heading, and paragraph becomes its
// own block. The inline converter is minimal on purpose — bold and links
// only; everything else renders as plain text.
package project

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// preambleSection is the block-map key for content between the `# ` title and
// the first `## ` heading. A literal doc heading cannot produce it — section
// keys come from `## ` lines, and this name is not a plausible one.
const preambleSection = "_preamble"

// DocSection is one `## ` heading plus everything until the next `## `.
type DocSection struct {
	Title  string // the `## ` heading text (map key); preambleSection for the preamble
	Hash   string // content hash of the section's raw markdown — the diff unit
	Blocks []NotionBlock
}

// sectionHash fingerprints a section's raw markdown. The SOURCE is hashed,
// not the converted blocks, so a section re-renders only when its content
// actually changes — the cheap, correct default for the surgical diff.
func sectionHash(raw []string) string {
	sum := sha256.Sum256([]byte(strings.Join(raw, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}

// ParseDoc converts a canonical doc into its page title and ordered sections.
// The FIRST `# ` line is the page-title context (it becomes the Notion page
// title, not a body block); every other line lands in a section.
func ParseDoc(md string) (title string, sections []DocSection) {
	lines := strings.Split(md, "\n")

	var cur *docSectionAccum
	flush := func() {
		if cur == nil {
			return
		}
		if s, ok := cur.finish(); ok {
			sections = append(sections, s)
		}
		cur = nil
	}
	cur = newSectionAccum(preambleSection, false)

	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		switch {
		case strings.HasPrefix(trimmed, "# ") && title == "":
			title = strings.TrimSpace(trimmed[2:])
			continue
		case strings.HasPrefix(trimmed, "## "):
			flush()
			cur = newSectionAccum(strings.TrimSpace(trimmed[3:]), true)
			cur.raw = append(cur.raw, trimmed)
			continue
		}
		cur.addLine(trimmed)
	}
	flush()

	// Duplicate `## ` titles would collide in the block map; suffix repeats so
	// every section keeps its own blocks.
	seen := map[string]int{}
	for i := range sections {
		n := seen[sections[i].Title]
		seen[sections[i].Title] = n + 1
		if n > 0 {
			sections[i].Title = sections[i].Title + " #" + strconv.Itoa(n+1)
		}
	}
	return title, sections
}

// docSectionAccum builds one section line by line.
type docSectionAccum struct {
	title   string
	heading bool // emit a heading_2 block for the title
	raw     []string
	blocks  []NotionBlock
	para    []string // pending paragraph lines
	hasBody bool
}

func newSectionAccum(title string, heading bool) *docSectionAccum {
	return &docSectionAccum{title: title, heading: heading}
}

func (a *docSectionAccum) flushPara() {
	if len(a.para) == 0 {
		return
	}
	text := strings.Join(a.para, "\n")
	a.para = nil
	a.blocks = append(a.blocks, NotionBlock{Type: "paragraph", Rich: parseInline(text)})
	a.hasBody = true
}

func (a *docSectionAccum) addLine(line string) {
	a.raw = append(a.raw, line)
	trimmed := strings.TrimSpace(line)
	switch {
	case trimmed == "":
		a.flushPara()
	case strings.HasPrefix(trimmed, "### "):
		a.flushPara()
		a.blocks = append(a.blocks, NotionBlock{Type: "heading_3", Rich: parseInline(strings.TrimSpace(trimmed[4:]))})
		a.hasBody = true
	case strings.HasPrefix(trimmed, "# "):
		// A later `# ` (the first one became the page title) renders as its own
		// top-level heading block.
		a.flushPara()
		a.blocks = append(a.blocks, NotionBlock{Type: "heading_1", Rich: parseInline(strings.TrimSpace(trimmed[2:]))})
		a.hasBody = true
	case strings.HasPrefix(trimmed, "▫️ "), strings.HasPrefix(trimmed, "- "), strings.HasPrefix(trimmed, "* "):
		a.flushPara()
		item := trimmed
		for _, p := range []string{"▫️ ", "- ", "* "} {
			if strings.HasPrefix(item, p) {
				item = strings.TrimSpace(item[len(p):])
				break
			}
		}
		a.blocks = append(a.blocks, NotionBlock{Type: "bulleted_list_item", Rich: parseInline(item)})
		a.hasBody = true
	default:
		a.para = append(a.para, trimmed)
	}
}

// finish closes the section. A preamble with no content reports ok=false so
// empty docs do not mint an empty phantom section.
func (a *docSectionAccum) finish() (DocSection, bool) {
	a.flushPara()
	blocks := a.blocks
	if a.heading {
		head := NotionBlock{Type: "heading_2", Rich: parseInline(a.title)}
		blocks = append([]NotionBlock{head}, blocks...)
	} else if !a.hasBody {
		return DocSection{}, false
	}
	return DocSection{Title: a.title, Hash: sectionHash(a.raw), Blocks: blocks}, true
}

// parseInline converts one text run into rich text: `**bold**` and
// `[text](url)` only, kept deliberately minimal (plan C2). Malformed markers
// fall through as plain text.
func parseInline(s string) []NotionRichText {
	var out []NotionRichText
	plain := strings.Builder{}
	emitPlain := func() {
		if plain.Len() > 0 {
			out = append(out, NotionRichText{Text: plain.String()})
			plain.Reset()
		}
	}

	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "**"):
			end := strings.Index(s[i+2:], "**")
			if end < 0 || end == 0 {
				plain.WriteString("**")
				i += 2
				continue
			}
			emitPlain()
			out = append(out, NotionRichText{Text: s[i+2 : i+2+end], Bold: true})
			i += 2 + end + 2
		case s[i] == '[':
			text, url, n := parseLink(s[i:])
			if n == 0 {
				plain.WriteByte('[')
				i++
				continue
			}
			emitPlain()
			out = append(out, NotionRichText{Text: text, Link: url})
			i += n
		default:
			plain.WriteByte(s[i])
			i++
		}
	}
	emitPlain()
	if out == nil {
		out = []NotionRichText{{Text: ""}}
	}
	return out
}

// parseLink matches a leading `[text](url)`, returning the consumed length
// (0 = no match).
func parseLink(s string) (text, url string, n int) {
	close := strings.Index(s, "](")
	if close < 0 {
		return "", "", 0
	}
	end := strings.Index(s[close+2:], ")")
	if end < 0 {
		return "", "", 0
	}
	text = s[1:close]
	url = s[close+2 : close+2+end]
	if strings.ContainsAny(text, "[\n") || url == "" || strings.ContainsAny(url, " \n") {
		return "", "", 0
	}
	return text, url, close + 2 + end + 1
}
