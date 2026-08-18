package project

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The project.event contract (P2, docs/PLAN-PROJECT-WORKSPACE.md "Autonomous
// research"): a project's research schedule is an event-mode schedule whose
// envelope enqueues a task of this kind; the daemon's consumer owns everything
// downstream, including chat delivery. The shapes live HERE so the producer
// (rpc project create) and the consumer (daemon) agree by construction.

// EventKind is the task-queue kind for project events.
const EventKind = "project.event"

// EventResearchDue is the event emitted by a project's research schedule.
const EventResearchDue = "research.due"

// ScheduleDedupKey is the explicit schedules.dedup_key for a project's
// research schedule: "project:<slug>". Archive/pause finds and disables the
// schedule by this key; re-activation re-enables it.
func ScheduleDedupKey(slug string) string {
	return "project:" + slug
}

// EventPayload is the payload carried inside the event envelope. ChatID and
// ThreadID ride along so the scheduler's eventPartitionKey serializes the
// consumer turn against the project's live conversation.
type EventPayload struct {
	Event    string `json:"event"`
	Slug     string `json:"slug"`
	ChatID   int64  `json:"chat_id,omitempty"`
	ThreadID int64  `json:"message_thread_id,omitempty"`
}

// ResearchScheduleMessage builds the event-mode schedule message (the
// envelope scheduler.ParseEventMessage expects) for a project's research
// schedule.
func ResearchScheduleMessage(slug string, chatID, threadID int64) (string, error) {
	payload, err := json.Marshal(EventPayload{
		Event: EventResearchDue, Slug: slug, ChatID: chatID, ThreadID: threadID,
	})
	if err != nil {
		return "", err
	}
	env, err := json.Marshal(struct {
		Kind    string          `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	}{Kind: EventKind, Payload: payload})
	if err != nil {
		return "", err
	}
	return string(env), nil
}

// DecodeEventPayload parses a project.event task payload.
func DecodeEventPayload(payload string) (EventPayload, error) {
	var p EventPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return p, fmt.Errorf("undecodable project.event payload: %w", err)
	}
	if p.Event == "" || p.Slug == "" {
		return p, fmt.Errorf("project.event payload needs event and slug")
	}
	return p, nil
}

// ResearchPrompt renders the bounded research-pass prompt for one project.
// The contract lines are the point: one pass, doc updates only through the
// project skill's doc-write (which prints the commit-hash receipt), and a
// short delta reply — never a re-dump of the doc.
func ResearchPrompt(slug, title, instructions, lang, doc string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Project research: %s]\n", slug)
	fmt.Fprintf(&b, "Project: %s\n", title)
	if instructions != "" {
		fmt.Fprintf(&b, "Standing instructions: %s\n", instructions)
	}
	if doc != "" {
		b.WriteString("\nCurrent doc:\n---\n")
		b.WriteString(doc)
		if !strings.HasSuffix(doc, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("---\n")
	}
	b.WriteString("\nDo ONE bounded research pass for this project now. Hard rules:\n")
	b.WriteString("- Update the doc via the project skill's doc-write (full revised content); the printed commit rev is your receipt — no rev, no claim.\n")
	b.WriteString("- Reply with the DELTA only: at most 3 short lines on what changed or was found. Never re-dump the doc.\n")
	b.WriteString("- Text only — no images, files, or generated media.\n")
	b.WriteString("- If nothing new was found, reply [noop].\n")
	if lang != "" {
		fmt.Fprintf(&b, "- Reply in the project's language: %s.\n", lang)
	}
	return b.String()
}
