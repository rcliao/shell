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
