package project

import (
	"strings"
	"testing"
)

const sampleDoc = `# Demo Trip

Intro line.

## 目標

- see the castle
▫️ eat well

## 選項

### Hotel A

**Cheap** and near [the station](https://example.com/sta).

Second paragraph.

## 更新紀錄
`

func TestParseDocSectionsAndTitle(t *testing.T) {
	title, secs := ParseDoc(sampleDoc)
	if title != "Demo Trip" {
		t.Errorf("title = %q", title)
	}
	want := []string{preambleSection, "目標", "選項", "更新紀錄"}
	if len(secs) != len(want) {
		t.Fatalf("got %d sections (%v), want %d", len(secs), sectionTitles(secs), len(want))
	}
	for i, w := range want {
		if secs[i].Title != w {
			t.Errorf("section %d = %q, want %q", i, secs[i].Title, w)
		}
		if secs[i].Hash == "" {
			t.Errorf("section %q has no hash", secs[i].Title)
		}
	}

	// Preamble: paragraph only, no heading block.
	if secs[0].Blocks[0].Type != "paragraph" || secs[0].Blocks[0].Rich[0].Text != "Intro line." {
		t.Errorf("preamble blocks = %+v", secs[0].Blocks)
	}

	// 目標: heading_2 + two bullets (both `- ` and `▫️ ` forms).
	goals := secs[1].Blocks
	if goals[0].Type != "heading_2" || goals[0].Rich[0].Text != "目標" {
		t.Errorf("goal heading = %+v", goals[0])
	}
	if len(goals) != 3 || goals[1].Type != "bulleted_list_item" || goals[2].Type != "bulleted_list_item" {
		t.Fatalf("goal blocks = %+v", goals)
	}
	if goals[2].Rich[0].Text != "eat well" {
		t.Errorf("▫️ bullet text = %q", goals[2].Rich[0].Text)
	}

	// 選項: heading_2, heading_3, then TWO paragraphs (blank-line split) —
	// fine-grained blocks are the Wave D comment anchors.
	opts := secs[2].Blocks
	types := []string{}
	for _, b := range opts {
		types = append(types, b.Type)
	}
	if strings.Join(types, ",") != "heading_2,heading_3,paragraph,paragraph" {
		t.Fatalf("option block types = %v", types)
	}

	// Inline: bold + link runs.
	rich := opts[2].Rich
	if len(rich) < 4 {
		t.Fatalf("rich runs = %+v", rich)
	}
	if !rich[0].Bold || rich[0].Text != "Cheap" {
		t.Errorf("bold run = %+v", rich[0])
	}
	foundLink := false
	for _, r := range rich {
		if r.Link == "https://example.com/sta" && r.Text == "the station" {
			foundLink = true
		}
	}
	if !foundLink {
		t.Errorf("no link run in %+v", rich)
	}

	// Empty trailing section still gets its heading block.
	if len(secs[3].Blocks) != 1 || secs[3].Blocks[0].Type != "heading_2" {
		t.Errorf("empty section blocks = %+v", secs[3].Blocks)
	}
}

func TestParseDocHashChangesOnlyForEditedSection(t *testing.T) {
	_, before := ParseDoc(sampleDoc)
	edited := strings.Replace(sampleDoc, "see the castle", "see the museum", 1)
	_, after := ParseDoc(edited)

	for i := range before {
		same := before[i].Hash == after[i].Hash
		if before[i].Title == "目標" && same {
			t.Errorf("edited section %q kept its hash", before[i].Title)
		}
		if before[i].Title != "目標" && !same {
			t.Errorf("untouched section %q changed hash", before[i].Title)
		}
	}
}

func TestParseDocNoPreambleAndDuplicateTitles(t *testing.T) {
	_, secs := ParseDoc("# T\n\n## A\n\nx\n\n## A\n\ny\n")
	if len(secs) != 2 {
		t.Fatalf("sections = %v", sectionTitles(secs))
	}
	if secs[0].Title != "A" || secs[1].Title != "A #2" {
		t.Errorf("duplicate titles = %v", sectionTitles(secs))
	}
}

func TestParseInlineMalformedFallsThroughPlain(t *testing.T) {
	runs := parseInline("**bold** then [not a link] and **unclosed")
	joined := ""
	for _, r := range runs {
		joined += r.Text
	}
	if joined != "bold then [not a link] and **unclosed" {
		t.Errorf("joined = %q", joined)
	}
	if !runs[0].Bold || runs[0].Text != "bold" {
		t.Errorf("first run = %+v", runs[0])
	}
	for _, r := range runs[1:] {
		if r.Bold || r.Link != "" {
			t.Errorf("malformed marker produced styled run %+v", r)
		}
	}
}

func sectionTitles(secs []DocSection) []string {
	out := make([]string, 0, len(secs))
	for _, s := range secs {
		out = append(out, s.Title)
	}
	return out
}
