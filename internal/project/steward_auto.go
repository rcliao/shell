package project

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// EventStewardAuto runs one auto steward proposal (docs/DESIGN-STEWARD-AUTO.md):
// a low-risk step the steward filed to do without asking. The daemon queues
// it right after the deep beat that filed it; the consumer runs it as one
// turn in the project's thread and posts the result.
const EventStewardAuto = "steward.auto"

// stewardAutoTTL drops an auto step that could not run within a day: the
// beat that chose it saw a world that has moved on.
const stewardAutoTTL = 24 * time.Hour

// EnqueueStewardAuto queues the turn that runs auto proposal id for p, once.
func EnqueueStewardAuto(q RenderQueue, p store.Project, id int64, now time.Time) (bool, error) {
	payload, err := json.Marshal(EventPayload{Event: EventStewardAuto, Slug: p.Slug, ChatID: p.ChatID, ThreadID: p.MessageThreadID, SuggestionID: id})
	if err != nil {
		return false, err
	}
	exp := now.UTC().Add(stewardAutoTTL)
	_, created, err := q.EnqueueTask(store.Task{
		Kind:           EventKind,
		Source:         store.TaskSourceAgent,
		IdempotencyKey: store.DeriveIdempotencyKey(EventKind, EventStewardAuto, strconv.FormatInt(id, 10)),
		PartitionKey:   fmt.Sprintf("chat:%d:%d", p.ChatID, p.MessageThreadID), // scheduler.PartitionKey
		Payload:        string(payload),
		ExpiresAt:      &exp,
	})
	return created, err
}

// StewardAutoPrompt is the turn that does an auto proposal and reports it.
// The rules are repeated here because this turn, not the beat, has the
// tools: a step that turns out to break one is dropped, not done.
func StewardAutoPrompt(id int64, p store.Project, change, evidence string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Steward auto #%d — project %s (%s)]\n", id, p.Slug, p.Title)
	b.WriteString("In your last deep beat you chose this as a step safe to do without asking the family:\n")
	fmt.Fprintf(&b, "Task: %s\n", strings.TrimSpace(change))
	if e := strings.TrimSpace(evidence); e != "" {
		fmt.Fprintf(&b, "Your notes: %s\n", e)
	}
	b.WriteString(`
Do it now, in this one turn. Stay inside the auto rules: buy nothing and start no paid service; send, email, post or submit nothing to anyone and contact no business or person (your reply here is the only message); book, cancel, delete or edit nothing the family already has (a new draft doc or sheet is fine); create no schedule. If on a closer look the task breaks a rule, or is no longer needed, do not do it and reply [noop].
Then reply here, to the family: what you did and what they get (the numbers, a link to any draft), in at most 4 short sentences, then the one next step you could take, as a yes/no question. Do not apologize for not asking first.
`)
	if p.Lang != "" {
		fmt.Fprintf(&b, "Reply in the project's language: %s.\n", p.Lang)
	}
	return b.String()
}
