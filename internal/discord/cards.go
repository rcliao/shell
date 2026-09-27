package discord

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// Cards (B10). The agent can show a place, a listing or a project as a card
// by writing a fenced block:
//
//	```card
//	{"title": "...", "url": "https://...", "description": "...",
//	 "image": "https://...", "thumbnail": "https://...",
//	 "fields": [{"name": "Price", "value": "$3,200", "inline": true}],
//	 "footer": "..."}
//	```
//
// On Discord the block is lifted out of the text and sent as an embed under
// the reply. JSON rather than a bespoke syntax because the model writes it
// reliably and it parses without guessing. A block that does not parse is
// left in the text as written, so nothing the agent said is lost.

// cardBlockRe matches a complete ```card block.
var cardBlockRe = regexp.MustCompile("(?s)```card[ \t]*\n(.*?)\n?```[ \t]*\n?")

// maxEmbedsPerMessage is Discord's limit.
const maxEmbedsPerMessage = 10

type card struct {
	Title       string      `json:"title"`
	URL         string      `json:"url"`
	Description string      `json:"description"`
	Image       string      `json:"image"`
	Thumbnail   string      `json:"thumbnail"`
	Footer      string      `json:"footer"`
	Fields      []cardField `json:"fields"`
}

type cardField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

// extractCards lifts card blocks out of text and returns the remaining text
// and the embeds, in order.
func extractCards(text string) (string, []*discordgo.MessageEmbed) {
	if !strings.Contains(text, "```card") {
		return text, nil
	}
	var embeds []*discordgo.MessageEmbed
	rest := cardBlockRe.ReplaceAllStringFunc(text, func(block string) string {
		body := cardBlockRe.FindStringSubmatch(block)[1]
		var c card
		if err := json.Unmarshal([]byte(body), &c); err != nil {
			return block // not a card after all: keep what the agent wrote
		}
		e := c.embed()
		if e == nil {
			return block
		}
		embeds = append(embeds, e)
		return ""
	})
	return strings.TrimSpace(rest), embeds
}

// embed converts a card within Discord's limits; nil when the card has
// nothing to show.
func (c card) embed() *discordgo.MessageEmbed {
	e := &discordgo.MessageEmbed{
		Title:       truncateRunes(c.Title, 256),
		URL:         webURL(c.URL),
		Description: truncateRunes(c.Description, 4096),
	}
	if u := webURL(c.Image); u != "" {
		e.Image = &discordgo.MessageEmbedImage{URL: u}
	}
	if u := webURL(c.Thumbnail); u != "" {
		e.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: u}
	}
	if c.Footer != "" {
		e.Footer = &discordgo.MessageEmbedFooter{Text: truncateRunes(c.Footer, 2048)}
	}
	for _, f := range c.Fields {
		if len(e.Fields) == 25 {
			break
		}
		if strings.TrimSpace(f.Name) == "" || strings.TrimSpace(f.Value) == "" {
			continue // Discord rejects empty field names and values
		}
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name: truncateRunes(f.Name, 256), Value: truncateRunes(f.Value, 1024), Inline: f.Inline,
		})
	}
	if e.Title == "" && e.Description == "" && len(e.Fields) == 0 && e.Image == nil {
		return nil
	}
	return e
}

// webURL keeps only http(s) links; Discord rejects anything else and would
// fail the whole message.
func webURL(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.String()
}

// hideCards is what streaming shows while a card is being written: the text
// with each card block — including one still being typed — replaced by a
// marker, instead of raw JSON flickering on screen.
func hideCards(text string) string {
	if !strings.Contains(text, "```card") {
		return text
	}
	text = cardBlockRe.ReplaceAllString(text, "🗂️ …\n")
	if i := strings.LastIndex(text, "```card"); i >= 0 {
		text = text[:i] + "🗂️ …"
	}
	return text
}

// embedBatches splits embeds into per-message groups of Discord's maximum.
func embedBatches(embeds []*discordgo.MessageEmbed) [][]*discordgo.MessageEmbed {
	var out [][]*discordgo.MessageEmbed
	for len(embeds) > maxEmbedsPerMessage {
		out = append(out, embeds[:maxEmbedsPerMessage])
		embeds = embeds[maxEmbedsPerMessage:]
	}
	if len(embeds) > 0 {
		out = append(out, embeds)
	}
	return out
}
