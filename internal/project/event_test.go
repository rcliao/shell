package project

import (
	"strings"
	"testing"

	"github.com/rcliao/shell/internal/scheduler"
)

func TestResearchScheduleMessageRoundTrip(t *testing.T) {
	msg, err := ResearchScheduleMessage("housing-search", -100200300, 7)
	if err != nil {
		t.Fatal(err)
	}

	// The envelope must be exactly what the scheduler's event mode accepts.
	kind, payload, err := scheduler.ParseEventMessage(msg)
	if err != nil {
		t.Fatalf("envelope rejected by scheduler: %v", err)
	}
	if kind != EventKind {
		t.Errorf("kind = %q, want %q", kind, EventKind)
	}

	p, err := DecodeEventPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if p.Event != EventResearchDue || p.Slug != "housing-search" {
		t.Errorf("payload = %+v", p)
	}
	if p.ChatID != -100200300 || p.ThreadID != 7 {
		t.Errorf("chat binding = (%d, %d), want (-100200300, 7)", p.ChatID, p.ThreadID)
	}
}

func TestDecodeEventPayloadRejectsGarbage(t *testing.T) {
	if _, err := DecodeEventPayload("not json"); err == nil {
		t.Error("expected error for non-JSON payload")
	}
	if _, err := DecodeEventPayload(`{"event":"research.due"}`); err == nil {
		t.Error("expected error for missing slug")
	}
	if _, err := DecodeEventPayload(`{"slug":"x"}`); err == nil {
		t.Error("expected error for missing event")
	}
}

func TestResearchPromptCarriesContract(t *testing.T) {
	prompt := ResearchPrompt("housing", "Housing Search", "sort by price", "zh", "# doc body")

	for _, want := range []string{
		"Housing Search",
		"sort by price",
		"# doc body",
		"ONE bounded research pass",
		"doc-write",
		"at most 3 short lines",
		"[noop]",
		"zh",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestScheduleDedupKey(t *testing.T) {
	if got := ScheduleDedupKey("housing"); got != "project:housing" {
		t.Errorf("dedup key = %q", got)
	}
}

func TestNotionPollScheduleMessageRoundTrip(t *testing.T) {
	msg, err := NotionPollScheduleMessage()
	if err != nil {
		t.Fatal(err)
	}
	kind, payload, err := scheduler.ParseEventMessage(msg)
	if err != nil {
		t.Fatalf("envelope rejected by scheduler: %v", err)
	}
	if kind != EventKind {
		t.Errorf("kind = %q, want %q", kind, EventKind)
	}
	// The poll tick has no slug — DecodeEventPayload must accept that for
	// this ONE event and reject it for everything else.
	p, err := DecodeEventPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if p.Event != EventNotionPoll || p.Slug != "" {
		t.Errorf("payload = %+v", p)
	}
	if _, err := DecodeEventPayload(`{"event":"notion.comment.created"}`); err == nil {
		t.Error("slug-less non-poll event must be rejected")
	}
}

func TestCommentRevisionPromptCarriesContract(t *testing.T) {
	prompt := CommentRevisionPrompt("demo", "Demo Project", "keep prices sorted", "zh",
		"# Demo\n\n## 選項\n\n- option A\n", "can you add option B?", "選項")

	for _, want := range []string{
		"Demo Project",
		"keep prices sorted",
		"can you add option B?",
		"\"選項\" section",
		"ONE bounded revision pass",
		"doc-write",
		"INTO the Notion comment thread",
		"Do NOT send any Telegram message",
		"zh",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}

	// Page-level comments carry no section context.
	pageLevel := CommentRevisionPrompt("demo", "Demo", "", "", "", "hi", "")
	if strings.Contains(pageLevel, "section") {
		t.Error("page-level prompt must not name a section")
	}
}
