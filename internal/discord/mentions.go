package discord

import (
	"regexp"
	"sort"
	"strings"
)

// Mentions turns "@name" in the agent's text into a Discord mention that
// notifies that person. The names are the family's canonical ids (the same
// ones memory uses), so the agent writes "@alex" and Discord shows and pings
// the person. Every other mention stays inert: the agent's text never pings
// anyone it did not name this way.
type Mentions struct {
	ids map[string]string // lowercased name → discord user id
	re  *regexp.Regexp
}

// NewMentions builds the converter from name → Discord user id. Empty names
// and ids are skipped; nil when there is nothing to convert.
func NewMentions(byName map[string]string) *Mentions {
	ids := map[string]string{}
	var names []string
	for n, id := range byName {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || id == "" {
			continue
		}
		ids[n] = id
		names = append(names, regexp.QuoteMeta(n))
	}
	if len(names) == 0 {
		return nil
	}
	// Longest first, so "@alexko" (if it existed) is not read as "@alex".
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	re := regexp.MustCompile(`(?i)(^|[^\w@])@(` + strings.Join(names, "|") + `)\b`)
	return &Mentions{ids: ids, re: re}
}

// Names lists the names the agent can mention, sorted, for the prompt.
func (m *Mentions) Names() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.ids))
	for n := range m.ids {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Render replaces "@name" with "<@id>" and returns the ids it mentioned, the
// only users the message is allowed to ping. Text inside code blocks and
// inline code is left alone.
func (m *Mentions) Render(text string) (string, []string) {
	if m == nil || !strings.Contains(text, "@") {
		return text, nil
	}
	seen := map[string]bool{}
	var users []string
	var out strings.Builder
	for i, part := range splitCode(text) {
		if i%2 == 1 { // code: untouched
			out.WriteString(part)
			continue
		}
		out.WriteString(m.re.ReplaceAllStringFunc(part, func(match string) string {
			sub := m.re.FindStringSubmatch(match)
			id := m.ids[strings.ToLower(sub[2])]
			if !seen[id] {
				seen[id] = true
				users = append(users, id)
			}
			return sub[1] + "<@" + id + ">"
		}))
	}
	return out.String(), users
}

// splitCode splits text into alternating prose and code segments (even
// indexes prose, odd code), on ``` fences and `inline` spans.
func splitCode(text string) []string {
	var parts []string
	var cur strings.Builder
	inCode := false
	delim := ""
	for i := 0; i < len(text); {
		if !inCode {
			if strings.HasPrefix(text[i:], "```") {
				parts = append(parts, cur.String())
				cur.Reset()
				inCode, delim = true, "```"
				cur.WriteString(delim)
				i += 3
				continue
			}
			if text[i] == '`' {
				parts = append(parts, cur.String())
				cur.Reset()
				inCode, delim = true, "`"
				cur.WriteByte('`')
				i++
				continue
			}
		} else if strings.HasPrefix(text[i:], delim) {
			cur.WriteString(delim)
			i += len(delim)
			parts = append(parts, cur.String())
			cur.Reset()
			inCode = false
			continue
		}
		cur.WriteByte(text[i])
		i++
	}
	// The tail is prose, or code left unclosed; either way it lands at the
	// right parity because every open appended the prose before it.
	return append(parts, cur.String())
}

// PlatformNote is the environment-prompt line telling the agent how Discord
// renders what it writes. names are the people it can notify.
func PlatformNote(names []string, answerButtons bool) string {
	var sb strings.Builder
	sb.WriteString("- **Where the family reads you:** Discord. It renders Markdown (headings, bold, lists, links, code blocks; tables appear as a code block). ")
	if len(names) > 0 {
		sb.WriteString("To notify a specific person — a reminder or message meant for them — write ")
		for i, n := range names {
			if i > 0 {
				sb.WriteString(" or ")
			}
			sb.WriteString("`@" + n + "`")
		}
		sb.WriteString("; it becomes a real mention that pings their phone. Use it in reminders and scheduled messages for someone; don't mention people otherwise. ")
	}
	sb.WriteString("To show a place, listing, restaurant or project as a card (title link, picture, a few facts), add a fenced block with language `card` holding JSON: `{\"title\", \"url\", \"description\", \"image\", \"thumbnail\", \"fields\": [{\"name\", \"value\", \"inline\"}], \"footer\"}` (links must be http/https; up to 10 cards per reply) — it appears as a rich card under your text. Use cards for things worth scanning, not for ordinary answers. ")
	if answerButtons {
		sb.WriteString("When you ask a question with two to five clear options, you may end the reply with a fenced block with language `choices` holding a JSON list of short labels, e.g. `[\"Tacos\", \"Ramen\"]`; they appear as buttons, and a tap comes back to you as that person's reply to your question. Only for real choices, never as decoration. ")
	}
	sb.WriteString("For a specific moment, you may write `<t:UNIX:f>` (date and time) or `<t:UNIX:R>` (relative, \"in 2 hours\"), where UNIX is seconds since the epoch computed from the current time you are given each turn; Discord shows it in each reader's own time zone.\n")
	return sb.String()
}
