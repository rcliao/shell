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

// Wave D events (docs/PLAN-PROJECT-WORKSPACE.md "Feedback via Notion").
// ONE poller produces all inbound Notion signals as normalized events; a
// future webhook producer emits the same shapes with zero consumer changes.
const (
	// EventNotionPoll is the shared poll tick (global event schedule).
	EventNotionPoll = "notion.poll"
	// EventNotionCommentCreated is one new (or newly human-answered) comment
	// discussion on a project's page; payload carries discussion + comment.
	EventNotionCommentCreated = "notion.comment.created"
	// EventNotionPageEdited is a direct human edit of the mirrored page that
	// our own renders do not explain; the consumer reconciles into canonical.
	EventNotionPageEdited = "notion.page.edited"
)

// NotionPollDedupKey is the explicit dedup key of the ONE global Notion poll
// schedule, registered idempotently at daemon startup.
const NotionPollDedupKey = "project:notion-poll"

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

	// Comment-event fields (EventNotionCommentCreated).
	DiscussionID string `json:"discussion_id,omitempty"`
	CommentPlain string `json:"comment_plain,omitempty"`
	// AnchorBlock is the block the discussion hangs off; empty for
	// page-level comments.
	AnchorBlock string `json:"anchor_block,omitempty"`
}

// ResearchScheduleMessage builds the event-mode schedule message (the
// envelope scheduler.ParseEventMessage expects) for a project's research
// schedule.
func ResearchScheduleMessage(slug string, chatID, threadID int64) (string, error) {
	return eventEnvelope(EventPayload{
		Event: EventResearchDue, Slug: slug, ChatID: chatID, ThreadID: threadID,
	})
}

// NotionPollScheduleMessage builds the envelope for the ONE global Notion
// poll schedule. No slug and no chat: the poll tick iterates every active
// notion-exported project itself.
func NotionPollScheduleMessage() (string, error) {
	return eventEnvelope(EventPayload{Event: EventNotionPoll})
}

// eventEnvelope wraps a payload in the event-mode schedule message shape.
func eventEnvelope(p EventPayload) (string, error) {
	payload, err := json.Marshal(p)
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

// DecodeEventPayload parses a project.event task payload. Slug is required
// for every event except the global poll tick, which fans out over all
// projects itself.
func DecodeEventPayload(payload string) (EventPayload, error) {
	var p EventPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return p, fmt.Errorf("undecodable project.event payload: %w", err)
	}
	if p.Event == "" {
		return p, fmt.Errorf("project.event payload needs event")
	}
	if p.Slug == "" && p.Event != EventNotionPoll {
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

// CommentRevisionPrompt renders the bounded revision-pass prompt for ONE
// comment thread on the project's Notion page (Wave D). The hard contract is
// the point: one fix pass per thread, doc changes only through doc-write with
// its receipt, and a short reply that will be posted INTO the Notion thread —
// never into the chat.
func CommentRevisionPrompt(slug, title, instructions, lang, doc, comment, section string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Project comment: %s]\n", slug)
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
	b.WriteString("\nThe user commented on the project's Notion page")
	if section != "" {
		fmt.Fprintf(&b, ", on the %q section", section)
	}
	b.WriteString(":\n---\n")
	b.WriteString(comment)
	if !strings.HasSuffix(comment, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("---\n")
	b.WriteString("\nDo ONE bounded revision pass for this comment now. Hard rules:\n")
	b.WriteString("- Apply exactly what the comment asks — one fix pass, nothing more.\n")
	b.WriteString("- Doc changes go through the project skill's doc-write (full revised content); the printed commit rev is your receipt — no rev, no claim.\n")
	b.WriteString("- If the comment is a question rather than a change request, answer it in your reply and touch the doc only if needed.\n")
	b.WriteString("- Your reply is posted INTO the Notion comment thread: at most 2 short lines on what changed (or the answer). Never re-dump the doc.\n")
	b.WriteString("- Do NOT send any Telegram message or relay for this — the conversation happens in Notion.\n")
	if lang != "" {
		fmt.Fprintf(&b, "- Reply in the project's language: %s.\n", lang)
	}
	return b.String()
}
