package discord

import (
	"strings"
	"testing"
)

const listingCard = "```card\n" + `{"title": "2BR near the park", "url": "https://example.com/listing/1",
 "description": "Quiet street, walk to school", "image": "https://example.com/p.jpg",
 "fields": [{"name": "Rent", "value": "$3,200", "inline": true}, {"name": "", "value": "dropped"}],
 "footer": "seen today"}` + "\n```"

func TestExtractCards(t *testing.T) {
	rest, embeds := extractCards("Found one:\n\n" + listingCard + "\nWant me to ask about parking?")
	if len(embeds) != 1 {
		t.Fatalf("want 1 card, got %d", len(embeds))
	}
	e := embeds[0]
	if e.Title != "2BR near the park" || e.URL != "https://example.com/listing/1" || e.Image == nil || e.Footer == nil {
		t.Fatalf("card lost fields: %+v", e)
	}
	if len(e.Fields) != 1 || e.Fields[0].Name != "Rent" || !e.Fields[0].Inline {
		t.Fatalf("fields = %+v (an empty-named field must be dropped)", e.Fields)
	}
	if strings.Contains(rest, "```card") || !strings.Contains(rest, "Found one:") || !strings.Contains(rest, "parking?") {
		t.Fatalf("rest = %q", rest)
	}
}

func TestExtractCardsKeepsWhatDoesNotParse(t *testing.T) {
	in := "```card\n{not json\n```"
	rest, embeds := extractCards(in)
	if len(embeds) != 0 || rest != in {
		t.Fatalf("a broken card must stay as written: rest=%q embeds=%d", rest, len(embeds))
	}
	rest, embeds = extractCards("```card\n{\"url\": \"javascript:alert(1)\"}\n```")
	if len(embeds) != 0 || !strings.Contains(rest, "javascript") {
		t.Fatal("a card with nothing showable (only a non-web link) stays as text")
	}
	_, embeds = extractCards("```card\n{\"title\": \"x\", \"image\": \"file:///etc/passwd\"}\n```")
	if len(embeds) != 1 || embeds[0].Image != nil {
		t.Fatal("non-http image links must be dropped, the card kept")
	}
}

func TestHideCardsWhileStreaming(t *testing.T) {
	if got := hideCards("Here:\n```card\n{\"title\": \"par"); got != "Here:\n🗂️ …" {
		t.Fatalf("half-typed card shows %q", got)
	}
	if got := hideCards("Here:\n" + listingCard + "\nmore"); !strings.Contains(got, "🗂️ …") || strings.Contains(got, "{") {
		t.Fatalf("finished card shows %q", got)
	}
}
